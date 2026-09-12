package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnviron(t *testing.T) {
	got := ParseEnviron([]byte("A=1\x00B=x=y\x00\x00NOEQ\x00"))
	if got["A"] != "1" || got["B"] != "x=y" || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestProcStartTime(t *testing.T) {
	// pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice num_threads itrealvalue starttime ...
	line := "42 (my app) S 1 42 42 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 1 0 12345 1000 200 18446744073709551615"
	st, err := procStartTime(line)
	if err != nil || st != 12345 {
		t.Fatalf("starttime = %d, err = %v", st, err)
	}
}

// writeProc lays out a fake /proc with the given processes.
func writeProc(t *testing.T, procs map[int]struct {
	env       string
	starttime string
}) string {
	t.Helper()
	root := t.TempDir()
	for pid, p := range procs {
		dir := filepath.Join(root, itoa(pid))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "environ"), []byte(p.env), 0o644)
		os.WriteFile(filepath.Join(dir, "stat"), []byte(itoa(pid)+" (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 "+p.starttime+" 0 0 0\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "self"), 0o755) // non-numeric entries must be skipped
	return root
}

func metadata(t *testing.T, containers string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/abc/task" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"TaskARN":"arn:aws:ecs:ap-northeast-1:1:task/c/t1","Containers":[` + containers + `]}`))
	}))
}

func TestReadPicksOldestProcessOfAppContainer(t *testing.T) {
	srv := metadata(t, `{"DockerId":"app123","Name":"app"},{"DockerId":"agent456","Name":"tetherd-agent"}`)
	defer srv.Close()
	root := writeProc(t, map[int]struct{ env, starttime string }{
		1:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/agent456\x00TETHERD_ENV=dev\x00", "100"},
		7:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/app123\x00PORT=8081\x00DB_PASSWORD=s3cret\x00", "200"},
		9:  {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/app123\x00PORT=9999\x00CHILD=1\x00", "250"},
		11: {"", "50"},
	})
	r := &ProcEnvReader{MetadataURL: srv.URL + "/v4/abc", ProcRoot: root, AppContainer: "app"}
	env, arn, err := r.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:ecs:ap-northeast-1:1:task/c/t1" {
		t.Fatalf("arn = %q", arn)
	}
	if env["PORT"] != "8081" || env["DB_PASSWORD"] != "s3cret" || env["CHILD"] != "" {
		t.Fatalf("env = %v (must be pid 7, the oldest app process)", env)
	}
}

func TestReadErrors(t *testing.T) {
	srv := metadata(t, `{"DockerId":"app123","Name":"app"}`)
	defer srv.Close()
	// no process of the app container visible → pidMode hint
	root := writeProc(t, map[int]struct{ env, starttime string }{
		1: {"ECS_CONTAINER_METADATA_URI_V4=" + srv.URL + "/v4/agent456\x00", "100"},
	})
	r := &ProcEnvReader{MetadataURL: srv.URL + "/v4/abc", ProcRoot: root, AppContainer: "app"}
	if _, _, err := r.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "pidMode") {
		t.Fatalf("want pidMode hint, got %v", err)
	}
	// unknown container name
	r.AppContainer = "web"
	if _, _, err := r.Read(context.Background()); err == nil || !strings.Contains(err.Error(), `"web"`) {
		t.Fatalf("want unknown-container error, got %v", err)
	}
	// metadata unreachable
	r.MetadataURL = "http://127.0.0.1:1/v4/abc"
	if _, _, err := r.Read(context.Background()); err == nil {
		t.Fatal("want metadata error")
	}
}

// roundTripFunc adapts a function to http.RoundTripper for tests.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestReadStopsWhenContextCancelled ensures the /proc scan loop, not just
// the metadata HTTP request, honours ctx cancellation so Hello's timeout is
// actually enforced even when the scan itself would otherwise run long.
func TestReadStopsWhenContextCancelled(t *testing.T) {
	srv := metadata(t, `{"DockerId":"app123","Name":"app"}`)
	defer srv.Close()

	procs := map[int]struct{ env, starttime string }{}
	for i := 1; i <= 50; i++ {
		procs[i] = struct{ env, starttime string }{env: "", starttime: "100"}
	}
	root := writeProc(t, procs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &ProcEnvReader{
		MetadataURL:  srv.URL + "/v4/abc",
		ProcRoot:     root,
		AppContainer: "app",
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(req)
			// Cancel only after the metadata call has been served, so the
			// cancellation is observed inside the /proc scan loop.
			cancel()
			return resp, err
		})},
	}
	_, _, err := r.Read(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("want error wrapping context.Canceled, got %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
