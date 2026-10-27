// Package cli is the kazari command line: the server (serve, check) and the
// client of the user and admin APIs, parsed by kong from the CLI grammar.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"gopkg.in/yaml.v3"

	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/config"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// CLI is the command grammar. The flags apply to the client commands; serve
// and check have their own.
type CLI struct {
	URL          string `help:"User API URL, such as https://api.art.example.com (env KAZARI_URL)." placeholder:"URL"`
	Token        string `help:"API key, kzk_... (env KAZARI_TOKEN)." placeholder:"KEY"`
	AdminURL     string `name:"admin-url" help:"Admin API URL (env KAZARI_ADMIN_URL; default: admin. in place of api. in --url)." placeholder:"URL"`
	AdminToken   string `name:"admin-token" help:"Admin token, or an admin user's key with the admin scope (env KAZARI_ADMIN_TOKEN; default: --token)." placeholder:"TOKEN"`
	AdminSocket  string `name:"admin-socket" help:"Admin socket of a server on this host, used without a token (env KAZARI_ADMIN_SOCKET; default: found from the server's configuration)." placeholder:"PATH"`
	ClientConfig string `name:"client-config" help:"Client configuration file (default: ~/.config/kazari/config.yaml)." placeholder:"PATH"`
	JSON         bool   `help:"Print the JSON response instead of text."`

	Serve      ServeCmd      `cmd:"" help:"Run the server."`
	Check      CheckCmd      `cmd:"" help:"Check the configuration, the DNS records, the DNS token and the ports."`
	Publish    PublishCmd    `cmd:"" help:"Publish a directory as a new artifact, or mirror it as a new version with --update."`
	Import     ImportCmd     `cmd:"" help:"Copy a web page into a new artifact."`
	Update     UpdateCmd     `cmd:"" help:"Add a version from a directory and/or change fields."`
	Get        GetCmd        `cmd:"" help:"Show an artifact and its versions."`
	List       ListCmd       `cmd:"" help:"List artifacts."`
	Rm         RmCmd         `cmd:"" help:"Delete an artifact, or purge it with --hard."`
	Stale      StaleCmd      `cmd:"" help:"Mark an artifact stale."`
	Unstale    UnstaleCmd    `cmd:"" help:"Clear the stale mark."`
	Visibility VisibilityCmd `cmd:"" help:"Set public, unlisted, members or unpublished."`
	Share      ShareCmd      `cmd:"" help:"Share links that open a members artifact."`
	Rollback   RollbackCmd   `cmd:"" help:"Publish an earlier version."`
	Edit       EditCmd       `cmd:"" help:"Change text in a file and publish it as a new version."`
	Files      FilesCmd      `cmd:"" help:"Read an artifact's files."`
	Skill      SkillCmd      `cmd:"" help:"Print the instance's Agent Skill (SKILL.md)."`
	Me         MeCmd         `cmd:"" help:"Show the caller's identity."`
	Keys       KeysCmd       `cmd:"" help:"List the caller's API keys."`
	Admin      AdminCmd      `cmd:"" help:"Administer the instance: users, keys, artifacts, jobs, audit, health."`
	Version    VersionCmd    `cmd:"" help:"Print the version."`
	Help       HelpCmd       `cmd:"" help:"Show help for a command."`
}

// usageError is a command-line or configuration mistake; Run exits 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exitRequest is kong's exit hook as a panic, so --help stops the parse
// without ending the process: run recovers it into an exit code.
type exitRequest int

type settings struct {
	URL, Token, AdminURL, AdminToken, AdminSocket string
}

// fileConfig is ~/.config/kazari/config.yaml.
type fileConfig struct {
	URL         string `yaml:"url"`
	Token       string `yaml:"token"`
	AdminURL    string `yaml:"admin_url"`
	AdminToken  string `yaml:"admin_token"`
	AdminSocket string `yaml:"admin_socket"`
}

type runner struct {
	ctx       context.Context
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv func(string) (string, bool)
	hc        *http.Client
	now       func() time.Time
	g         *CLI
}

// Run executes one command and returns the exit code: 0 on success, 1 when
// the command or the server fails, 2 for usage and configuration errors.
// args excludes the program name. lookupEnv resolves the KAZARI_* variables
// of the client; the server reads the process environment itself.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	return RunContext(context.Background(), args, stdin, stdout, stderr, lookupEnv)
}

// RunContext is Run under ctx, whose end stops serve.
func RunContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) int {
	r := &runner{
		ctx:       ctx,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		lookupEnv: lookupEnv,
		hc:        &http.Client{},
		now:       time.Now,
		g:         &CLI{},
	}
	return r.exit(r.run(args))
}

func (r *runner) run(args []string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			code, ok := p.(exitRequest)
			if !ok {
				panic(p)
			}
			err = nil
			if code != 0 {
				err = fmt.Errorf("exit %d", code)
			}
		}
	}()
	k, err := kong.New(r.g,
		kong.Name("kazari"),
		kong.Description("Kazari hosts static pages, one origin per artifact. "+
			"serve runs the server; the other commands talk to a running instance."),
		kong.Writers(r.stdout, r.stderr),
		kong.Exit(func(code int) { panic(exitRequest(code)) }),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true, NoExpandSubcommands: true}),
		kong.Bind(r),
	)
	if err != nil {
		return err
	}
	ctx, err := k.Parse(args)
	if err != nil {
		return err
	}
	return ctx.Run()
}

