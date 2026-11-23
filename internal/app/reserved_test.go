package app_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

// seedArtifacts stores artifacts in cfg's data directory with no slug
// reserved, as an instance with another list would have left them.
func seedArtifacts(t *testing.T, cfg config.Config, slugs ...string) {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, cfg.DBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES ('seed', 'member', 0, ?)`, created.UnixNano()); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewFS(cfg.BlobDir(), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc := artifact.New(sqlite.NewArtifactStore(db), blobs, nil, artifact.Options{
		MaxVersionBytes: int64(cfg.Limits.MaxVersionBytes), MaxFileBytes: int64(cfg.Limits.MaxFileBytes), MaxFiles: cfg.Limits.MaxFiles,
		DefaultVisibility: artifact.Public, Now: func() time.Time { return created },
	})
	for _, slug := range slugs {
		if _, _, err := svc.Create(ctx, artifact.Actor{Handle: "seed"}, artifact.CreateRequest{
			Slug: slug, Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader(indexHTML)}},
		}); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
}

func TestHostLabelsAlwaysReserved(t *testing.T) {
	cfg := testConfig(t)
	cfg.Serve.ReservedSlugs = []string{"foo"}
	seedArtifacts(t, cfg, "admin", "api", "mcp", "demo")
	in := start(t, cfg)
	hc := client(in.app.Addrs().HTTP)
	ctx := context.Background()

	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter", Email: "peter@example.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{Label: "reserved"})
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	user := cli.NewClient("http://api.localhost", issued.Key, hc)

	settings, err := admin.Settings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	serve, _ := settings["serve"].(map[string]any)
	if diff := cmp.Diff([]any{"foo", "api", "mcp", "admin"}, serve["reserved_slugs"]); diff != "" {
		t.Errorf("settings serve.reserved_slugs mismatch (-want +got):\n%s", diff)
	}

	for _, slug := range []string{"admin", "api", "mcp", "foo"} {
		files := map[string]string{"index.html": indexHTML}
		_, err := user.CreateBundle(ctx, api.CreateManifest{Slug: slug}, bundle(t, files))
		if !cli.IsCode(err, "invalid_slug") {
			t.Errorf("CreateBundle(%q) = %v, want invalid_slug", slug, err)
		}
	}

	for _, host := range []string{"admin--v1.localhost", "api--v1.localhost", "mcp--v1.localhost"} {
		wantStatus(t, do(t, hc, http.MethodGet, "http://"+host+"/", "", ""), http.StatusNotFound, "GET "+host)
	}
	if r := get(t, hc, "http://demo--v1.localhost/"); r.status != http.StatusOK || r.body != indexHTML {
		t.Errorf("GET demo--v1.localhost = %d %q, want 200 and the seeded page", r.status, truncate(r.body))
	}
}
