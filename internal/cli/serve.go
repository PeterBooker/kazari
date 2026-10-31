package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/telemetry"
)

const defaultConfigPath = "/etc/kazari/kazari.yaml"

// serverFlags are shared by serve and check.
type serverFlags struct {
	Config  string `help:"Configuration file (default: $KAZARI_CONFIG, then /etc/kazari/kazari.yaml if it exists, else the environment alone)." placeholder:"PATH" xor:"source"`
	EnvFile string `name:"env-file" help:"Dotenv file with the secrets (default: $KAZARI_ENV_FILE, then the dotenv file in the working directory if it exists)." placeholder:"PATH"`
	Local   bool   `help:"Serve http://<slug>.localhost:8080/ with the data in ./.data: no DNS, certificate or token needed. KAZARI_* variables still apply." xor:"source"`
}

// load resolves the configuration as serve does. source names where the
// file came from, "" for the environment alone.
func (f *serverFlags) load(r *runner) (cfg config.Config, source string, err error) {
	envFile := f.EnvFile
	if envFile == "" {
		envFile = r.env("KAZARI_ENV_FILE")
	}
	if f.Local {
		cfg, err = config.LoadLocal(envFile)
		return cfg, "--local", wrapConfig(err)
	}
	path := f.Config
	if path == "" {
		path = r.env("KAZARI_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat(defaultConfigPath); err == nil {
			path = defaultConfigPath
		}
	}
	cfg, err = config.Load(path, envFile)
	return cfg, path, wrapConfig(err)
}

// wrapConfig makes a configuration error exit 2, with a pointer to the docs
// for a missing setting.
func wrapConfig(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return usagef("%s", err.Error())
	}
	return usagef("%s\n(see https://kazari.io/docs/configuration/ or run kazari check)", err.Error())
}

// ServeCmd runs the server.
type ServeCmd struct {
	serverFlags
}

func (c *ServeCmd) Run(r *runner) error {
	cfg, _, err := c.load(r)
	if err != nil {
		return err
	}
	logger := cfg.Logger(r.stderr)
	slog.SetDefault(logger)
	telemetry.CaptureLibraryLogs(logger)
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, app.Options{Logger: logger})
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// CheckCmd validates the configuration and runs the preflight checks.
type CheckCmd struct {
	serverFlags
}

func (c *CheckCmd) Run(r *runner) error {
	cfg, source, err := c.load(r)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.stdout, "%-12s %-5s %s\n", "config", preflight.OK, preflight.Describe(cfg, source))
	tel := app.TelemetryOptions(cfg)
	findings := preflight.Run(r.ctx, cfg, preflight.Options{Telemetry: &tel})
	preflight.Write(r.stdout, findings)
	if preflight.Failed(findings) {
		return errors.New("preflight checks failed")
	}
	return nil
}