func (r *runner) exit(err error) int {
	var ue *usageError
	var he *HTTPError
	var pe *kong.ParseError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &pe):
		// The usage line, then the mistake, on stderr.
		if pe.Context != nil {
			stdout := pe.Context.Kong.Stdout
			pe.Context.Kong.Stdout = r.stderr
			_ = pe.Context.PrintUsage(true)
			pe.Context.Kong.Stdout = stdout
		}
		fmt.Fprintf(r.stderr, "kazari: %s\n", pe.Error())
		return exitUsage
	case errors.As(err, &ue):
		fmt.Fprintf(r.stderr, "kazari: %s\n", ue.msg)
		return exitUsage
	case errors.As(err, &he):
		fmt.Fprintf(r.stderr, "kazari: %s: %s\n", he.Err.Code, he.Err.Message)
		return exitError
	default:
		fmt.Fprintf(r.stderr, "kazari: %v\n", err)
		return exitError
	}
}

func (r *runner) env(name string) string {
	if r.lookupEnv == nil {
		return ""
	}
	v, _ := r.lookupEnv(name)
	return v
}

// configPath reports whether the path was named explicitly, in which case it
// must exist.
func (r *runner) configPath() (string, bool) {
	if r.g.ClientConfig != "" {
		return r.g.ClientConfig, true
	}
	if x := r.env("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "kazari", "config.yaml"), false
	}
	if h := r.env("HOME"); h != "" {
		return filepath.Join(h, ".config", "kazari", "config.yaml"), false
	}
	return "", false
}

// loadConfigFile rejects unknown keys and skips a missing file unless it was
// named with --client-config.
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

// settings prefers the flag, then the environment, then the config file. A
// missing admin token falls back to the user token: an admin-role key works on
// the admin host.
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
		URL:         pick(r.g.URL, "KAZARI_URL", fc.URL),
		Token:       pick(r.g.Token, "KAZARI_TOKEN", fc.Token),
		AdminURL:    pick(r.g.AdminURL, "KAZARI_ADMIN_URL", fc.AdminURL),
		AdminToken:  pick(r.g.AdminToken, "KAZARI_ADMIN_TOKEN", fc.AdminToken),
		AdminSocket: pick(r.g.AdminSocket, "KAZARI_ADMIN_SOCKET", fc.AdminSocket),
	}
	if s.AdminToken == "" {
		s.AdminToken = s.Token
	}
	if s.AdminURL == "" {
		s.AdminURL = adminFromUserURL(s.URL)
	}
	return s, nil
}

func adminFromUserURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(u.Host, "api.") {
		return ""
	}
	u.Host = "admin." + strings.TrimPrefix(u.Host, "api.")
	return u.String()
}

func (r *runner) userClient() (*Client, error) {
	s, err := r.settings()
	if err != nil {
		return nil, err
	}
	if s.URL == "" {
		return nil, usagef("no API URL: set --url, KAZARI_URL or url in %s", r.configHint())
	}
	if s.Token == "" {
		return nil, usagef("no API token: set --token, KAZARI_TOKEN or token in %s", r.configHint())
	}
	return NewClient(s.URL, s.Token, r.hc), nil
}

// adminClient prefers an explicit socket, then an explicit admin URL (with
// a token unless the command needs none), then a socket found on this host.
func (r *runner) adminClient(needToken bool) (*Client, error) {
	s, err := r.settings()
	if err != nil {
		return nil, err
	}
	if s.AdminSocket != "" {
		return socketClient(s.AdminSocket), nil
	}
	if s.AdminURL != "" && (s.AdminToken != "" || !needToken) {
		return NewClient(s.AdminURL, s.AdminToken, r.hc), nil
	}
	if socket := r.discoverSocket(); socket != "" {
		return socketClient(socket), nil
	}
	if s.AdminURL == "" {
		return nil, usagef("no admin API: run this on the server's host (as its user, as root, or with docker exec) to use the admin socket, " +
			"or set --admin-url (KAZARI_ADMIN_URL) and --admin-token (KAZARI_ADMIN_TOKEN)")
	}
	return nil, usagef("no admin token: set --admin-token, KAZARI_ADMIN_TOKEN or admin_token in %s", r.configHint())
}

// discoverSocket finds the admin socket of a server on this host from its
// configuration: $KAZARI_CONFIG, /etc/kazari/kazari.yaml, $KAZARI_DATA_DIR,
// else the --local preset's data directory. It returns "" unless a socket
// file is there.
func (r *runner) discoverSocket() string {
	path := r.env("KAZARI_CONFIG")
	if path == "" {
		if _, err := os.Stat(defaultConfigPath); err == nil {
			path = defaultConfigPath
		}
	}
	var candidates []string
	if cfg, err := config.Peek(path, r.lookupEnv); err == nil && cfg.DataDir != "" {
		candidates = append(candidates, cfg.AdminSocketPath())
	}
	candidates = append(candidates, config.Local().AdminSocketPath())
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.Mode().Type() == fs.ModeSocket {
			return c
		}
	}
	return ""
}

// socketClient talks to the admin API over the Unix socket at path, which
// needs no token.
func socketClient(path string) *Client {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	return NewClient("http://kazari", "", hc)
}

func (r *runner) configHint() string {
	if p, _ := r.configPath(); p != "" {
		return p
	}
	return "~/.config/kazari/config.yaml"
}

// VersionCmd prints the version.
type VersionCmd struct{}

func (c *VersionCmd) Run(r *runner) error {
	info := buildinfo.Get()
	if r.g.JSON {
		return r.printJSON(info)
	}
	fmt.Fprintf(r.stdout, "kazari %s\n", info)
	return nil
}

// HelpCmd is "kazari help [command]".
type HelpCmd struct {
	Command []string `arg:"" optional:"" help:"Command to show help for, such as admin users."`
}

func (c *HelpCmd) Run(k *kong.Kong) error {
	_, err := k.Parse(append(append([]string(nil), c.Command...), "--help"))
	return err
}
