package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Exit codes returned by Run.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError is a mistake in the command line or the client configuration;
// Run prints it and exits 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// errUsagePrinted is a usage error the flag package already reported.
var errUsagePrinted = errors.New("usage error already printed")

// globals are the flags before the subcommand.
type globals struct {
	url, token           string
	adminURL, adminToken string
	config               string
	json                 bool
}

// settings is the resolved client configuration.
type settings struct {
	URL, Token, AdminURL, AdminToken string
}

// fileConfig is ~/.config/agalma/config.yaml.
type fileConfig struct {
	URL        string `yaml:"url"`
	Token      string `yaml:"token"`
	AdminURL   string `yaml:"admin_url"`
	AdminToken string `yaml:"admin_token"`
}

// runner carries the I/O and configuration of one Run.
type runner struct {
	ctx       context.Context
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv func(string) (string, bool)
	hc        *http.Client
	now       func() time.Time
	g         globals
}

// Run executes one agalma client command and returns the process exit code:
// 0 on success, 1 when the command or the server fails, 2 for usage and
// configuration errors. args excludes the program name, for example
// []string{"--json", "get", "my-slug"}. lookupEnv is normally os.LookupEnv.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	r := &runner{
		ctx:       context.Background(),
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		lookupEnv: lookupEnv,
		hc:        &http.Client{},
		now:       time.Now,
	}
	return r.exit(r.run(args))
}

func (r *runner) run(args []string) error {
	fs := flag.NewFlagSet("agalma", flag.ContinueOnError)
	fs.SetOutput(r.stderr)
	fs.StringVar(&r.g.url, "url", "", "user API URL, e.g. https://api.art.example.com (env AGALMA_URL)")
	fs.StringVar(&r.g.token, "token", "", "API key (env AGALMA_TOKEN)")
	fs.StringVar(&r.g.adminURL, "admin-url", "", "admin API URL (env AGALMA_ADMIN_URL)")
	fs.StringVar(&r.g.adminToken, "admin-token", "", "admin token (env AGALMA_ADMIN_TOKEN)")
	fs.StringVar(&r.g.config, "config", "", "config file (default $XDG_CONFIG_HOME/agalma/config.yaml or ~/.config/agalma/config.yaml)")
	fs.BoolVar(&r.g.json, "json", false, "print JSON responses")
	fs.Usage = func() {
		fmt.Fprint(r.stderr, topUsage)
		fmt.Fprintln(r.stderr, "\nGlobal flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsagePrinted
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errUsagePrinted
	}
	name, rest := fs.Arg(0), fs.Args()[1:]
	if name == "help" {
		fs.Usage()
		return nil
	}
	cmd, ok := commands()[name]
	if !ok {
		return usagef("unknown command %q (run agalma help)", name)
	}
	return cmd(r, rest)
}

// exit prints err and maps it to an exit code.
func (r *runner) exit(err error) int {
	var ue *usageError
	var he *HTTPError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsagePrinted):
		return exitUsage
	case errors.As(err, &ue):
		fmt.Fprintf(r.stderr, "agalma: %s\n", ue.msg)
		return exitUsage
	case errors.As(err, &he):
		fmt.Fprintf(r.stderr, "agalma: %s: %s\n", he.Err.Code, he.Err.Message)
		return exitError
	default:
		fmt.Fprintf(r.stderr, "agalma: %v\n", err)
		return exitError
	}
}

// env returns a non-empty environment variable.
func (r *runner) env(name string) string {
	if r.lookupEnv == nil {
		return ""
	}
	v, _ := r.lookupEnv(name)
	return v
}

// configPath returns the config file path and whether it was named
// explicitly (in which case it must exist).
func (r *runner) configPath() (string, bool) {
	if r.g.config != "" {
		return r.g.config, true
	}
	if x := r.env("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "agalma", "config.yaml"), false
	}
	if h := r.env("HOME"); h != "" {
		return filepath.Join(h, ".config", "agalma", "config.yaml"), false
	}
	return "", false
}

// loadConfigFile reads the YAML config file, if any. Unknown keys are an
// error; a missing file is skipped unless it was named with --config.
func (r *runner) loadConfigFile() (fileConfig, error) {
	var fc fileConfig
	path, explicit := r.configPath()
	if path == "" {
		return fc, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return fc, nil
		}
		return fc, usagef("config %s: %v", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return fileConfig{}, usagef("config %s: %v", path, err)
	}
	return fc, nil
}

