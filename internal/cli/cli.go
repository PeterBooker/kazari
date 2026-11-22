// Package cli is the kazari command line: the server (serve, check) and the
// client of the user and admin APIs, parsed by kong from the CLI grammar.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"gopkg.in/yaml.v3"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/config"
)

const (
	exitOK           = 0
	exitError        = 1
	exitUsage        = 2
	exitNotFound     = 3
	exitConflict     = 4
	exitRateLimited  = 5
	exitUnauthorized = 6
)

// exitCode sorts the API's error codes into the exit codes a script tests.
func exitCode(code string) int {
	switch code {
	case api.CodeNotFound, api.CodeVersionNotFound, api.CodeFileNotFound:
		return exitNotFound
	case api.CodeSlugTaken, api.CodeVersionConflict, api.CodePublishedNotLatest, api.CodeAnchorMismatch:
		return exitConflict
	case api.CodeRateLimited:
		return exitRateLimited
	case api.CodeUnauthorized, api.CodeForbidden:
		return exitUnauthorized
	}
	return exitError
}

// CLI is the command grammar. The flags apply to the client commands; serve
// and check have their own.
type CLI struct {
	URL          string        `help:"User API URL, such as https://api.art.example.com (env KAZARI_URL)." placeholder:"URL"`
	Token        string        `help:"API key, kzk_... (env KAZARI_TOKEN)." placeholder:"KEY"`
	AdminURL     string        `name:"admin-url" help:"Admin API URL (env KAZARI_ADMIN_URL; default: admin. in place of api. in --url)." placeholder:"URL"`
	AdminToken   string        `name:"admin-token" help:"Admin token, or an admin user's key with the admin scope (env KAZARI_ADMIN_TOKEN; default: --token)." placeholder:"TOKEN"`
	AdminSocket  string        `name:"admin-socket" help:"Admin socket of a server on this host, used without a token (env KAZARI_ADMIN_SOCKET; default: found from the server's configuration)." placeholder:"PATH"`
	ClientConfig string        `name:"client-config" help:"Client configuration file (default: ~/.config/kazari/config.yaml)." placeholder:"PATH"`
	JSON         bool          `help:"Print the JSON response instead of text, and API errors as JSON on stderr."`
	Timeout      time.Duration `help:"Give up on a request after this long, such as 30s or 5m (default: no limit; the server must start answering within 60s)." placeholder:"DURATION"`
	AllowHTTP    bool          `name:"allow-http" help:"Send the key to an http:// URL whose host is not this machine (env KAZARI_ALLOW_HTTP=1)."`

	Serve      ServeCmd      `cmd:"" help:"Run the server."`
	Check      CheckCmd      `cmd:"" help:"Check the configuration and what the server needs before it starts: data directory, DNS records, DNS token, certificate files, ports, identity provider."`
	Login      LoginCmd      `cmd:"" help:"Save the API URL and a key in the client configuration, after checking them."`
	Logout     LogoutCmd     `cmd:"" help:"Remove the key from the client configuration."`
	Publish    PublishCmd    `cmd:"" help:"Publish a directory or a file as a new artifact."`
	Deploy     DeployCmd     `cmd:"" help:"Make an artifact match a directory: create it, or add a version that also deletes the files gone from the directory."`
	Import     ImportCmd     `cmd:"" help:"Copy a web page into a new artifact."`
	Update     UpdateCmd     `cmd:"" help:"Add files from a directory as a new version, keeping the others, and/or change fields."`
	Get        GetCmd        `cmd:"" help:"Show an artifact and its versions."`
	List       ListCmd       `cmd:"" help:"List artifacts."`
	Rm         RmCmd         `cmd:"" help:"Delete an artifact, or purge it with --hard."`
	Restore    RestoreCmd    `cmd:"" help:"Restore a deleted artifact that is not purged yet."`
	Stale      StaleCmd      `cmd:"" help:"Mark an artifact stale."`
	Unstale    UnstaleCmd    `cmd:"" help:"Clear the stale mark."`
	Visibility VisibilityCmd `cmd:"" help:"Set public, unlisted, members or unpublished."`
	Share      ShareCmd      `cmd:"" help:"Share links that open a members artifact."`
	Rollback   RollbackCmd   `cmd:"" help:"Publish an earlier version; until a later one is published, update, deploy and edit need --base to say which version they build on."`
	Edit       EditCmd       `cmd:"" help:"Change text in a file and publish it as a new version."`
	Files      FilesCmd      `cmd:"" help:"List and read an artifact's files."`
	Skill      SkillCmd      `cmd:"" help:"Print the instance's Agent Skill (SKILL.md)."`
	Me         MeCmd         `cmd:"" help:"Show the caller's identity."`
	Keys       KeysCmd       `cmd:"" help:"List, issue, rotate and revoke the caller's API keys."`
	Admin      AdminCmd      `cmd:"" help:"Administer the instance: users, invites, keys, identities, grants, artifacts, jobs, audit, views, backup and restore, readiness."`
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
	args      []string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	lookupEnv func(string) (string, bool)
	// hc is built on first use, once --timeout is parsed.
	hc  *http.Client
	now func() time.Time
	// sleep waits between retries; tests replace it.
	sleep func(context.Context, time.Duration) error
	// terminal reports whether stdin is a terminal a person types at.
	terminal func() bool
	g        *CLI
	// apiURL is the user API the command used, for the hints on errors.
	apiURL string
}

