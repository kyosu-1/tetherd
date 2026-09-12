// Package agent is the sidecar: it accepts CLI sessions on the control
// port, dials VPC destinations on their behalf and (from v0.3) proxies ALB
// traffic. It never calls AWS APIs.
package agent

import "errors"

// Config comes from environment variables only.
type Config struct {
	Env          string // TETHERD_ENV, required. Refuse to start without it.
	TaskARN      string // TETHERD_TASK_ARN, optional; metadata wins when present.
	Control      string // TETHERD_CONTROL, default 127.0.0.1:9900.
	AppContainer string // TETHERD_APP_CONTAINER, default app.
	MetadataURL  string // ECS_CONTAINER_METADATA_URI_V4, set by ECS; empty outside ECS.
	// Proxy is where the ALB arrives: TETHERD_PROXY, default
	// 0.0.0.0:8080. Unlike Control it cannot be a loopback address by
	// default - the ALB reaches the task over the VPC network.
	Proxy string
	// AppAddr is the application the proxy passes everything through to:
	// TETHERD_APP_ADDR, default defaultAppAddr. The app container listens
	// on 8081 and only the agent listens on 8080, which is the swap that
	// puts the agent on the ALB's data path (spec §5.1).
	AppAddr string
}

// defaultAppAddr is where the proxy passes requests through to when nothing
// says otherwise. One constant for the two paths that must agree -
// ConfigFromEnv and Config.withDefaults - because the copy that drifts is
// the one no task definition mentions.
const defaultAppAddr = "127.0.0.1:8081"

// withDefaults fills in what a Config built by hand would otherwise hand on
// empty. New applies it, so the paths that do not go through ConfigFromEnv
// (tests today, an embedded agent later) get the documented behaviour.
//
// Only AppAddr, because it is the one that fails quietly: Control and Proxy
// are listen addresses, so an empty one is a startup error naming the port
// (see Run), while an empty AppAddr is a dial target the proxy carries
// forward - every request through it then fails against an address nobody
// typed, several layers away from the empty field that caused it.
func (c Config) withDefaults() Config {
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
	if cfg.Control == "" {
		cfg.Control = "127.0.0.1:9900"
	}
	if cfg.AppContainer == "" {
		cfg.AppContainer = "app"
	}
	if cfg.Proxy == "" {
		cfg.Proxy = "0.0.0.0:8080"
	}
	// The last default comes from withDefaults rather than from a literal
	// here, so that the environment path and New's cannot disagree about
	// where the application is.
	return cfg.withDefaults(), nil
}