// settings resolves each value from the flag, then the environment, then
// the config file. A missing admin token falls back to the user token (an
// admin-role key works on the admin host), and a missing admin URL is
// derived from an api.<base> user URL as admin.<base>.
func (r *runner) settings() (settings, error) {
	fc, err := r.loadConfigFile()
	if err != nil {
		return settings{}, err
	}
	pick := func(flagVal, envName, fileVal string) string {
		if flagVal != "" {
			return flagVal
		}
		if v := r.env(envName); v != "" {
			return v
		}
		return fileVal
	}
	s := settings{
		URL:        pick(r.g.url, "AGALMA_URL", fc.URL),
		Token:      pick(r.g.token, "AGALMA_TOKEN", fc.Token),
		AdminURL:   pick(r.g.adminURL, "AGALMA_ADMIN_URL", fc.AdminURL),
		AdminToken: pick(r.g.adminToken, "AGALMA_ADMIN_TOKEN", fc.AdminToken),
	}
	if s.AdminToken == "" {
		s.AdminToken = s.Token
	}
	if s.AdminURL == "" {
		s.AdminURL = adminFromUserURL(s.URL)
	}
	return s, nil
}

// adminFromUserURL maps https://api.<base> to https://admin.<base>.
func adminFromUserURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Host, "api.") {
		return ""
	}
	u.Host = "admin." + strings.TrimPrefix(u.Host, "api.")
	return u.String()
}

// userClient returns a client for the user API.
func (r *runner) userClient() (*Client, error) {
	s, err := r.settings()
	if err != nil {
		return nil, err
	}
	if s.URL == "" {
		return nil, usagef("no API URL: set --url, AGALMA_URL or url in %s", r.configHint())
	}
	if s.Token == "" {
		return nil, usagef("no API token: set --token, AGALMA_TOKEN or token in %s", r.configHint())
	}
	return NewClient(s.URL, s.Token, r.hc), nil
}

// adminClient returns a client for the admin API.
func (r *runner) adminClient() (*Client, error) {
	s, err := r.settings()
	if err != nil {
		return nil, err
	}
	if s.AdminURL == "" {
		return nil, usagef("no admin API URL: set --admin-url, AGALMA_ADMIN_URL or admin_url in %s", r.configHint())
	}
	if s.AdminToken == "" {
		return nil, usagef("no admin token: set --admin-token, AGALMA_ADMIN_TOKEN or admin_token in %s", r.configHint())
	}
	return NewClient(s.AdminURL, s.AdminToken, r.hc), nil
}

func (r *runner) configHint() string {
	if p, _ := r.configPath(); p != "" {
		return p
	}
	return "~/.config/agalma/config.yaml"
}

// newFlags returns a flag set for a subcommand. usage is the synopsis after
// "agalma ". Every subcommand also accepts --json after its name.
func (r *runner) newFlags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(r.stderr)
	fs.BoolVar(&r.g.json, "json", r.g.json, "print the JSON response")
	fs.Usage = func() {
		fmt.Fprintf(r.stderr, "usage: agalma %s\n", usage)
		var hasFlags bool
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(r.stderr, "\nFlags:")
			fs.PrintDefaults()
		}
	}
	return fs
}

// parse parses flags interspersed with positional arguments (so both
// "publish ./site --slug x" and "publish --slug x ./site" work) and checks
// that there are between min and max positionals (max < 0: no limit).
func parse(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errUsagePrinted
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		fs.Usage()
		return nil, errUsagePrinted
	}
	return pos, nil
}

// flagSet reports whether the named flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// listFlag is a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// metaFlag is a repeatable k=v flag.
type metaFlag map[string]string

func (m metaFlag) String() string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (m metaFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	m[k] = val
	return nil
}

const topUsage = `usage: agalma [global flags] <command> [arguments]

Commands:
  publish <dir> --slug <slug>   publish a directory as a new artifact (--update to add a version)
  update <slug> [dir]           add a version from a directory and/or change fields
  get <slug>                    show an artifact and its versions
  list                          list artifacts
  rm <slug>                     delete an artifact (--hard to purge)
  stale <slug>                  mark stale
  unstale <slug>                clear the stale mark
  visibility <slug> <v>         set public, unlisted or unpublished
  rollback <slug> <version>     publish an earlier version
  files cat <slug> <path>       write a file to stdout
  me                            show the caller's identity
  keys                          list the caller's API keys
  version                       print the client version
  admin <command>               admin API: users, keys, artifacts, transfer, hide,
                                settings, jobs, audit, health

Run "agalma <command> -h" for a command's flags.
`
