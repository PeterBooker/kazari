package certs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"github.com/libdns/duckdns"
	"github.com/libdns/gandi"
	"github.com/libdns/hetzner"
	"github.com/libdns/namecheap"
	"github.com/libdns/ovh"
	"github.com/libdns/route53"
)

// DNSProviders are the names NewDNSProvider accepts.
var DNSProviders = []string{"cloudflare", "route53", "hetzner", "digitalocean", "gandi", "ovh", "namecheap", "duckdns"}

// DNSConfig names a DNS provider and holds its credentials. APIToken is:
//
//   - cloudflare: an API token with Zone.DNS:Edit.
//   - hetzner: a Hetzner DNS API token.
//   - route53: "ACCESS_KEY_ID:SECRET_ACCESS_KEY", or empty for the AWS
//     default credential chain.
//   - digitalocean: a personal access token with write scope.
//   - gandi: a personal access token with the "see and renew domains" and
//     "manage domain name technical configurations" permissions.
//   - ovh: "APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY"; Endpoint is
//     ovh-eu (default), ovh-ca or ovh-us.
//   - namecheap: the API key; User is the API user and ClientIP the
//     allow-listed address, discovered when empty.
//   - duckdns: the account token.
type DNSConfig struct {
	Provider string
	APIToken string
	Endpoint string
	User     string
	ClientIP string
}

// NewDNSProvider returns the libdns provider c names.
func NewDNSProvider(c DNSConfig) (certmagic.DNSProvider, error) {
	name := strings.ToLower(strings.TrimSpace(c.Provider))
	need := func() error {
		if c.APIToken == "" {
			return fmt.Errorf("certs: dns provider %s: api token is required", name)
		}
		return nil
	}
	switch name {
	case "cloudflare":
		if err := need(); err != nil {
			return nil, err
		}
		return &cloudflare.Provider{APIToken: c.APIToken}, nil
	case "hetzner":
		if err := need(); err != nil {
			return nil, err
		}
		return &hetzner.Provider{AuthAPIToken: c.APIToken}, nil
	case "route53":
		p := &route53.Provider{}
		if c.APIToken != "" {
			id, secret, ok := strings.Cut(c.APIToken, ":")
			if !ok || id == "" || secret == "" {
				return nil, errors.New("certs: dns provider route53: api token must be empty or ACCESS_KEY_ID:SECRET_ACCESS_KEY")
			}
			p.AccessKeyId, p.SecretAccessKey = id, secret
		}
		return p, nil
	case "digitalocean":
		if err := need(); err != nil {
			return nil, err
		}
		return &digitalocean.Provider{APIToken: c.APIToken}, nil
	case "gandi":
		if err := need(); err != nil {
			return nil, err
		}
		return &gandi.Provider{BearerToken: c.APIToken}, nil
	case "ovh":
		if err := need(); err != nil {
			return nil, err
		}
		parts := strings.Split(c.APIToken, ":")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, errors.New("certs: dns provider ovh: api token must be APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY")
		}
		endpoint := c.Endpoint
		if endpoint == "" {
			endpoint = "ovh-eu"
		}
		return &ovh.Provider{Endpoint: endpoint, ApplicationKey: parts[0], ApplicationSecret: parts[1], ConsumerKey: parts[2]}, nil
	case "namecheap":
		if err := need(); err != nil {
			return nil, err
		}
		if c.User == "" {
			return nil, errors.New("certs: dns provider namecheap: user is required")
		}
		return &namecheap.Provider{APIKey: c.APIToken, User: c.User, ClientIP: c.ClientIP}, nil
	case "duckdns":
		if err := need(); err != nil {
			return nil, err
		}
		return &duckdns.Provider{APIToken: c.APIToken}, nil
	}
	return nil, fmt.Errorf("certs: unknown dns provider %q (supported: %s)", c.Provider, strings.Join(DNSProviders, ", "))
}
