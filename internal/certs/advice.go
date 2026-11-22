package certs

import (
	"regexp"
	"strings"
)

var (
	refusedStatus   = regexp.MustCompile(`\b(400|401)\b`)
	forbiddenStatus = regexp.MustCompile(`\b403\b`)
)

// tokenPermissions is what each provider's token needs, as docs/certificates.md
// lists it.
var tokenPermissions = map[string]string{
	"cloudflare":   "an API token with Zone.DNS:Edit on the zone",
	"route53":      "AWS credentials allowed to list hosted zones and change their record sets",
	"hetzner":      "a Hetzner DNS API token (not a Hetzner Cloud token)",
	"digitalocean": "a personal access token with write scope",
	"gandi":        "a personal access token allowed to manage the domain's technical configuration",
	"ovh":          "APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY, the consumer key with rights on /domain/zone/*",
	"namecheap":    "the API key, with API access on and this server's IP address allow-listed",
	"duckdns":      "the account token",
}

// DNSAdvice says in plain words what to do about an error from the DNS
// provider, such as "HTTP 400: Invalid format for Authorization header": the
// next step for a refused token, a token without permission and a zone the
// account does not hold. It returns "" for any other error.
func DNSAdvice(provider, zone string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	need := tokenPermissions[provider]
	if need == "" {
		need = "permission to add and delete records in the zone"
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(msg, w) {
				return true
			}
		}
		return false
	}
	z := strings.TrimSuffix(zone, ".")
	if z == "" {
		z = "of the base domain"
	}
	switch {
	case has("zone not found", "no zone", "could not find zone", "unknown zone", "zone does not exist", "no hosted zone", "hostedzonenotfound"):
		return "the provider's account has no zone " + z +
			": check that the token belongs to the account holding it, and that tls.dns_provider names the provider serving it"
	case forbiddenStatus.MatchString(msg) || has("forbidden", "permission", "not authorized to perform", "accessdenied", "access denied"):
		return "the token lacks permission on zone " + z + "; " + provider + " needs " + need
	case refusedStatus.MatchString(msg) || has("unauthorized", "unauthenticated", "authentication", "authorization header", "invalid token",
		"invalid api key", "invalid credentials", "invalidclienttokenid", "signaturedoesnotmatch"):
		return "the provider refused the token: check that tls.dns.api_token (KAZARI_TLS_DNS_API_TOKEN) holds the whole token, " +
			"without quotes or spaces, and that it has not expired or been revoked; " + provider + " needs " + need
	}
	return ""
}

// ObtainHint says what to do about a failed certificate order, by the kind
// of error: the DNS provider refused the token, the CA cannot be reached, a
// CAA record forbids the CA, or the CA's rate limit was hit.
func ObtainHint(provider string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(msg, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("caa"):
		return "a CAA record on the domain forbids this CA: add one allowing letsencrypt.org, or remove the CAA records; retries continue in the background"
	case has("ratelimited", "rate limit", "too many certificates", "too many failed authorizations", "too many new orders"):
		return "the CA's rate limit was hit: wait for the time the error names, and test with tls.ca: letsencrypt-staging meanwhile; retries continue in the background"
	case has("presenting", "temporary record", "appendrecords", "append records", "zone") && DNSAdvice(provider, "", err) != "":
		return DNSAdvice(provider, "", err) + "; kazari check tests the token; retries continue in the background"
	case has("directory", "acme-v02", "acme-staging", "/acme/") && has("dial tcp", "no such host", "connection refused", "i/o timeout",
		"tls handshake timeout", "network is unreachable", "context deadline exceeded", "eof"):
		return "the CA cannot be reached: check that this host makes outbound HTTPS connections, through any firewall or proxy; retries continue in the background"
	}
	return "check the DNS records and the DNS API token with kazari check; retries continue in the background"
}
