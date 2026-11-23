package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// RunBlobIndex checks the artifact.BlobIndex of a fresh set of stores per
// subtest.
func RunBlobIndex(t *testing.T, open func(t *testing.T) Stores) {
	suite{
		{"Blobs", testBlobs},
		{"DeleteBlobs", testDeleteBlobs},
		{"BlobTotals", testBlobTotals},
	}.run(t, open)
}

func testDeleteBlobs(t *testing.T, h *harness) {
	h.mkUser("alice")
	old, recent, kept, used := hashOf("old"), hashOf("recent"), hashOf("kept"), hashOf("used")
	for hash, created := range map[string]time.Duration{old: 0, recent: 2 * time.Hour, kept: 0} {
		if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: hash, Size: 5}}, at(created)); err != nil {
			t.Fatal(err)
		}
	}
	h.create(t, newArtifact("demo", "alice", at(0)), file("used.txt", "used"))

	n, err := h.blobs.DeleteBlobs(h.ctx, []string{old, recent, used, hashOf("unknown")}, at(time.Hour))
	if err != nil {
		t.Fatalf("DeleteBlobs: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteBlobs = %d, want 1: the old row only, not the recent, the referenced or the unknown one", n)
	}
	got, err := h.blobs.UnreferencedBlobs(h.ctx, at(3*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{kept, recent}
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("UnreferencedBlobs after DeleteBlobs (-want +got):\n%s", diff)
	}
	if referenced, err := h.blobs.BlobReferenced(h.ctx, used); err != nil || !referenced {
		t.Errorf("BlobReferenced(used) after DeleteBlobs = %v, %v; want true", referenced, err)
	}
	if n, err := h.blobs.DeleteBlobs(h.ctx, nil, at(time.Hour)); err != nil || n != 0 {
		t.Errorf("DeleteBlobs(nil) = %d, %v; want 0, nil", n, err)
	}
}

func testBlobs(t *testing.T, h *harness) {
	h.mkUser("alice")
	free1, free2, used := hashOf("free1"), hashOf("free2"), hashOf("used")
	for _, hash := range []string{free2, free1} {
		if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: hash, Size: 5}}, at(0)); err != nil {
			t.Fatal(err)
		}
	}
	h.create(t, newArtifact("demo", "alice", at(0)), file("used.txt", "used"))

	unref := func(before time.Time, limit int) []string {
		t.Helper()
		got, err := h.blobs.UnreferencedBlobs(h.ctx, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	sorted := func(s ...string) []string { slices.Sort(s); return s }

	if got := unref(at(0), 10); len(got) != 0 {
		t.Fatalf("UnreferencedBlobs before creation = %v, want none (strictly before)", got)
	}
	if diff := cmp.Diff(sorted(free1, free2), unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("UnreferencedBlobs (-want +got):\n%s", diff)
	}
	if got := unref(at(time.Hour), 1); len(got) != 1 {
		t.Fatalf("UnreferencedBlobs limit 1 returned %d", len(got))
	}

	if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: free1, Size: 5}}, at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{free2}, unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("after re-record (-want +got):\n%s", diff)
	}
	if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: free1, Size: 5}}, at(0)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{free2}, unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("after older re-record (-want +got):\n%s", diff)
	}

	for hash, want := range map[string]bool{free1: false, used: true, hashOf("unknown"): false} {
		got, err := h.blobs.BlobReferenced(h.ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("BlobReferenced(%s) = %v, want %v", hash, got, want)
		}
	}

	if err := h.blobs.DeleteBlob(h.ctx, free2); err != nil {
		t.Fatal(err)
	}
	if got := unref(at(3*time.Hour), 10); !slices.Equal(got, []string{free1}) {
		t.Fatalf("after DeleteBlob = %v, want [free1]", got)
	}
	if err := h.blobs.DeleteBlob(h.ctx, free2); err != nil {
		t.Fatalf("DeleteBlob of missing row: %v", err)
	}

	if err := h.store.Purge(h.ctx, "demo", at(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(sorted(free1, used), unref(at(3*time.Hour), 10)); diff != "" {
		t.Fatalf("after purge (-want +got):\n%s", diff)
	}
}

func testBlobTotals(t *testing.T, h *harness) {
	if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: hashOf("a"), Size: 10}, {Hash: hashOf("b"), Size: 32}}, at(0)); err != nil {
		t.Fatal(err)
	}
	if err := h.blobs.RecordBlobs(h.ctx, []artifact.StoredBlob{{Hash: hashOf("b"), Size: 32}}, at(0)); err != nil {
		t.Fatal(err)
	}
	if err := h.blobs.RecordBlobs(h.ctx, nil, at(0)); err != nil {
		t.Fatalf("RecordBlobs(nil): %v", err)
	}
	blobs, err := h.blobs.BlobTotals(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blobs.Count != 2 || blobs.Bytes != 42 {
		t.Errorf("BlobTotals = %+v, want the two recorded blobs, each once", blobs)
	}
}
