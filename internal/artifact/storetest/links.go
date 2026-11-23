package storetest

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// RunLinks checks the artifact.LinkStore of a fresh set of stores per
// subtest.
func RunLinks(t *testing.T, open func(t *testing.T) Stores) {
	suite{
		{"ShareLinks", testShareLinks},
		{"UserShareLinks", testUserShareLinks},
		{"ShareLinksFollowTheRow", testShareLinksFollowTheRow},
	}.run(t, open)
}

func testShareLinks(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	h.create(t, newArtifact("demo", "alice", at(0)))
	h.create(t, newArtifact("other", "bob", at(0)))

	l1 := &artifact.ShareLink{Prefix: "aaaaaaaa", Label: "for the client", CreatedBy: "alice",
		CreatedAt: at(time.Hour), ExpiresAt: at(48 * time.Hour)}
	l2 := &artifact.ShareLink{Prefix: "bbbbbbbb", CreatedBy: "bob", CreatedAt: at(2 * time.Hour), ExpiresAt: at(72 * time.Hour),
		HasPassword: true}
	if err := h.links.CreateShareLink(h.ctx, "demo", l1, hashOf("token-1"), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.links.CreateShareLink(h.ctx, "demo", l2, hashOf("token-2"), "$argon2id$stored"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "CreateShareLink on a missing slug",
		h.links.CreateShareLink(h.ctx, "missing", &artifact.ShareLink{Prefix: "cccccccc", CreatedAt: at(0), ExpiresAt: at(time.Hour)}, hashOf("t"), ""),
		artifact.ErrNotFound)

	links, err := h.links.ListShareLinks(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]artifact.ShareLink{*l2, *l1}, links, cmpOpts...); diff != "" {
		t.Fatalf("ListShareLinks, newest first (-want +got):\n%s", diff)
	}
	if links, err := h.links.ListShareLinks(h.ctx, "other"); err != nil || len(links) != 0 {
		t.Fatalf("ListShareLinks(other) = %v, %v; want none", links, err)
	}

	got, passwordHash, err := h.links.ShareLink(h.ctx, "demo", hashOf("token-1"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(l1, got, cmpOpts...); diff != "" || passwordHash != "" {
		t.Fatalf("ShareLink, password hash %q (-want +got):\n%s", passwordHash, diff)
	}
	if got, passwordHash, err := h.links.ShareLink(h.ctx, "demo", hashOf("token-2")); err != nil || !got.HasPassword ||
		passwordHash != "$argon2id$stored" {
		t.Fatalf("ShareLink with a password = %+v, %q, %v; want HasPassword and the stored hash", got, passwordHash, err)
	}
	_, _, err = h.links.ShareLink(h.ctx, "other", hashOf("token-1"))
	wantErr(t, "ShareLink of another artifact", err, artifact.ErrLinkNotFound)
	_, _, err = h.links.ShareLink(h.ctx, "demo", hashOf("unknown"))
	wantErr(t, "ShareLink unknown", err, artifact.ErrLinkNotFound)

	for i := range 2 {
		if err := h.links.UseShareLink(h.ctx, "demo", l1.Prefix, at(time.Duration(3+i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, "UseShareLink through another artifact", h.links.UseShareLink(h.ctx, "other", l1.Prefix, at(0)), artifact.ErrLinkNotFound)
	if got, _, _ = h.links.ShareLink(h.ctx, "demo", hashOf("token-1")); got.Uses != 2 || !got.LastUsedAt.Equal(at(4*time.Hour)) {
		t.Fatalf("after two uses: Uses %d, LastUsedAt %v", got.Uses, got.LastUsedAt)
	}

	if err := h.links.RevokeShareLink(h.ctx, "demo", l1.Prefix, at(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A second revoke keeps the first time.
	if err := h.links.RevokeShareLink(h.ctx, "demo", l1.Prefix, at(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = h.links.ShareLink(h.ctx, "demo", hashOf("token-1")); got.RevokedAt == nil || !got.RevokedAt.Equal(at(5*time.Hour)) {
		t.Fatalf("RevokedAt = %v, want %v", got.RevokedAt, at(5*time.Hour))
	}
	wantErr(t, "RevokeShareLink through another artifact",
		h.links.RevokeShareLink(h.ctx, "other", l2.Prefix, at(0)), artifact.ErrLinkNotFound)
	wantErr(t, "RevokeShareLink unknown", h.links.RevokeShareLink(h.ctx, "demo", "zzzzzzzz", at(0)), artifact.ErrLinkNotFound)
}

// A user's links end together, on every artifact and only theirs, and a link
// revoked earlier keeps its revoke time.
func testUserShareLinks(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	h.create(t, newArtifact("demo", "alice", at(0)))
	h.create(t, newArtifact("other", "bob", at(0)))
	for i, l := range []struct{ slug, prefix, by string }{
		{"demo", "aaaaaaaa", "alice"}, {"other", "bbbbbbbb", "alice"},
		{"demo", "cccccccc", "alice"}, {"demo", "dddddddd", "bob"},
	} {
		link := &artifact.ShareLink{Prefix: l.prefix, CreatedBy: l.by, CreatedAt: at(0), ExpiresAt: at(time.Hour)}
		if err := h.links.CreateShareLink(h.ctx, l.slug, link, hashOf(fmt.Sprintf("token-%d", i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.links.RevokeShareLink(h.ctx, "demo", "cccccccc", at(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := h.links.RevokeUserShareLinks(h.ctx, "alice", at(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b artifact.RevokedShareLink) int { return strings.Compare(a.Prefix, b.Prefix) })
	want := []artifact.RevokedShareLink{{Slug: "demo", Prefix: "aaaaaaaa"}, {Slug: "other", Prefix: "bbbbbbbb"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("RevokeUserShareLinks (-want +got):\n%s", diff)
	}
	for i, wantAt := range []*time.Time{new(at(2 * time.Minute)), new(at(2 * time.Minute)), new(at(time.Minute)), nil} {
		slug := "demo"
		if i == 1 {
			slug = "other"
		}
		l, _, err := h.links.ShareLink(h.ctx, slug, hashOf(fmt.Sprintf("token-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(wantAt, l.RevokedAt); diff != "" {
			t.Errorf("link %s RevokedAt (-want +got):\n%s", l.Prefix, diff)
		}
	}

	// Nothing is left to revoke, and a handle with no links is not an error.
	for _, handle := range []string{"alice", "nobody"} {
		if got, err := h.links.RevokeUserShareLinks(h.ctx, handle, at(3*time.Minute)); err != nil || len(got) != 0 {
			t.Fatalf("RevokeUserShareLinks(%s) = %v, %v; want none", handle, got, err)
		}
	}
}

// A link belongs to its artifact row: it outlives a purge, goes with the
// tombstone, and never opens a later artifact that reuses the slug.
func testShareLinksFollowTheRow(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	h.create(t, newArtifact("demo", "alice", at(0)))
	l := &artifact.ShareLink{Prefix: "aaaaaaaa", CreatedBy: "alice", CreatedAt: at(0), ExpiresAt: at(1000 * time.Hour)}
	if err := h.links.CreateShareLink(h.ctx, "demo", l, hashOf("token"), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Purge(h.ctx, "demo", at(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.links.ShareLink(h.ctx, "demo", hashOf("token")); err != nil {
		t.Fatalf("ShareLink on a tombstone: %v", err)
	}
	if err := h.store.DeleteTombstone(h.ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	h.create(t, newArtifact("demo", "bob", at(2*time.Hour)))
	_, _, err := h.links.ShareLink(h.ctx, "demo", hashOf("token"))
	wantErr(t, "old link on the reused slug", err, artifact.ErrLinkNotFound)
	if links, err := h.links.ListShareLinks(h.ctx, "demo"); err != nil || len(links) != 0 {
		t.Fatalf("ListShareLinks on the reused slug = %v, %v; want none", links, err)
	}
}