// hintError is an error followed by lines that say what to do next.
type hintError struct {
	err   error
	hints []string
}

func (e *hintError) Error() string { return e.err.Error() }
func (e *hintError) Unwrap() error { return e.err }

func withHints(err error, hints ...string) error {
	if err == nil || len(hints) == 0 {
		return err
	}
	return &hintError{err: err, hints: hints}
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
	r := newRunner(ctx, args, stdin, stdout, stderr, lookupEnv)
	return r.exit(r.run(args))
}

func newRunner(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookupEnv func(string) (string, bool)) *runner {
	return &runner{
		ctx:       ctx,
		args:      args,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		lookupEnv: lookupEnv,
		now:       time.Now,
		sleep:     sleepContext,
		terminal:  func() bool { return isTerminal(stdin) },
		g:         &CLI{},
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// httpClient gives up on a server that accepts no connection within 10s or
// sends no response header within 60s; --timeout bounds the whole request,
// upload included.
func (r *runner) httpClient() *http.Client {
	if r.hc == nil {
		r.hc = &http.Client{
			Timeout: r.g.Timeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return r.hc
}

func (r *runner) newClient(base, token string) *Client {
	c := NewClient(base, token, r.httpClient())
	c.sleep = r.sleep
	return c
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
		kong.Help(serverHelp),
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

// serverHelp leaves the client's flags out of the help of serve and
// check, which do not read them.
func serverHelp(options kong.HelpOptions, ctx *kong.Context) error {
	if n := ctx.Selected(); n != nil && n.Parent == ctx.Model.Node && (n.Name == "serve" || n.Name == "check") {
		for _, f := range ctx.Model.Flags {
			if !f.Hidden && f.Name != "help" {
				f.Hidden = true
				defer func() { f.Hidden = false }()
			}
		}
	}
	return kong.DefaultHelpPrinter(options, ctx)
}

func (r *runner) exit(err error) int {
	var ue *usageError
	var he *HTTPError
	var pe *kong.ParseError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &pe):
		// The usage line, then the mistake, on stderr; a mistyped command
		// gets the mistake and kong's suggestion alone, not the whole list.
		if pe.Context != nil && !unknownCommand(pe) {
			stdout := pe.Context.Kong.Stdout
			pe.Context.Kong.Stdout = r.stderr
			_ = pe.Context.PrintUsage(true)
			pe.Context.Kong.Stdout = stdout
		}
		fmt.Fprintf(r.stderr, "kazari: %s\n", pe.Error())
		if unknownCommand(pe) {
			fmt.Fprintln(r.stderr, "run kazari --help for the commands")
		}
		return exitUsage
	case errors.As(err, &ue):
		fmt.Fprintf(r.stderr, "kazari: %s\n", ue.msg)
		return exitUsage
	case errors.As(err, &he):
		if r.g.JSON {
			enc := json.NewEncoder(termSafe{r.stderr})
			enc.SetEscapeHTML(false)
			_ = enc.Encode(api.ErrorResponse{Error: he.Err})
			return exitCode(he.Err.Code)
		}
		fmt.Fprintf(termSafe{r.stderr}, "kazari: %s: %s\n", he.Err.Code, he.Err.Message)
		r.printHints(err, he)
		return exitCode(he.Err.Code)
	default:
		fmt.Fprintf(r.stderr, "kazari: %v\n", err)
		r.printHints(err, nil)
		return exitError
	}
}

func (r *runner) printHints(err error, he *HTTPError) {
	var hints []string
	if hint, ok := errors.AsType[*hintError](err); ok {
		hints = hint.hints
	}
	if he != nil && he.Err.Code == api.CodeUnauthorized {
		h := "check KAZARI_TOKEN or run kazari login"
		if base := baseDomain(r.apiURL); base != "" {
			h += "; keys are on https://" + base + "/account/"
		}
		hints = append(hints, h)
	}
	for _, h := range hints {
		fmt.Fprintln(termSafe{r.stderr}, h)
	}
}

// unknownCommand reports a word kong could not place before any command
// was chosen.
func unknownCommand(pe *kong.ParseError) bool {
	return pe.Context != nil && pe.Context.Command() == "" && strings.HasPrefix(pe.Error(), "unexpected argument")
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

// configKeys are the keys of fileConfig, in the order the error lists them.
var configKeys = []string{"url", "token", "admin_url", "admin_token", "admin_socket"}

// loadConfigFile rejects unknown keys, and a key in a file other users can
// read, and skips a missing file unless it was named with --client-config.
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
	var keys map[string]any
	if err := yaml.Unmarshal(data, &keys); err != nil {
		return fc, usagef("config %s: %v", path, err)
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Contains(configKeys, k) {
			return fc, usagef("unknown key %q in %s (known: %s)", k, path, strings.Join(configKeys, ", "))
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) {
		return fileConfig{}, usagef("config %s: %v", path, err)
	}
	if fc.Token != "" || fc.AdminToken != "" {
		if err := checkPrivate(path); err != nil {
			return fileConfig{}, err
		}
	}
	return fc, nil
}

func checkPrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return usagef("config %s: %v", path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return usagef("config %s holds a key but other users may read it (mode %04o); run chmod 600 %s",
			path, st.Mode().Perm(), path)
	}
	return nil
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
	if s.URL, err = normalizeURL(s.URL); err != nil {
		return settings{}, err
	}
	if s.AdminURL, err = normalizeURL(s.AdminURL); err != nil {
		return settings{}, err
	}
	if s.AdminToken == "" {
		s.AdminToken = s.Token
	}
	if s.AdminURL == "" {
		s.AdminURL = adminFromUserURL(s.URL)
	}
	return s, nil
}

// normalizeURL takes an API URL as people write it: without a scheme, which
// means https, or http for this machine, and with a trailing / or /v1, which
// the client adds itself.
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		scheme := "https://"
		if u, err := url.Parse("http://" + raw); err == nil && localHost(u.Hostname()) {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", usagef("API URL %q is not an http or https URL; it looks like https://api.<base>", raw)
	}
	for p := ""; p != u.Path; {
		p = u.Path
		u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	}
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// baseDomain guesses the instance's base domain from an API URL.
func baseDomain(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return ""
	}
	for _, p := range []string{"api.", "mcp.", "admin."} {
		if rest, ok := strings.CutPrefix(u.Host, p); ok {
			return rest
		}
	}
	return u.Host
}

// localHost reports a host name that cannot leave this machine.
func localHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkScheme stops a key going over plain http to another machine unless
// --allow-http or KAZARI_ALLOW_HTTP=1 says so.
func (r *runner) checkScheme(rawURL, token string) error {
	if token == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return usagef("API URL %q: %v", rawURL, err)
	}
	if u.Scheme != "http" || localHost(u.Hostname()) {
		return nil
	}
	if r.g.AllowHTTP || r.env("KAZARI_ALLOW_HTTP") == "1" {
		return nil
	}
	return usagef("not sending the key to %s over plain http: use https, or pass --allow-http (KAZARI_ALLOW_HTTP=1)", rawURL)
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
		return nil, usagef("no API URL: run kazari login --url https://api.<base>, or set --url, KAZARI_URL or url in %s", r.configHint())
	}
	if s.Token == "" {
		return nil, usagef("no API token: run kazari login, or set --token, KAZARI_TOKEN or token in %s", r.configHint())
	}
	if err := r.checkScheme(s.URL, s.Token); err != nil {
		return nil, err
	}
	r.apiURL = s.URL
	return r.newClient(s.URL, s.Token), nil
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
		if err := r.checkScheme(s.AdminURL, s.AdminToken); err != nil {
			return nil, err
		}
		c := r.newClient(s.AdminURL, s.AdminToken)
		c.host = "admin"
		return c, nil
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

// serverConfig reads, without validating it, the configuration of a server
// on this host: $KAZARI_CONFIG, else /etc/kazari/kazari.yaml, with the
// kazari.env beside it when this user can read it, under the KAZARI_*
// variables of the environment.
func (r *runner) serverConfig() (config.Config, error) {
	path := r.env("KAZARI_CONFIG")
	if path == "" {
		if _, err := os.Stat(defaultConfigPath); err == nil {
			path = defaultConfigPath
		}
	}
	lookup := r.lookupEnv
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	if path != "" {
		if dotenv, err := config.LoadEnvFile(filepath.Join(filepath.Dir(path), config.EnvFileName)); err == nil {
			env := lookup
			lookup = func(key string) (string, bool) {
				if v, ok := env(key); ok {
					return v, true
				}
				v, ok := dotenv[key]
				return v, ok
			}
		}
	}
	return config.Peek(path, lookup)
}

// discoverSocket finds the admin socket of a server on this host from its
// configuration, as serverConfig reads it, else the --local preset's data
// directory. It returns "" unless a socket file is there.
func (r *runner) discoverSocket() string {
	var candidates []string
	if cfg, err := r.serverConfig(); err == nil && cfg.DataDir != "" {
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
	c := NewClient("http://kazari", "", hc)
	c.local = true
	return c
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
