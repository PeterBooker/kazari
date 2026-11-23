package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// RunBlobIndex checks the artifact.BlobIndex of a fresh set of stores per
// subtest.
func RunBlobIndex(t *testing.T, open func(t *testing.T) Stores) {
	suite{
		{"Blobs", testBlobs},
		{"BlobTotals", testBlobTotals},
	}.run(t, open)
}

func testBlobs(t *testing.T, h *harness) {
	h.mkUser("alice")
	free1, free2, used := hashOf("free1"), hashOf("free2"), hashOf("used")
	for _, hash := range []string{free2, free1} {
		if err := h.blobs.RecordBlob(h.ctx, hash, 5, at(0)); err != nil {
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

	if err := h.blobs.RecordBlob(h.ctx, free1, 5, at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{free2}, unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("after re-record (-want +got):\n%s", diff)
	}
	if err := h.blobs.RecordBlob(h.ctx, free1, 5, at(0)); err != nil {
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
	if err := h.blobs.RecordBlob(h.ctx, hashOf("a"), 10, at(0)); err != nil {
		t.Fatal(err)
	}
	if err := h.blobs.RecordBlob(h.ctx, hashOf("b"), 32, at(0)); err != nil {
		t.Fatal(err)
	}
	blobs, err := h.blobs.BlobTotals(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if blobs.Count < 2 || blobs.Bytes < 42 {
		t.Errorf("BlobTotals = %+v, want at least the two recorded blobs", blobs)
	}
}
