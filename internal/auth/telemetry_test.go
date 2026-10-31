package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// recognizing tells tokens of the form key_* apart, as user.Service does.
type recognizing struct{ auth.Authenticator }

func (recognizing) Recognize(token string) (string, bool) {
	return auth.SourceAPIKey, strings.HasPrefix(token, "key_")
}

func TestAuthRequestsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	opts := auth.Options{Telemetry: instrument.Providers{MeterProvider: mp}}
	chained := auth.Chain(recognizing{tokens})

	bearer := auth.RequireBearer(chained, opts)(echo)
	admin := auth.RequireAdmin("admin-token-0123456789abcdef01234", chained, opts)(echo)
	local := auth.AsLocalAdmin(opts)(echo)
	for _, c := range []struct {
		h     http.Handler
		authz string
	}{
		{bearer, ""},
		{bearer, "Bearer alice-token"},
		{bearer, "Bearer key_unknown"},
		{bearer, "Bearer garbage"},
		{admin, "Bearer alice-token"},
		{admin, "Bearer admin-token-0123456789abcdef01234"},
		{local, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.authz != "" {
			r.Header.Set("Authorization", c.authz)
		}
		c.h.ServeHTTP(httptest.NewRecorder(), r)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kazari.auth.requests" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				typ, _ := dp.Attributes.Value(instrument.CredentialType)
				res, _ := dp.Attributes.Value(instrument.AuthResult)
				got[typ.AsString()+"/"+res.AsString()] += dp.Value
			}
		}
	}
	want := map[string]int64{
		"_OTHER/missing":    1,
		"api_key/ok":        1,
		"api_key/invalid":   1,
		"_OTHER/invalid":    1,
		"api_key/forbidden": 1,
		"admin_token/ok":    1,
		"local/ok":          1,
	}
	if len(got) != len(want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestAuthSpanAttributes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())
	oauthToken := auth.AuthenticatorFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Handle: "alice", Source: auth.SourceOAuth, KeyID: "oauth:7", ClientID: "https://claude.ai/oauth/client"}, nil
	})
	h := auth.RequireBearer(oauthToken, auth.Options{})(echo)
	ctx, span := tp.Tracer("test").Start(context.Background(), "POST /")
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer t")
	h.ServeHTTP(httptest.NewRecorder(), r)
	span.End()

	attrs := map[string]string{}
	for _, kv := range rec.Ended()[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["kazari.credential.type"] != "oauth" || attrs["kazari.oauth.client.id"] != "https://claude.ai/oauth/client" {
		t.Errorf("attributes = %v", attrs)
	}
	for _, v := range attrs {
		if v == "alice" || v == "oauth:7" {
			t.Errorf("span carries the caller's identity: %v", attrs)
		}
	}
}

func TestCredentialType(t *testing.T) {
	chained := auth.Chain(tokens, recognizing{tokens})
	if got := auth.CredentialType(chained, "key_abc"); got != auth.SourceAPIKey {
		t.Errorf("CredentialType(key_abc) = %q", got)
	}
	if got := auth.CredentialType(chained, "other"); got != "" {
		t.Errorf("CredentialType(other) = %q, want empty", got)
	}
}
