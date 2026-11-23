// Package preflight checks a configuration against the world before the
// first start: the data directory, the DNS records, the DNS provider token,
// the listener ports and the OpenTelemetry endpoint. kazari check prints its
// findings.
package preflight

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/telemetry"
)

// Status is a finding's severity: ok, warn (worth knowing, not blocking),
// fail or skip.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Finding is one line of the report.
type Finding struct {
	// data_dir, dns, dns token, ports, admin socket, metrics, dashboard,
	// telemetry; from Ready also config, certificate, disk, database, blobs
	Check  string
	Status Status
	Detail string
}

// Options replace the network and the file system in tests.
type Options struct {
	// Lookup resolves a host name; nil means the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// LocalAddrs lists this host's addresses; nil means net.InterfaceAddrs.
	LocalAddrs func() ([]net.Addr, error)
	// DNSProvider replaces the one the configuration names.
	DNSProvider certmagic.DNSProvider
	// Zone skips the SOA lookup of the base domain's zone.
	Zone string
	// Nameservers for the zone lookup; nil means the system's.
	Nameservers []string
	// Timeout bounds the DNS token check. Default: a minute.
	Timeout time.Duration
	// Telemetry is the telemetry section as the server builds it; nil skips
	// the check of the OTLP endpoint.
	Telemetry *telemetry.Options
	// InContainer reports whether this process runs in a container, whose
	// addresses are not the host's; nil looks for the files Docker and
	// Podman create.
	InContainer func() bool
	// HTTPClient fetches the identity provider's discovery document; nil
	// means http.DefaultClient.
	HTTPClient *http.Client
}

// Run performs every check and returns the findings; Failed reports whether
// any is a Fail.
func Run(ctx context.Context, cfg config.Config, opts Options) []Finding {
	var out []Finding
	add := func(check string, s Status, format string, args ...any) {
		out = append(out, Finding{Check: check, Status: s, Detail: fmt.Sprintf(format, args...)})
	}
	checkDataDir(cfg, add)
	checkDNS(ctx, cfg, opts, add)
	checkDNSToken(ctx, cfg, opts, add)
	checkCertFiles(cfg, time.Now(), add)
	checkPorts(ctx, cfg, add)
	checkSocket(ctx, cfg, add)
	checkOIDC(ctx, cfg, opts, add)
	checkMetrics(cfg, add)
	checkDashboard(cfg, add)
	if opts.Telemetry != nil {
		checkTelemetry(ctx, *opts.Telemetry, add)
	}
	return out
}

// checkMetrics says that a scraper of the public admin host gets nothing
// until the scrape token is set, which 0.6.0 made a requirement. Most
// instances scrape nothing, so it is no warning.
func checkMetrics(cfg config.Config, add adder) {
	t := cfg.Telemetry.Metrics
	if !t.Prometheus || t.Token != "" || cfg.Admin.Listen != "" || config.IsLocalhost(cfg.BaseDomain) {
		return
	}
	add("metrics", Skip, "/metrics is not served on %s until telemetry.metrics.token is set, or scrape admin.listen on a private address",
		cfg.Host(config.AdminHost))
}

// checkDashboard reports where a browser finds the dashboard, and an address
// that cannot carry its session.
func checkDashboard(cfg config.Config, add adder) {
	if !cfg.Admin.Dashboard {
		return
	}
	addr := cfg.AdminURL()
	if addr == "" {
		add("dashboard", Warn, "off: admin.listen is set and admin.public_url is not; set admin.public_url to the address a browser uses for the admin host, "+
			"for example http://localhost:9443 through an SSH tunnel")
		return
	}
	if u, err := url.Parse(addr); err == nil && u.Scheme == "http" && !config.IsLoopbackHost(u.Hostname()) {
		add("dashboard", Fail, "%s is plain http, so the session cookie would cross the network unencrypted; "+
			"set admin.public_url to an https address, or set admin.dashboard to false", addr)
		return
	}
	add("dashboard", OK, "%s", addr)
}

