package certs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"github.com/libdns/duckdns"
	"github.com/libdns/gandi"
	"github.com/libdns/hetzner"
	"github.com/libdns/libdns"
	"github.com/libdns/namecheap"
	"github.com/libdns/ovh"
	"github.com/libdns/route53"
	"go.uber.org/zap"
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
			r, ok := p.(redactErrors)
			if !ok {
				t.Fatalf("got %T", p)
			}
			if d, ok := r.DNSProvider.(*duckdns.Provider); !ok || d.APIToken != "duck" || r.secret != "duck" {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubDuckDNS answers every request of the duckdns client, which calls
// http.DefaultClient against a fixed host, so no test reaches the network.
func stubDuckDNS(t *testing.T, f roundTripFunc) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = f
	t.Cleanup(func() { http.DefaultClient.Transport = old })
}

func TestDuckDNSErrorsOmitToken(t *testing.T) {
	const (
		plain   = "dummy-token-000000000000000000000000" // 36 characters, as the client requires
		escaped = "dummy+token/00000000000000000000000/" // differs from its query-escaped form
		short   = "dummy-token-of-the-wrong-length"
	)
	koReply := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("KO"))}, nil
	}
	refused := func(*http.Request) (*http.Response, error) { return nil, errors.New("stub: connection refused") }
	tests := []struct {
		name, token, reason string
		reply               roundTripFunc
	}{
		{"KO reply", plain, "got (KO)", koReply},
		{"transport error", plain, "stub: connection refused", refused},
		{"escaped token, KO reply", escaped, "got (KO)", koReply},
		{"escaped token, transport error", escaped, "stub: connection refused", refused},
		{"wrong length", short, "36 characters", koReply},
	}
	rec := libdns.TXT{Name: "_acme-challenge", Text: "value", TTL: time.Minute}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDuckDNS(t, tt.reply)
			p, err := NewDNSProvider(DNSConfig{Provider: "duckdns", APIToken: tt.token})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			_, appendErr := p.AppendRecords(ctx, "kz.duckdns.org.", []libdns.Record{rec})
			_, deleteErr := p.DeleteRecords(ctx, "kz.duckdns.org.", []libdns.Record{rec})
			checkErr := CheckDNSProvider(ctx, p, "kz.duckdns.org.", "kz.duckdns.org")

			h := &recordHandler{}
			certEvents(slog.New(h))(ctx, "cert_failed", map[string]any{"identifier": "kz.duckdns.org", "error": appendErr})
			newZapLogger(slog.New(h)).Error("could not get certificate from issuer", zap.Error(fmt.Errorf("solving: %w", appendErr)))

			texts := map[string]string{"append": fmt.Sprint(appendErr), "delete": fmt.Sprint(deleteErr), "check": fmt.Sprint(checkErr)}
			for _, r := range h.all() {
				r.Attrs(func(a slog.Attr) bool {
					texts[r.Message+" "+a.Key] = a.Value.String()
					return true
				})
			}
			for where, text := range texts {
				if strings.Contains(text, tt.token) || strings.Contains(text, url.QueryEscape(tt.token)) {
					t.Errorf("%s carries the token: %s", where, text)
				}
			}
			if appendErr == nil || !strings.Contains(appendErr.Error(), tt.reason) {
				t.Errorf("append error = %v, want the reason %q", appendErr, tt.reason)
			}
			if deleteErr == nil || !strings.Contains(deleteErr.Error(), tt.reason) {
				t.Errorf("delete error = %v, want the reason %q", deleteErr, tt.reason)
			}
			var raw *url.Error
			if errors.As(appendErr, &raw) {
				t.Errorf("the original error is reachable: %v", raw)
			}
		})
	}
}

// CertMagic stops retrying on context.Canceled, so the error must still match it.
func TestDuckDNSErrorKeepsCancellation(t *testing.T) {
	stubDuckDNS(t, func(*http.Request) (*http.Response, error) { return nil, context.Canceled })
	p, err := NewDNSProvider(DNSConfig{Provider: "duckdns", APIToken: "dummy-token-000000000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	rec := libdns.TXT{Name: "_acme-challenge", Text: "value", TTL: time.Minute}
	if _, err := p.AppendRecords(context.Background(), "kz.duckdns.org.", []libdns.Record{rec}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
