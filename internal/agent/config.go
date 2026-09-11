// Package agent is the sidecar: it accepts CLI sessions on the control
// port, dials VPC destinations on their behalf and (from v0.3) proxies ALB
// traffic. It never calls AWS APIs.
package agent

import "errors"

// Config comes from environment variables only.
type Config struct {
	Env     string // TETHERD_ENV, required. Refuse to start without it.
	TaskARN string // TETHERD_TASK_ARN, optional until v0.2 reads metadata.
	Control string // TETHERD_CONTROL, default 127.0.0.1:9900.
}

// ConfigFromEnv builds Config from getenv (os.Getenv in main).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Env:     getenv("TETHERD_ENV"),
		TaskARN: getenv("TETHERD_TASK_ARN"),
		Control: getenv("TETHERD_CONTROL"),
	}
	if cfg.Env == "" {
		return Config{}, errors.New("TETHERD_ENV is not set; tetherd-agent refuses to start without it")
	}
	if cfg.Control == "" {
		cfg.Control = "127.0.0.1:9900"
	}
	return cfg, nil
}
