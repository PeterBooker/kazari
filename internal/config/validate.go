package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Validate returns every problem joined, each prefixed by its YAML key, e.g.
// "tls.mode: must be one of acme, files, self-signed".
func (c *Config) Validate() error {
	var v validator

	if v.hostname("base_domain", c.BaseDomain) && isPlaceholderDomain(c.BaseDomain) {
		v.add("base_domain", c.BaseDomain+" is the example placeholder; replace it with a domain you control")
	}
	v.required("data_dir", c.DataDir)
	if e := c.TLS.Email; e != "" {
		_, domain, ok := strings.Cut(e, "@")
		switch {
		case !ok || domain == "" || strings.ContainsAny(e, " \t<>"):
			v.add("tls.email", "must be an email address, or empty for an ACME account without a contact")
		case isPlaceholderDomain(domain):
			v.add("tls.email", e+" is the example placeholder; replace it with your address, or remove it")
		}
	}
	v.oneOf("external_scheme", c.ExternalScheme, "http", "https")
	if c.ExternalPort < 0 || c.ExternalPort > 65535 {
		v.add("external_port", "must be between 0 and 65535")
	}

	if c.Listen.ProxyMode {
		if c.Listen.HTTP == "" {
			v.add("listen.http", "required when listen.proxy_mode is true")
		}
	} else {
		v.required("listen.https", c.Listen.HTTPS)
	}
	v.listenAddr("listen.http", c.Listen.HTTP)
	v.listenAddr("listen.https", c.Listen.HTTPS)
	v.listenAddr("admin.listen", c.Admin.Listen)

	if v.oneOf("tls.mode", c.TLS.Mode, "acme", "files", "self-signed") && !c.Listen.ProxyMode {
		switch c.TLS.Mode {
		case "acme":
			if c.TLS.CA != "letsencrypt" && c.TLS.CA != "letsencrypt-staging" && !isHTTPSURL(c.TLS.CA) {
				v.add("tls.ca", "must be letsencrypt, letsencrypt-staging or an https ACME directory URL")
			}
			if v.oneOf("tls.dns_provider", c.TLS.DNSProvider, DNSProviders...) {
				if c.TLS.DNSProvider != "route53" && c.TLS.DNS.APIToken == "" {
					v.add("tls.dns.api_token", "required for dns_provider "+c.TLS.DNSProvider+" (or set KAZARI_TLS_DNS_API_TOKEN)")
				}
				if c.TLS.DNSProvider == "namecheap" && c.TLS.DNS.User == "" {
					v.add("tls.dns.user", "required for dns_provider namecheap (or set KAZARI_TLS_DNS_USER)")
				}
			}
		case "files":
			v.required("tls.cert_file", c.TLS.CertFile)
			v.required("tls.key_file", c.TLS.KeyFile)
		}
	}

	v.positive("limits", c.Limits)
	if c.Limits.MaxFileBytes > c.Limits.MaxVersionBytes {
		v.add("limits.max_file_bytes", "must not exceed limits.max_version_bytes")
	}
	if c.Limits.MaxChunkBytes > c.Limits.MaxVersionBytes {
		v.add("limits.max_chunk_bytes", "must not exceed limits.max_version_bytes")
	}
	v.positive("lifecycle", c.Lifecycle)

	for _, s := range c.Serve.ReservedSlugs {
		if s == "" || s != strings.ToLower(s) {
			v.add("serve.reserved_slugs", fmt.Sprintf("%q must be a non-empty lower-case slug", s))
		}
	}
	v.oneOf("serve.default_visibility", c.Serve.DefaultVisibility, "public", "unlisted", "members")
	if c.Serve.ShareLinkMaxTTL <= 0 {
		v.add("serve.share_link_max_ttl", "must be greater than 0")
	}

	v.positive("fetch", c.Fetch)
	if c.Fetch.Budget < c.Fetch.Timeout {
		v.add("fetch.budget", "must not be shorter than fetch.timeout")
	}

	if c.Analytics.Retention < MinAnalyticsRetention {
		v.add("analytics.retention", "must be at least 24h")
	}

	v.oneOf("auth.write_policy", c.Auth.WritePolicy, "shared", "owner")
	if t := c.Auth.AdminToken; t != "" && len(t) < MinAdminTokenLen && !IsLocalhost(c.BaseDomain) {
		v.add("auth.admin_token", fmt.Sprintf("must be at least %d characters; generate one with openssl rand -hex 32", MinAdminTokenLen))
	}
	t := c.Auth.Tokens
	if t.AccessTTL <= 0 {
		v.add("auth.tokens.access_ttl", "must be greater than 0")
	}
	if t.RefreshTTL < t.AccessTTL {
		v.add("auth.tokens.refresh_ttl", "must not be shorter than auth.tokens.access_ttl")
	}
	if t.RefreshReuseWindow < 0 {
		v.add("auth.tokens.refresh_reuse_window", "must not be negative; 0 turns the window off")
	}
	if t.RefreshReuseWindow > MaxRefreshReuseWindow {
		v.add("auth.tokens.refresh_reuse_window", fmt.Sprintf("must not exceed %s", MaxRefreshReuseWindow))
	}
	if t.MaxKeyTTL < 0 {
		v.add("auth.tokens.max_key_ttl", "must not be negative; 0 sets no limit")
	}
	l := c.Auth.Local
	if l.Enabled != nil && *l.Enabled && c.Auth.OIDC != nil {
		v.add("auth.local.enabled", "must not be true with auth.oidc: people sign in with built-in accounts or the identity provider, never both")
	}
	if l.MinPasswordLength < MinPasswordLength || l.MinPasswordLength > MaxPasswordLength {
		v.add("auth.local.min_password_length", fmt.Sprintf("must be between %d and %d", MinPasswordLength, MaxPasswordLength))
	}
	if l.InviteTTL <= 0 {
		v.add("auth.local.invite_ttl", "must be greater than 0")
	}
	if l.ResetTTL <= 0 {
		v.add("auth.local.reset_ttl", "must be greater than 0")
	}
	if o := c.Auth.OIDC; o != nil {
		if !isHTTPSURL(o.Issuer) {
			v.add("auth.oidc.issuer", "must be an https URL")
		}
		v.required("auth.oidc.client_id", o.ClientID)
		if o.ClientSecret == "" {
			v.add("auth.oidc.client_secret", "required (or set KAZARI_AUTH_OIDC_CLIENT_SECRET)")
		}
	}

	if c.Admin.PublicURL != "" {
		if err := CheckAdminURL(c.Admin.PublicURL); err != nil {
			v.add("admin.public_url", err.Error())
		}
	}

	v.oneOf("log.level", c.Log.Level, logLevels...)
	v.oneOf("log.format", c.Log.Format, "text", "json")

	v.telemetry(c)

	return errors.Join(v.errs...)
}

