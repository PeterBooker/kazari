package artifact_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// counts sums a counter's points by the given attribute keys, joined by /.
func counts(t *testing.T, r *sdkmetric.ManualReader, name string, keys ...string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				k := ""
				for i, key := range keys {
					v, _ := dp.Attributes.Value(attribute.Key(key))
					if i > 0 {
						k += "/"
					}
					k += v.String()
				}
				out[k] += dp.Value
			}
		}
	}
	return out
}

func TestArtifactMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer mp.Shutdown(context.Background())
	e := newEnv(t, artifact.Options{Telemetry: instrument.Providers{MeterProvider: mp}})
	alice := e.user(t, "alice")
	ctx := instrument.WithSurface(e.ctx, instrument.SurfaceAPI)

	e.create(t, alice, "demo", "index.html", "<h1>Hello</h1>\n")
	if _, _, err := e.svc.Update(ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("index.html", "<h1>Two</h1>\n")}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", Edits: []artifact.Edit{
		{OldText: "<h1>Two</h1>", NewText: "<h1>Three</h1>"},
		{OldText: "<h1>Three</h1>   ", NewText: "<h1>Four</h1>"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Publish(ctx, alice, "demo", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 1,
		Edits: []artifact.Edit{{OldText: "x", NewText: "y"}}}); err == nil {
		t.Fatal("an edit against a stale version succeeded")
	}

	publishes := counts(t, reader, "kazari.artifact.publishes", "kazari.publish.kind", "error.type")
	for k, want := range map[string]int64{"create/": 1, "update/": 1, "edit/": 1, "rollback/": 1, "edit/version_conflict": 1} {
		if publishes[k] != want {
			t.Errorf("publishes[%s] = %d, want %d (all %v)", k, publishes[k], want, publishes)
		}
	}
	edits := counts(t, reader, "kazari.artifact.edits", "kazari.edit.kind", "kazari.edit.match")
	if edits["text/exact"] != 1 || edits["text/trailing_whitespace"] != 1 {
		t.Errorf("edits = %v", edits)
	}
	actions := counts(t, reader, "kazari.artifact.actions", "kazari.audit.action")
	total := int64(0)
	for _, n := range actions {
		total += n
	}
	if int(total) != len(e.rec.Events) {
		t.Errorf("actions counted %d, audit rows %d (%v)", total, len(e.rec.Events), actions)
	}
	if surfaces := counts(t, reader, "kazari.artifact.actions", "kazari.surface"); surfaces["api"] == 0 {
		t.Errorf("actions by surface = %v", surfaces)
	}
}
