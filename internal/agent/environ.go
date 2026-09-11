package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvReader returns the app container's environment and the task ARN.
type EnvReader interface {
	Read(ctx context.Context) (env map[string]string, taskARN string, err error)
}

// ProcEnvReader implements EnvReader the way mirrord does: ask the ECS
// metadata endpoint which container is the app, then read the environ of
// that container's oldest process from /proc (spec §5.3). Needs
// pidMode: task and CAP_SYS_PTRACE.
type ProcEnvReader struct {
	MetadataURL  string // ECS_CONTAINER_METADATA_URI_V4 of the agent container
	ProcRoot     string // "/proc"
	AppContainer string // TETHERD_APP_CONTAINER
	HTTP         *http.Client
}

type taskMetadata struct {
	TaskARN    string `json:"TaskARN"`
	Containers []struct {
		DockerID string `json:"DockerId"`
		Name     string `json:"Name"`
	} `json:"Containers"`
}

// Read implements EnvReader.
func (r *ProcEnvReader) Read(ctx context.Context) (map[string]string, string, error) {
	if r.MetadataURL == "" {
		return nil, "", errors.New("no ECS metadata endpoint (agent is not running in ECS)")
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.MetadataURL, "/")+"/task", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("task metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("task metadata: HTTP %d", resp.StatusCode)
	}
	var md taskMetadata
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return nil, "", fmt.Errorf("task metadata: %w", err)
	}
	var dockerID string
	var names []string
	for _, c := range md.Containers {
		names = append(names, c.Name)
		if c.Name == r.AppContainer {
			dockerID = c.DockerID
		}
	}
	if dockerID == "" {
		return nil, "", fmt.Errorf("container %q is not in the task (containers: %s); set TETHERD_APP_CONTAINER", r.AppContainer, strings.Join(names, ", "))
	}

	// Find processes whose own metadata URI points at the app container.
	entries, err := os.ReadDir(r.ProcRoot)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", r.ProcRoot, err)
	}
	var bestEnv map[string]string
	var bestStart uint64
	found := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.ProcRoot, e.Name(), "environ"))
		if err != nil || len(raw) == 0 {
			continue
		}
		env := ParseEnviron(raw)
		if !strings.Contains(env["ECS_CONTAINER_METADATA_URI_V4"], dockerID) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(r.ProcRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		start, err := procStartTime(string(stat))
		if err != nil {
			continue
		}
		if !found || start < bestStart {
			bestEnv, bestStart, found = env, start, true
		}
		_ = pid
	}
	if !found {
		return nil, "", fmt.Errorf("no process of container %q is visible from the agent; is pidMode \"task\" set on the task definition (and SYS_PTRACE added to the agent)?", r.AppContainer)
	}
	return bestEnv, md.TaskARN, nil
}

// ParseEnviron splits the NUL-separated KEY=VALUE list of /proc/<pid>/environ.
func ParseEnviron(b []byte) map[string]string {
	out := map[string]string{}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if len(kv) == 0 {
			continue
		}
		k, v, ok := strings.Cut(string(kv), "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// procStartTime returns field 22 (starttime, clock ticks since boot) of a
// /proc/<pid>/stat line. The comm field is parenthesised and may contain
// spaces, so parse from the last ')'.
func procStartTime(statLine string) (uint64, error) {
	i := strings.LastIndex(statLine, ")")
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(statLine[i+1:])
	// fields[0] is state (field 3); starttime is field 22 → index 19.
	if len(fields) < 20 {
		return 0, errors.New("short stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
