package app_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/cli"
)

const (
	plantedWorker = "self.onfetch = () => {}"
	ownWorker     = "self.oninstall = () => {}"
)

// member creates a user and returns a client with that user's key.
func member(t *testing.T, ctx context.Context, admin *cli.Client, addr, handle string) *cli.Client {
	t.Helper()
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: handle}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, handle, api.IssueKeyRequest{Label: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return cli.NewClient("http://api.localhost", issued.Key, client(addr))
}

// updateCheck asks for a worker script the way a browser does, past any
// worker it holds.
func updateCheck(t *testing.T, addr, slug string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+slug+".localhost/sw.js", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Service-Worker", "script")
	resp, err := client(addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func wantWorker(t *testing.T, addr, slug, want, what string) {
	t.Helper()
	if got := updateCheck(t, addr, slug); got != want {
		t.Fatalf("%s: update check answered %q, want %q", what, got, want)
	}
}

func wantEvicted(t *testing.T, addr, slug, what string) {
	t.Helper()
	if got := updateCheck(t, addr, slug); !strings.Contains(got, "registration.unregister()") {
		t.Fatalf("%s: update check answered %q, not the eviction script", what, got)
	}
}

func publishWorker(t *testing.T, ctx context.Context, c *cli.Client, slug string) {
	t.Helper()
	_, err := c.Create(ctx, api.CreateArtifactRequest{Slug: slug, Files: []api.FileInput{
		{Path: "index.html", Content: "page"},
		{Path: "sw.js", Content: plantedWorker},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// Disabling a member ends the worker they uploaded, on an artifact someone
// else carries on with, until that person replaces the script.
func TestWorkerEndsWithItsAuthor(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	mallory, alice := member(t, ctx, admin, addr, "mallory"), member(t, ctx, admin, addr, "alice")

	publishWorker(t, ctx, mallory, "pwa")
	wantWorker(t, addr, "pwa", plantedWorker, "author enabled")

	disabled := true
	if _, err := admin.UpdateUser(ctx, "mallory", api.UpdateUserRequest{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	wantEvicted(t, addr, "pwa", "author disabled")

	// A new version that carries the same script on is still theirs.
	_, err := alice.Update(ctx, "pwa", api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "index.html", Content: "new page"}}})
	if err != nil {
		t.Fatal(err)
	}
	wantEvicted(t, addr, "pwa", "script carried into another's version")

	_, err = alice.Update(ctx, "pwa", api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "sw.js", Content: ownWorker}}})
	if err != nil {
		t.Fatal(err)
	}
	wantWorker(t, addr, "pwa", ownWorker, "script replaced")
}

// Under the owner policy a transfer takes the artifact from its author. A
// shared artifact loses nobody's control that way.
func TestWorkerEndsWithTransfer(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		evicted bool
	}{
		{artifact.WriteOwner, true},
		{artifact.WriteShared, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Auth.WritePolicy = tc.policy
			in := start(t, cfg)
			addr := in.app.Addrs().HTTP
			ctx := context.Background()
			admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
			mallory := member(t, ctx, admin, addr, "mallory")
			member(t, ctx, admin, addr, "victim")

			publishWorker(t, ctx, mallory, "pwa")
			wantWorker(t, addr, "pwa", plantedWorker, "before the transfer")
			if _, err := admin.Transfer(ctx, "pwa", "victim"); err != nil {
				t.Fatal(err)
			}
			if tc.evicted {
				wantEvicted(t, addr, "pwa", "after the transfer")
			} else {
				wantWorker(t, addr, "pwa", plantedWorker, "after the transfer")
			}
		})
	}
}

// A purge leaves nothing at the script's URL, so a browser that returns
// before the slug is reused drops the worker.
func TestWorkerEndsWithPurge(t *testing.T) {
	in := start(t, testConfig(t))
	addr := in.app.Addrs().HTTP
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, client(addr))
	mallory := member(t, ctx, admin, addr, "mallory")

	publishWorker(t, ctx, mallory, "pwa")
	if err := mallory.Delete(ctx, "pwa", true); err != nil {
		t.Fatal(err)
	}
	wantEvicted(t, addr, "pwa", "purged")
}
