// Package agent is the sidecar: it accepts CLI sessions on the control
// port, dials VPC destinations on their behalf and (from v0.3) proxies ALB
// traffic. It never calls AWS APIs.
package agent

import (
	"errors"
	"net"
	"strconv"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// Config comes from environment variables only.
type Config struct {
	Env     string // TETHERD_ENV, required. Refuse to start without it.
	TaskARN string // TETHERD_TASK_ARN, optional; metadata wins when present.
	// Control is the port the CLIs attach to: TETHERD_CONTROL, default
	// defaultControl. Loopback, and not by accident - it is what keeps the
	// control port unreachable from the task's ENI without a
	// security-group change, and the SSM port forward terminates inside
	// the task, on 127.0.0.1 (docs/design.md: ":9900 (lo only)").
	//
	// It is not a deployment setting, despite reading like one. The CLI's
	// ssm transport and this default now come from one constant
	// (proto.DefaultControlPort), so the two cannot drift, but the CLI
	// still cannot follow a deployment that changes this: the only channel
	// on which it could learn the new port is the control port itself. A
	// task that sets TETHERD_CONTROL=127.0.0.1:9901 therefore listens
	// where no CLI looks, and the failure arrives as "the agent is
	// unreachable" - pointing at the transport rather than at the setting
	// that was changed. So this exists for tests and for an embedded
	// agent; carrying a changed port properly means putting it in
	// .tetherd.yml, which the CLI reads before it connects, and that is a
	// v1 decision.
	Control      string
	AppContainer string // TETHERD_APP_CONTAINER, default defaultAppContainer.
	MetadataURL  string // ECS_CONTAINER_METADATA_URI_V4, set by ECS; empty outside ECS.
	// Proxy is where the ALB arrives: TETHERD_PROXY, default
	// defaultProxy. Unlike Control it cannot be a loopback address by
	// default - the ALB reaches the task over the VPC network.
	Proxy string
	// AppAddr is the application the proxy passes everything through to:
	// TETHERD_APP_ADDR, default defaultAppAddr. The app container listens
	// on 8081 and only the agent listens on 8080, which is the swap that
	// puts the agent on the ALB's data path (spec §5.1).
	AppAddr string
}

// The defaults, one constant each and nowhere else. Both paths that build a
// Config - ConfigFromEnv and Config.withDefaults - read these, because the
// copy that drifts is the one no task definition mentions.
const (
	defaultAppContainer = "app"
	defaultAppAddr      = "127.0.0.1:8081"
)

// defaultControl and defaultProxy are the two listen addresses, and neither
// port half is written here: both numbers live in internal/proto, where the
// other side of each one reads them too - `tetherd doctor` compares the ALB
// target group's port against the proxy port, and the CLI's ssm transport
// forwards to the control port. A literal here as well would be the second
// copy, and the one that drifts.
//
// Vars rather than consts only because the addresses are assembled from
// those ports; nothing assigns to either. defaultControl is loopback on
// purpose (see Config.Control); defaultProxy is 0.0.0.0 for the opposite
// reason, because the ALB reaches the task over the VPC network.
var (
	defaultControl = net.JoinHostPort("127.0.0.1", strconv.Itoa(proto.DefaultControlPort))
	defaultProxy   = net.JoinHostPort("0.0.0.0", strconv.Itoa(proto.DefaultProxyPort))
)

// withDefaults fills in what a Config built by hand would otherwise hand on
// empty. New applies it, so the paths that do not go through ConfigFromEnv
// (tests today, an embedded agent later) get the same agent the environment
// path produces.
//
// Every field, not just the dial target. An empty address is not a startup
// error: net.Listen("tcp", "") succeeds and binds *every* interface on a
// kernel-chosen port. Measured on this branch -
// New(Config{Env: "dev"}).Run(ctx) returned nil and logged "control
// listening on [::]:58165, proxy listening on [::]:58166" - so an embedded
// agent built from a Config by hand would have published the control port,
// which is documented loopback-only, on the task's ENI, and served the ALB
// port on a port the target group has never heard of. Neither says a word
// at startup.
//
// So: defaults here rather than a refusal in Run. Every one of them is the
// safe value - loopback for control, the documented port for the ALB - and
// a caller who means something else says so in the field.
func (c Config) withDefaults() Config {
	if c.Control == "" {
		c.Control = defaultControl
	}
	if c.AppContainer == "" {
		c.AppContainer = defaultAppContainer
	}
	if c.Proxy == "" {
		c.Proxy = defaultProxy
	}
	if c.AppAddr == "" {
		c.AppAddr = defaultAppAddr
	}
	return c
}

// ConfigFromEnv builds Config from getenv (os.Getenv in main).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Env:          getenv("TETHERD_ENV"),
		TaskARN:      getenv("TETHERD_TASK_ARN"),
		Control:      getenv("TETHERD_CONTROL"),
		AppContainer: getenv("TETHERD_APP_CONTAINER"),
		MetadataURL:  getenv("ECS_CONTAINER_METADATA_URI_V4"),
		Proxy:        getenv("TETHERD_PROXY"),
		AppAddr:      getenv("TETHERD_APP_ADDR"),
	}
	if cfg.Env == "" {
		return Config{}, errors.New("TETHERD_ENV is not set; tetherd-agent refuses to start without it")
	}
	// Every default comes from withDefaults rather than from literals here,
	// so that the environment path and New's cannot disagree about where
	// the control port, the ALB port or the application is. This function
	// is then only "read the environment, and refuse without TETHERD_ENV".
	return cfg.withDefaults(), nil
}
