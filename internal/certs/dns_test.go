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
		ok      func(p any) bool
		wantErr string
	}{
		{name: "cloudflare", cfg: DNSConfig{Provider: "cloudflare", APIToken: "cf-token"}, ok: func(p any) bool {
			c, ok := p.(*cloudflare.Provider)
			return ok && c.APIToken == "cf-token"
		}},
		{name: "case insensitive", cfg: DNSConfig{Provider: " Cloudflare ", APIToken: "x"}, ok: func(p any) bool {
			_, ok := p.(*cloudflare.Provider)
			return ok
		}},
		{name: "hetzner", cfg: DNSConfig{Provider: "hetzner", APIToken: "hz-token"}, ok: func(p any) bool {
			h, ok := p.(*hetzner.Provider)
			return ok && h.AuthAPIToken == "hz-token"
		}},
		{name: "route53 default chain", cfg: DNSConfig{Provider: "route53"}, ok: func(p any) bool {
			r, ok := p.(*route53.Provider)
			return ok && r.AccessKeyId == "" && r.SecretAccessKey == ""
		}},
		{name: "route53 static keys", cfg: DNSConfig{Provider: "route53", APIToken: "AKID:secret"}, ok: func(p any) bool {
			r, ok := p.(*route53.Provider)
			return ok && r.AccessKeyId == "AKID" && r.SecretAccessKey == "secret"
		}},
		{name: "digitalocean", cfg: DNSConfig{Provider: "digitalocean", APIToken: "do-token"}, ok: func(p any) bool {
			d, ok := p.(*digitalocean.Provider)
			return ok && d.APIToken == "do-token"
		}},
		{name: "gandi", cfg: DNSConfig{Provider: "gandi", APIToken: "pat"}, ok: func(p any) bool {
			g, ok := p.(*gandi.Provider)
			return ok && g.BearerToken == "pat"
		}},
		{name: "ovh default endpoint", cfg: DNSConfig{Provider: "ovh", APIToken: "ak:as:ck"}, ok: func(p any) bool {
			o, ok := p.(*ovh.Provider)
			return ok && o.Endpoint == "ovh-eu" && o.ApplicationKey == "ak" && o.ApplicationSecret == "as" && o.ConsumerKey == "ck"
		}},
		{name: "ovh endpoint", cfg: DNSConfig{Provider: "ovh", APIToken: "ak:as:ck", Endpoint: "ovh-ca"}, ok: func(p any) bool {
			o, ok := p.(*ovh.Provider)
			return ok && o.Endpoint == "ovh-ca"
		}},
		{name: "namecheap", cfg: DNSConfig{Provider: "namecheap", APIToken: "key", User: "ann", ClientIP: "203.0.113.5"}, ok: func(p any) bool {
			n, ok := p.(*namecheap.Provider)
			return ok && n.APIKey == "key" && n.User == "ann" && n.ClientIP == "203.0.113.5"
		}},
		{name: "duckdns", cfg: DNSConfig{Provider: "duckdns", APIToken: "duck"}, ok: func(p any) bool {
			r, ok := p.(redactErrors)
			if !ok {
				return false
			}
			d, ok := r.DNSProvider.(*duckdns.Provider)
			return ok && d.APIToken == "duck" && r.secret == "duck"
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
			if !tt.ok(p) {
				t.Errorf("NewDNSProvider(%+v) = %#v", tt.cfg, p)
			}
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
			(&ACME{}).certEvents(slog.New(h), "duckdns")(ctx, "cert_failed", map[string]any{"identifier": "kz.duckdns.org", "error": appendErr})
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
			if raw, ok := errors.AsType[*url.Error](appendErr); ok {
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
