package certs

import (
	"strings"
	"testing"

	"github.com/libdns/cloudflare"
	"github.com/libdns/hetzner"
	"github.com/libdns/route53"
)

func TestNewDNSProvider(t *testing.T) {
	tests := []struct {
		name, provider, token string
		check                 func(t *testing.T, p any)
		wantErr               string
	}{
		{name: "cloudflare", provider: "cloudflare", token: "cf-token", check: func(t *testing.T, p any) {
			if c, ok := p.(*cloudflare.Provider); !ok || c.APIToken != "cf-token" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "case insensitive", provider: " Cloudflare ", token: "x", check: func(t *testing.T, p any) {
			if _, ok := p.(*cloudflare.Provider); !ok {
				t.Errorf("got %T", p)
			}
		}},
		{name: "hetzner", provider: "hetzner", token: "hz-token", check: func(t *testing.T, p any) {
			if h, ok := p.(*hetzner.Provider); !ok || h.AuthAPIToken != "hz-token" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 default chain", provider: "route53", check: func(t *testing.T, p any) {
			if r, ok := p.(*route53.Provider); !ok || r.AccessKeyId != "" || r.SecretAccessKey != "" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 static keys", provider: "route53", token: "AKID:secret", check: func(t *testing.T, p any) {
			if r, ok := p.(*route53.Provider); !ok || r.AccessKeyId != "AKID" || r.SecretAccessKey != "secret" {
				t.Errorf("got %#v", p)
			}
		}},
		{name: "route53 bad token", provider: "route53", token: "nocolon", wantErr: "ACCESS_KEY_ID:SECRET_ACCESS_KEY"},
		{name: "cloudflare no token", provider: "cloudflare", wantErr: "api token is required"},
		{name: "hetzner no token", provider: "hetzner", wantErr: "api token is required"},
		{name: "unknown", provider: "godaddy", token: "x", wantErr: "supported: cloudflare, route53, hetzner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewDNSProvider(tt.provider, tt.token)
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