// checkTelemetry sends one record per enabled signal to its OTLP endpoint.
func checkTelemetry(ctx context.Context, o telemetry.Options, add adder) {
	for _, w := range telemetry.Warnings(o) {
		add("telemetry", Warn, "%s", w)
	}
	results := telemetry.Check(ctx, o)
	if len(results) == 0 {
		add("telemetry", Skip, "no OTLP endpoint is set; nothing is exported")
		return
	}
	type target struct{ protocol, endpoint string }
	var order []target
	accepted := map[target][]string{}
	for _, r := range results {
		t := target{r.Protocol, strings.TrimSuffix(r.Endpoint, "/v1/"+r.Signal)}
		if r.Err != nil {
			add("telemetry", Fail, "%s %s: %s refused: %v; %s", t.protocol, t.endpoint, r.Signal, r.Err, r.Problem)
			continue
		}
		if _, seen := accepted[t]; !seen {
			order = append(order, t)
		}
		accepted[t] = append(accepted[t], r.Signal)
	}
	for _, t := range order {
		add("telemetry", OK, "%s %s: %s accepted", t.protocol, t.endpoint, strings.Join(accepted[t], ", "))
	}
}

// Failed reports whether any finding is a Fail.
func Failed(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Status == Fail })
}

// Write prints the findings as aligned lines, then a summary.
func Write(w io.Writer, findings []Finding) {
	fails, warns := 0, 0
	for _, f := range findings {
		fmt.Fprintf(w, "%-12s %-5s %s\n", f.Check, f.Status, f.Detail)
		switch f.Status {
		case Fail:
			fails++
		case Warn:
			warns++
		}
	}
	switch {
	case fails > 0:
		fmt.Fprintf(w, "\n%d check(s) failed\n", fails)
	case warns > 0:
		fmt.Fprintf(w, "\nall checks passed, %d warning(s)\n", warns)
	default:
		fmt.Fprintln(w, "\nall checks passed")
	}
}

type adder func(check string, s Status, format string, args ...any)

func checkDataDir(cfg config.Config, add adder) {
	next := ""
	if os.Geteuid() != 0 {
		next = "; run kazari check as root (sudo kazari check), which the service's directory needs, or set data_dir to a directory you can write"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			next = ""
		}
		add("data_dir", Fail, "%s: %v%s", cfg.DataDir, err, next)
		return
	}
	f, err := os.CreateTemp(cfg.DataDir, ".check-*")
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			next = ""
		}
		add("data_dir", Fail, "%s is not writable: %v%s", cfg.DataDir, err, next)
		return
	}
	f.Close()
	os.Remove(f.Name())
	add("data_dir", OK, "%s is writable", cfg.DataDir)
}

// checkDNS resolves the base domain, a fixed host and a random one, which
// only a wildcard record answers, and compares the answers with this host's
// addresses.
func checkDNS(ctx context.Context, cfg config.Config, opts Options, add adder) {
	if config.IsLocalhost(cfg.BaseDomain) {
		add("dns", Skip, "%s resolves to this machine without records", cfg.BaseDomain)
		return
	}
	lookup := opts.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	localAddrs := opts.LocalAddrs
	if localAddrs == nil {
		localAddrs = net.InterfaceAddrs
	}
	inContainer := opts.InContainer
	if inContainer == nil {
		inContainer = runningInContainer
	}
	local := map[netip.Addr]bool{}
	if addrs, err := localAddrs(); err == nil {
		for _, a := range addrs {
			if p, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(p.IP); ok {
					local[ip.Unmap()] = true
				}
			}
		}
	}
	names := []struct{ host, why string }{
		{cfg.BaseDomain, "the apex record"},
		{"api." + cfg.BaseDomain, "the wildcard record"},
		{randomLabel() + "." + cfg.BaseDomain, "the wildcard record, which answers any name"},
	}
	for _, n := range names {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		addrs, err := lookup(lctx, n.host)
		cancel()
		if err != nil || len(addrs) == 0 {
			add("dns", Fail, "%s does not resolve: add %s", n.host, n.why)
			continue
		}
		var shown []string
		mine := false
		for _, a := range addrs {
			shown = append(shown, a.String())
			if local[a.Unmap()] {
				mine = true
			}
		}
		where := "this host"
		status := OK
		switch {
		case mine:
		case inContainer():
			where = "in a container, which does not see the host's addresses; check that this is the host's public address"
		default:
			where = "not an address of this host; fine behind NAT, a proxy or a tunnel"
			status = Warn
		}
		add("dns", status, "%s -> %s (%s)", n.host, strings.Join(shown, ", "), where)
	}
}

