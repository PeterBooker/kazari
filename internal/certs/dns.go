package certs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/hetzner"
	"github.com/libdns/route53"
)

// DNSProviders lists the names NewDNSProvider accepts.
var DNSProviders = []string{"cloudflare", "route53", "hetzner"}

// NewDNSProvider returns the libdns provider called name, for the ACME
// DNS-01 challenge:
//
//   - cloudflare: apiToken is an API token with Zone.DNS:Edit.
//   - hetzner: apiToken is a Hetzner DNS API token.
//   - route53: an empty apiToken uses the AWS default credential chain
//     (environment, shared config, instance role); otherwise it must be
//     "ACCESS_KEY_ID:SECRET_ACCESS_KEY".
func NewDNSProvider(name, apiToken string) (certmagic.DNSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cloudflare":
		if apiToken == "" {
			return nil, errors.New("certs: dns provider cloudflare: api token is required")
		}
		return &cloudflare.Provider{APIToken: apiToken}, nil
	case "hetzner":
		if apiToken == "" {
			return nil, errors.New("certs: dns provider hetzner: api token is required")
		}
		return &hetzner.Provider{AuthAPIToken: apiToken}, nil
	case "route53":
		p := &route53.Provider{}
		if apiToken != "" {
			id, secret, ok := strings.Cut(apiToken, ":")
			if !ok || id == "" || secret == "" {
				return nil, errors.New("certs: dns provider route53: api token must be empty or ACCESS_KEY_ID:SECRET_ACCESS_KEY")
			}
			p.AccessKeyId, p.SecretAccessKey = id, secret
		}
		return p, nil
	}
	return nil, fmt.Errorf("certs: unknown dns provider %q (supported: %s)", name, strings.Join(DNSProviders, ", "))
}
