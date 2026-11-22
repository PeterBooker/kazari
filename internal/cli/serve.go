package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/telemetry"
)

const defaultConfigPath = "/etc/kazari/kazari.yaml"

// serverFlags are shared by serve and check; each command adds its own
// --local, whose help differs.
type serverFlags struct {
	Config  string `help:"Configuration file (default: $KAZARI_CONFIG, then /etc/kazari/kazari.yaml if it exists, else the environment alone)." placeholder:"PATH" xor:"source"`
	EnvFile string `name:"env-file" help:"Dotenv file with the secrets (default: $KAZARI_ENV_FILE, then kazari.env beside the configuration file, then the dotenv file in the working directory, which beside a configuration file only fills references)." placeholder:"PATH"`
}

// load resolves the configuration as serve does.
func (f *serverFlags) load(r *runner, local bool, cmd string) (cfg config.Config, origin config.Origin, err error) {
	envFile := f.EnvFile
	if envFile == "" {
		envFile = r.env("KAZARI_ENV_FILE")
	}
	if local {
		cfg, origin, err = config.LoadLocal(envFile)
		return cfg, origin, wrapConfig(err, origin, cmd)
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
	cfg, origin, err = config.Load(path, envFile)
	return cfg, origin, wrapConfig(err, origin, cmd)
}

// wrapConfig makes a configuration error exit 2, with what may explain it:
// a dotenv file that could not be read, no configuration file at all, and
// a pointer to the docs.
func wrapConfig(err error, origin config.Origin, cmd string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return usagef("%s", err.Error())
	}
	var notes []string
	if origin.Unreadable != "" {
		notes = append(notes, fmt.Sprintf("%s was skipped; run kazari %s as root, or name a readable file with --env-file", origin.Unreadable, cmd))
	}
	if origin.File == "" && origin.Preset == "" {
		notes = append(notes, "no configuration file: none was named with --config or $KAZARI_CONFIG, and "+defaultConfigPath+
			" does not exist; to try Kazari on this machine, run kazari "+cmd+" --local")
	}
	see := "see https://kazari.io/docs/configuration/"
	if cmd != "check" {
		see += " or run kazari check"
	}
	notes = append(notes, see)
	return usagef("%s\n(%s)", err.Error(), strings.Join(notes, ")\n("))
}

// ServeCmd runs the server.
type ServeCmd struct {
	serverFlags
	Local bool `help:"Serve http://<slug>.kazari.localhost:8080/ with the data in ./.data: no DNS, certificate or token needed. KAZARI_* variables still apply." xor:"source"`
}

func (c *ServeCmd) Run(r *runner) error {
	cfg, origin, err := c.load(r, c.Local, "serve")
	if err != nil {
		return err
	}
	logger := cfg.Logger(r.stderr, journald(r.env("JOURNAL_STREAM"), r.stderr))
	slog.SetDefault(logger)
	telemetry.CaptureLibraryLogs(logger)
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, app.Options{Logger: logger, Origin: origin})
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// journald reports whether w is the journal stream systemd connected, which
// it names in JOURNAL_STREAM as device:inode; a redirect to a file or pipe
// keeps the variable but not the stream.
func journald(stream string, w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || stream == "" {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && stream == fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// CheckCmd validates the configuration and runs the preflight checks.
type CheckCmd struct {
	serverFlags
	Local bool `help:"Check the --local preset of kazari serve: http://<slug>.kazari.localhost:8080/ with the data in ./.data. KAZARI_* variables still apply." xor:"source"`
}

func (c *CheckCmd) Run(r *runner) error {
	cfg, origin, err := c.load(r, c.Local, "check")
	if err != nil {
		return err
	}
	findings := []preflight.Finding{{Check: "config", Status: preflight.OK, Detail: preflight.Describe(cfg, origin)}}
	findings = append(findings, preflight.Sources(origin)...)
	tel := app.TelemetryOptions(cfg)
	findings = append(findings, preflight.Run(r.ctx, cfg, preflight.Options{Telemetry: &tel})...)
	preflight.Write(r.stdout, findings)
	if preflight.Failed(findings) {
		return errors.New("preflight checks failed")
	}
	return nil
}