// checkDNSToken writes and removes a TXT record with the configured
// provider, which is exactly what issuance does.
func checkDNSToken(ctx context.Context, cfg config.Config, opts Options, add adder) {
	switch {
	case cfg.Listen.ProxyMode:
		add("dns token", Skip, "proxy mode: the proxy holds the certificate")
		return
	case cfg.TLS.Mode != "acme":
		add("dns token", Skip, "tls.mode is %s", cfg.TLS.Mode)
		return
	}
	provider := opts.DNSProvider
	if provider == nil {
		var err error
		if provider, err = certs.NewDNSProvider(certs.DNSConfig{
			Provider: cfg.TLS.DNSProvider, APIToken: cfg.TLS.DNS.APIToken, Endpoint: cfg.TLS.DNS.Endpoint,
			User: cfg.TLS.DNS.User, ClientIP: cfg.TLS.DNS.ClientIP,
		}); err != nil {
			add("dns token", Fail, "%v", err)
			return
		}
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	zone := opts.Zone
	if zone == "" {
		var err error
		if zone, err = certs.FindZone(tctx, cfg.BaseDomain, opts.Nameservers); err != nil {
			add("dns token", Fail, "%v", err)
			return
		}
	}
	if err := certs.CheckDNSProvider(tctx, provider, zone, cfg.BaseDomain); err != nil {
		if advice := certs.DNSAdvice(cfg.TLS.DNSProvider, zone, err); advice != "" {
			add("dns token", Fail, "%s cannot manage zone %s: %s (the provider said: %v)", cfg.TLS.DNSProvider, zone, advice, err)
			return
		}
		add("dns token", Fail, "%s cannot manage zone %s: %v; check tls.dns.api_token and that this host reaches the provider's API", cfg.TLS.DNSProvider, zone, err)
		return
	}
	add("dns token", OK, "%s wrote and removed a TXT record in zone %s", cfg.TLS.DNSProvider, zone)
}

func checkPorts(ctx context.Context, cfg config.Config, add adder) {
	addrs := []string{cfg.Listen.HTTP}
	if !cfg.Listen.ProxyMode {
		addrs = []string{cfg.Listen.HTTPS, cfg.Listen.HTTP}
	} else if !privateListener(cfg.Listen.HTTP) {
		add("ports", Warn, "listen.http %s accepts connections from any network, and proxy mode trusts the X-Forwarded-* headers they send; "+
			"bind it where only the proxy reaches it, such as 127.0.0.1:8080", cfg.Listen.HTTP)
	}
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		ln, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
		switch {
		case err == nil:
			ln.Close()
			add("ports", OK, "%s is free", addr)
		case errors.Is(err, syscall.EADDRINUSE):
			add("ports", Warn, "%s is in use (expected while Kazari is running)", addr)
		case errors.Is(err, syscall.EACCES):
			add("ports", Fail, "%s needs root or CAP_NET_BIND_SERVICE, which the systemd unit grants", addr)
		default:
			add("ports", Fail, "%s: %v", addr, err)
		}
	}
}

func checkSocket(ctx context.Context, cfg config.Config, add adder) {
	path := cfg.AdminSocketPath()
	if path == "" {
		add("admin socket", Skip, "disabled")
		return
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		add("admin socket", OK, "%s (no server listening yet)", path)
		return
	}
	conn.Close()
	add("admin socket", OK, "%s (a server is listening)", path)
}

