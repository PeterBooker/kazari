// Package preflight checks a configuration against the world before the
// first start: the data directory, the DNS records, the DNS provider token,
// the listener ports and the OpenTelemetry endpoint. kazari check prints its
// findings.
package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	Check  string // data_dir, dns, dns token, ports, admin socket, metrics, telemetry
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
	checkPorts(cfg, add)
	checkSocket(cfg, add)
	checkMetrics(cfg, add)
	if opts.Telemetry != nil {
		checkTelemetry(ctx, *opts.Telemetry, add)
	}
	return out
}

// checkMetrics warns that a scraper of the public admin host gets nothing
// until the scrape token is set, which 0.3.0 made a requirement.
func checkMetrics(cfg config.Config, add adder) {
	t := cfg.Telemetry.Metrics
	if !t.Prometheus || t.Token != "" || cfg.Admin.Listen != "" || config.IsLocalhost(cfg.BaseDomain) {
		return
	}
	add("metrics", Warn, "/metrics is not served on %s without telemetry.metrics.token; set it, or scrape admin.listen on a private address",
		cfg.Host("admin"))
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
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		add("data_dir", Fail, "%s: %v", cfg.DataDir, err)
		return
	}
	f, err := os.CreateTemp(cfg.DataDir, ".check-*")
	if err != nil {
		add("data_dir", Fail, "%s is not writable: %v", cfg.DataDir, err)
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
		if !mine {
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
		add("dns token", Fail, "%s cannot manage zone %s: %v", cfg.TLS.DNSProvider, zone, err)
		return
	}
	add("dns token", OK, "%s wrote and removed a TXT record in zone %s", cfg.TLS.DNSProvider, zone)
}

func checkPorts(cfg config.Config, add adder) {
	addrs := []string{cfg.Listen.HTTP}
	if !cfg.Listen.ProxyMode {
		addrs = []string{cfg.Listen.HTTPS, cfg.Listen.HTTP}
	}
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", addr)
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

func checkSocket(cfg config.Config, add adder) {
	path := cfg.AdminSocketPath()
	if path == "" {
		add("admin socket", Skip, "disabled")
		return
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		add("admin socket", OK, "%s (no server listening yet)", path)
		return
	}
	conn.Close()
	add("admin socket", OK, "%s (a server is listening)", path)
}

func randomLabel() string {
	var b [4]byte
	rand.Read(b[:])
	return "check-" + hex.EncodeToString(b[:])
}

// Describe summarises where the configuration came from, for the line
// kazari check prints before the findings.
func Describe(cfg config.Config, source string) string {
	mode := cfg.TLS.Mode + " via " + cfg.TLS.DNSProvider
	if cfg.Listen.ProxyMode {
		mode = "proxy"
	} else if cfg.TLS.Mode != "acme" {
		mode = cfg.TLS.Mode
	}
	if source == "" {
		source = "environment only"
	}
	return fmt.Sprintf("%s, base domain %s, mode %s, data in %s", source, cfg.BaseDomain, mode, filepath.Clean(cfg.DataDir))
}
