package oidc

import "time"

// WithTimeout sets the timeout of the client New builds when cfg has none.
func WithTimeout(cfg Config, d time.Duration) Config {
	cfg.timeout = d
	return cfg
}