// privateListener reports whether addr listens on a loopback or private
// address, or on localhost.
func privateListener(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if config.IsLoopbackHost(host) {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// runningInContainer looks for the marker files of Docker and Podman.
func runningInContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// checkCertFiles opens tls.cert_file and tls.key_file as the server does
// and reports the names the certificate covers and its expiry.
func checkCertFiles(cfg config.Config, now time.Time, add adder) {
	if cfg.Listen.ProxyMode || cfg.TLS.Mode != "files" {
		return
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		add("cert files", Fail, "%v; the user Kazari runs as must read both files (https://kazari.io/docs/certificates/#using-another-dns-provider)", err)
		return
	}
	leaf := pair.Leaf
	if leaf == nil {
		add("cert files", Fail, "%s holds no certificate", cfg.TLS.CertFile)
		return
	}
	names := leaf.DNSNames
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}
	var missing []string
	for _, host := range []string{cfg.BaseDomain, "api." + cfg.BaseDomain} {
		if leaf.VerifyHostname(host) != nil {
			missing = append(missing, host)
		}
	}
	date := leaf.NotAfter.UTC().Format("2006-01-02")
	switch {
	case len(missing) > 0:
		add("cert files", Fail, "%s covers %s, not %s; issue one for %s and *.%s", cfg.TLS.CertFile, strings.Join(names, ", "),
			strings.Join(missing, " or "), cfg.BaseDomain, cfg.BaseDomain)
	case !now.Before(leaf.NotAfter):
		add("cert files", Fail, "%s expired on %s; renew it, and Kazari reloads it within a minute", cfg.TLS.CertFile, date)
	case leaf.NotAfter.Sub(now) < CertWarnWindow:
		add("cert files", Warn, "%s covers %s and expires on %s; renew it", cfg.TLS.CertFile, strings.Join(names, ", "), date)
	default:
		add("cert files", OK, "%s covers %s, valid until %s", cfg.TLS.CertFile, strings.Join(names, ", "), date)
	}
}

// checkOIDC fetches the identity provider's discovery document, as the
// server does at start, and compares the issuer it names with the
// configured one, which must match exactly.
func checkOIDC(ctx context.Context, cfg config.Config, opts Options, add adder) {
	o := cfg.Auth.OIDC
	if o == nil {
		return
	}
	if err := Discover(ctx, opts.HTTPClient, o.Issuer); err != nil {
		add("oidc", Fail, "%v", err)
		return
	}
	add("oidc", OK, "%s answers discovery", o.Issuer)
}

// Discover fetches issuer's OpenID Connect discovery document and checks
// that it names issuer; its error says what to do. client nil means
// http.DefaultClient.
func Discover(ctx context.Context, client *http.Client, issuer string) error {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("auth.oidc.issuer %q: %w", issuer, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w; check auth.oidc.issuer and that this host reaches the provider", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s; check auth.oidc.issuer against the provider's issuer URL", u, resp.Status)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("%s is not a discovery document: %w; check auth.oidc.issuer", u, err)
	}
	if doc.Issuer != issuer {
		return fmt.Errorf("the provider names its issuer %q, not %q; set auth.oidc.issuer to exactly %q", doc.Issuer, issuer, doc.Issuer)
	}
	return nil
}

func randomLabel() string {
	var b [4]byte
	rand.Read(b[:])
	return "check-" + hex.EncodeToString(b[:])
}

// Describe summarises where the configuration came from, for the line
// kazari check prints before the findings.
func Describe(cfg config.Config, origin config.Origin) string {
	mode := cfg.TLS.Mode + " via " + cfg.TLS.DNSProvider
	if cfg.Listen.ProxyMode {
		mode = "proxy"
	} else if cfg.TLS.Mode != "acme" {
		mode = cfg.TLS.Mode
	}
	return fmt.Sprintf("%s, base domain %s, mode %s, data in %s", origin.Summary(), cfg.BaseDomain, mode, filepath.Clean(cfg.DataDir))
}

// Sources reports what kazari check should say about where settings came
// from: a dotenv file it could not read, variables that changed nothing,
// and every key a KAZARI_* variable set.
func Sources(origin config.Origin) []Finding {
	var out []Finding
	if origin.Unreadable != "" {
		out = append(out, Finding{Check: "env file", Status: Warn,
			Detail: origin.Unreadable + ", so it was skipped; run kazari check as root, as the service reads it, or name a readable file with --env-file"})
	}
	for _, k := range origin.Ignored {
		out = append(out, Finding{Check: "env file", Status: Warn,
			Detail: k + " in .env changed nothing: beside a configuration file, an unnamed .env only fills ${NAME} references; " +
				"name the file with --env-file, or move the line to " + config.EnvFileName + " beside the configuration file"})
	}
	for _, k := range origin.Overrides() {
		out = append(out, Finding{Check: "override", Status: OK, Detail: k + " from " + origin.Describe(k)})
	}
	return out
}
