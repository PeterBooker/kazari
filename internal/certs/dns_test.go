package certs

import (
	"strings"
	"testing"

	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"github.com/libdns/duckdns"
	"github.com/libdns/gandi"
	"github.com/libdns/hetzner"
	"github.com/libdns/namecheap"
	"github.com/libdns/ovh"
	"github.com/libdns/route53"
)

func TestNewDNSProvider(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DNSConfig
		check   func(t *testing.T, p any)
		wantErr string
	}{
		{name: "cloudflare", cfg: DNSConfig{Provider: "cloudflare", APIToken: "cf-token"}, check: func(t *testing.T, p any) {
			if c, ok := p.(*cloudflare.Provider); !ok || c.APIToken != "cf-token" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "case insensitive", cfg: DNSConfig{Provider: " Cloudflare ", APIToken: "x"}, check: func(t *testing.T, p any) {
			if _, ok := p.(*cloudflare.Provider); !ok {
				t.Errorf("got %T", p)
			}
		}},
		{name: "hetzner", cfg: DNSConfig{Provider: "hetzner", APIToken: "hz-token"}, check: func(t *testing.T, p any) {
			if h, ok := p.(*hetzner.Provider); !ok || h.AuthAPIToken != "hz-token" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 default chain", cfg: DNSConfig{Provider: "route53"}, check: func(t *testing.T, p any) {
			if r, ok := p.(*route53.Provider); !ok || r.AccessKeyId != "" || r.SecretAccessKey != "" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 static keys", cfg: DNSConfig{Provider: "route53", APIToken: "AKID:secret"}, check: func(t *testing.T, p any) {
			if r, ok := p.(*route53.Provider); !ok || r.AccessKeyId != "AKID" || r.SecretAccessKey != "secret" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "digitalocean", cfg: DNSConfig{Provider: "digitalocean", APIToken: "do-token"}, check: func(t *testing.T, p any) {
			if d, ok := p.(*digitalocean.Provider); !ok || d.APIToken != "do-token" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "gandi", cfg: DNSConfig{Provider: "gandi", APIToken: "pat"}, check: func(t *testing.T, p any) {
			if g, ok := p.(*gandi.Provider); !ok || g.BearerToken != "pat" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "ovh default endpoint", cfg: DNSConfig{Provider: "ovh", APIToken: "ak:as:ck"}, check: func(t *testing.T, p any) {
			o, ok := p.(*ovh.Provider)
			if !ok || o.Endpoint != "ovh-eu" || o.ApplicationKey != "ak" || o.ApplicationSecret != "as" || o.ConsumerKey != "ck" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "ovh endpoint", cfg: DNSConfig{Provider: "ovh", APIToken: "ak:as:ck", Endpoint: "ovh-ca"}, check: func(t *testing.T, p any) {
			if o, ok := p.(*ovh.Provider); !ok || o.Endpoint != "ovh-ca" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "namecheap", cfg: DNSConfig{Provider: "namecheap", APIToken: "key", User: "ann", ClientIP: "203.0.113.5"}, check: func(t *testing.T, p any) {
			n, ok := p.(*namecheap.Provider)
			if !ok || n.APIKey != "key" || n.User != "ann" || n.ClientIP != "203.0.113.5" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "duckdns", cfg: DNSConfig{Provider: "duckdns", APIToken: "duck"}, check: func(t *testing.T, p any) {
			if d, ok := p.(*duckdns.Provider); !ok || d.APIToken != "duck" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 bad token", cfg: DNSConfig{Provider: "route53", APIToken: "nocolon"}, wantErr: "ACCESS_KEY_ID:SECRET_ACCESS_KEY"},
		{name: "ovh bad token", cfg: DNSConfig{Provider: "ovh", APIToken: "ak:as"}, wantErr: "APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY"},
		{name: "namecheap no user", cfg: DNSConfig{Provider: "namecheap", APIToken: "key"}, wantErr: "user is required"},
		{name: "cloudflare no token", cfg: DNSConfig{Provider: "cloudflare"}, wantErr: "api token is required"},
		{name: "hetzner no token", cfg: DNSConfig{Provider: "hetzner"}, wantErr: "api token is required"},
		{name: "gandi no token", cfg: DNSConfig{Provider: "gandi"}, wantErr: "api token is required"},
		{name: "unknown", cfg: DNSConfig{Provider: "godaddy", APIToken: "x"}, wantErr: "supported: cloudflare, route53, hetzner, digitalocean, gandi, ovh, namecheap, duckdns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewDNSProvider(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, p)
		})
	}
}
