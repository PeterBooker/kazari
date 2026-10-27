package config

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
)

// Validate returns every problem joined, each prefixed by its YAML key, e.g.
// "tls.mode: must be one of acme, files, self-signed".
func (c *Config) Validate() error {
	var v validator

	v.hostname("base_domain", c.BaseDomain)
	v.required("data_dir", c.DataDir)
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

	if v.oneOf("tls.mode", c.TLS.Mode, "acme", "files", "self-signed") && !c.Listen.ProxyMode {
		switch c.TLS.Mode {
		case "acme":
			v.required("tls.email", c.TLS.Email)
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

	v.oneOf("auth.write_policy", c.Auth.WritePolicy, "shared", "owner")
	if t := c.Auth.AdminToken; t != "" && len(t) < MinAdminTokenLen && !isLocalhost(c.BaseDomain) {
		v.add("auth.admin_token", fmt.Sprintf("must be at least %d characters; generate one with openssl rand -hex 32", MinAdminTokenLen))
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

	v.oneOf("log.level", c.Log.Level, "debug", "info", "warn", "error")
	v.oneOf("log.format", c.Log.Format, "text", "json")

	return errors.Join(v.errs...)
}

// MinAdminTokenLen is the shortest auth.admin_token accepted, except under a
// localhost base domain, which only the local machine can reach.
const MinAdminTokenLen = 32

// DNSProviders are the tls.dns_provider values, as certs.NewDNSProvider
// knows them.
var DNSProviders = []string{"cloudflare", "route53", "hetzner", "digitalocean", "gandi", "ovh", "namecheap", "duckdns"}

func isLocalhost(base string) bool {
	return base == "localhost" || strings.HasSuffix(base, ".localhost")
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

func (v *validator) hostname(key, host string) {
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
