package certs

import (
	"errors"
	"log/slog"
	"testing"

	"go.uber.org/zap"
)

func TestZapLoggerForwardsToSlog(t *testing.T) {
	h := &recordHandler{}
	zl := newZapLogger(slog.New(h)).Named("certmagic").With(zap.String("issuer", "acme"))
	zl.Warn("renewing", zap.Strings("identifiers", []string{"a.test"}), zap.Error(errors.New("boom")))
	zl.Debug("detail")

	recs := h.all()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	r := recs[0]
	if r.Message != "renewing" || r.Level != slog.LevelWarn {
		t.Errorf("record = %q at %v", r.Message, r.Level)
	}
	got := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.String()
		return true
	})
	want := map[string]string{"logger": "certmagic", "issuer": "acme", "error": "boom", "identifiers": "[a.test]"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if recs[1].Level != slog.LevelDebug {
		t.Errorf("debug level = %v", recs[1].Level)
	}
}
