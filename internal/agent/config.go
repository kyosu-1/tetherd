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
	// TETHERD_APP_ADDR, default 127.0.0.1:8081. The app container listens
	// on 8081 and only the agent listens on 8080, which is the swap that
	// puts the agent on the ALB's data path (spec §5.1).
	AppAddr string
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
	if cfg.AppAddr == "" {
		cfg.AppAddr = "127.0.0.1:8081"
	}
	return cfg, nil
}