var logLevels = []string{"debug", "info", "warn", "error"}

// MaxRefreshReuseWindow bounds auth.tokens.refresh_reuse_window: for that
// long a copied refresh token forks a connection unnoticed.
const MaxRefreshReuseWindow = Duration(5 * minute)

// MinAdminTokenLen is the shortest auth.admin_token accepted, except under a
// localhost base domain, which only the local machine can reach.
const MinAdminTokenLen = 32

// DNSProviders are the tls.dns_provider values, as certs.NewDNSProvider
// knows them.
var DNSProviders = []string{"cloudflare", "route53", "hetzner", "digitalocean", "gandi", "ovh", "namecheap", "duckdns"}

// IsLocalhost reports a localhost base domain, which only the local machine
// reaches.
func IsLocalhost(base string) bool {
	return base == "localhost" || strings.HasSuffix(base, ".localhost")
}

// CheckAdminURL reports why raw cannot be the address of the dashboard: it
// must be an origin, and plain http only on this machine, where no one else
// reads the session cookie off the wire.
func CheckAdminURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return errors.New("must be an http or https URL, such as https://admin.example.com or http://localhost:9443")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		return errors.New("must be an origin: a scheme, a host and an optional port, with no path or query")
	case u.Scheme == "http" && !IsLoopbackHost(u.Hostname()):
		return errors.New("must be https unless the host is localhost or a loopback address; put a TLS proxy in front of admin.listen")
	}
	return nil
}

// IsLoopbackHost reports whether host (without a port) only ever names this
// machine: localhost, a name under it, or a loopback IP address.
func IsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if IsLocalhost(host) {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

type validator struct {
	errs []error
}

func (v *validator) add(key, msg string) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", key, msg))
}

func (v *validator) required(key, value string) {
	if value == "" {
		v.add(key, "required")
	}
}

func (v *validator) oneOf(key, value string, allowed ...string) bool {
	if slices.Contains(allowed, value) {
		return true
	}
	v.add(key, "must be one of "+strings.Join(allowed, ", "))
	return false
}

// positive requires every integer field of s to be > 0.
func (v *validator) positive(section string, s any) {
	rv := reflect.ValueOf(s)
	t := rv.Type()
	for i := range t.NumField() {
		f := rv.Field(i)
		if f.CanInt() && f.Int() <= 0 {
			v.add(section+"."+keyName(t.Field(i)), "must be greater than 0")
		}
	}
}

// listenAddr requires host:port, which a bare port is not.
func (v *validator) listenAddr(key, addr string) {
	if addr == "" {
		return
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return
	}
	port := "8080"
	if _, err := strconv.Atoi(strings.TrimPrefix(addr, ":")); err == nil {
		port = strings.TrimPrefix(addr, ":")
	}
	v.add(key, fmt.Sprintf("%q is not host:port; write 127.0.0.1:%s for this machine only, or :%s for every address", addr, port, port))
}

// placeholderDomains are the documentation domains of RFC 2606, which the
// shipped examples use.
var placeholderDomains = []string{"example.com", "example.org", "example.net"}

// isPlaceholderDomain reports a documentation domain or a name under one.
func isPlaceholderDomain(d string) bool {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	return slices.ContainsFunc(placeholderDomains, func(p string) bool {
		return d == p || strings.HasSuffix(d, "."+p)
	})
}

// hostname reports whether host is a valid host name.
func (v *validator) hostname(key, host string) bool {
	n := len(v.errs)
	switch {
	case host == "":
		v.add(key, "required")
	case strings.Contains(host, "://"):
		v.add(key, "must not include a scheme")
	case strings.Contains(host, ":"):
		v.add(key, "must not include a port")
	case strings.HasSuffix(host, "."):
		v.add(key, "must not end with a dot")
	case host != strings.ToLower(host):
		v.add(key, "must be lower-case")
	case !isHostname(host):
		v.add(key, "must be a host name of letters, digits, hyphens and dots")
	}
	return len(v.errs) == n
}

func isHostname(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func isHTTPSURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
