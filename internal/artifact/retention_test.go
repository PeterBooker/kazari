package artifact_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
)

// versions adds versions to slug until its latest is n, each with its own
// index.html.
func (e *env) versions(t *testing.T, actor artifact.Actor, slug string, n int, stage bool) {
	t.Helper()
	a, _, err := e.svc.Get(e.ctx, actor, slug)
	if err != nil {
		t.Fatal(err)
	}
	for i := a.LatestVersion + 1; i <= n; i++ {
		_, _, err := e.svc.Update(e.ctx, actor, artifact.UpdateRequest{Slug: slug, Stage: stage, BaseVersion: i - 1,
			Files: files("index.html", "v"+strconv.Itoa(i))})
		if err != nil {
			t.Fatalf("Update(%s) to version %d: %v", slug, i, err)
		}
	}
}

// numbers lists slug's versions, newest first.
func (e *env) numbers(t *testing.T, actor artifact.Actor, slug string) []int {
	t.Helper()
	_, vs, err := e.svc.Get(e.ctx, actor, slug)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int, len(vs))
	for i, v := range vs {
		out[i] = v.Number
	}
	return out
}

func TestDeleteVersion(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1")
	e.versions(t, alice, "demo", 4, false)
	if _, err := e.svc.Publish(e.ctx, alice, "demo", 3); err != nil {
		t.Fatal(err)
	}

	a, err := e.svc.DeleteVersion(e.ctx, alice, "demo", 2)
	if err != nil {
		t.Fatalf("DeleteVersion(2): %v", err)
	}
	if a.PublishedVersion != 3 || a.LatestVersion != 4 {
		t.Errorf("DeleteVersion(2): published %d, latest %d; want 3 and 4", a.PublishedVersion, a.LatestVersion)
	}
	if got := e.numbers(t, alice, "demo"); !slices.Equal(got, []int{4, 3, 1}) {
		t.Errorf("versions after DeleteVersion(2) = %v, want [4 3 1]", got)
	}
	last := e.rec.Events[len(e.rec.Events)-1]
	if last.Action != audit.ArtifactVersionDelete || last.Details["versions"] != "2" {
		t.Errorf("DeleteVersion(2) recorded %s %v, want %s with versions 2", last.Action, last.Details, audit.ArtifactVersionDelete)
	}

	for _, tc := range []struct {
		name    string
		version int
		want    error
		says    string
	}{
		{"the published version", 3, artifact.ErrInvalidArgument, "is published"},
		{"the latest version", 4, artifact.ErrInvalidArgument, "is the latest"},
		{"a deleted version", 2, artifact.ErrVersionNotFound, "already deleted"},
		{"a version past the latest", 9, artifact.ErrVersionNotFound, "its latest is 4"},
	} {
		_, err := e.svc.DeleteVersion(e.ctx, alice, "demo", tc.version)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("DeleteVersion(%d), %s: err = %v, want %v saying %q", tc.version, tc.name, err, tc.want, tc.says)
		}
	}

	_, err = e.svc.Publish(e.ctx, alice, "demo", 2)
	if !errors.Is(err, artifact.ErrVersionNotFound) || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("Publish(2) after deleting it: err = %v, want version_not_found saying it was deleted", err)
	}
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", BaseVersion: 2, Files: files("a.txt", "a")})
	if !errors.Is(err, artifact.ErrVersionNotFound) || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("Update on base 2 after deleting it: err = %v, want version_not_found saying it was deleted", err)
	}
}

func TestDeleteVersionNeedsDeleteOrOwnership(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	e.create(t, alice, "demo", "index.html", "v1")
	e.versions(t, alice, "demo", 3, false)

	_, err := e.svc.DeleteVersion(e.ctx, bob, "demo", 1)
	wantErr(t, "bob deleting a version of alice's page", err, artifact.ErrForbidden)
	_, _, err = e.svc.PruneVersions(e.ctx, bob, "demo", 1, true)
	wantErr(t, "bob pruning alice's page", err, artifact.ErrForbidden)

	bob.CanDelete = true
	if _, err := e.svc.DeleteVersion(e.ctx, bob, "demo", 1); err != nil {
		t.Errorf("bob with artifacts:delete deleting a version: %v", err)
	}
	restricted := alice
	restricted.Slugs = []string{"weekly-*"}
	_, err = e.svc.DeleteVersion(e.ctx, restricted, "demo", 2)
	wantErr(t, "a key restricted to other slugs", err, artifact.ErrForbidden)
}

func TestPruneVersions(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1")
	e.versions(t, alice, "demo", 4, false)
	if _, err := e.svc.Publish(e.ctx, alice, "demo", 2); err != nil {
		t.Fatal(err)
	}
	// Staged after the rollback, so newer than the published version 2.
	e.versions(t, alice, "demo", 6, true)

	_, planned, err := e.svc.PruneVersions(e.ctx, alice, "demo", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]int{1, 3, 4}, planned); diff != "" {
		t.Errorf("PruneVersions(keep 1, dry run) (-want +got):\n%s", diff)
	}
	if got := e.numbers(t, alice, "demo"); len(got) != 6 {
		t.Errorf("versions after a dry run = %v, want all six", got)
	}

	_, deleted, err := e.svc.PruneVersions(e.ctx, alice, "demo", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]int{1, 3, 4}, deleted); diff != "" {
		t.Errorf("PruneVersions(keep 1) (-want +got):\n%s", diff)
	}
	// 6 is the newest, 5 is staged and waits for review, 2 is published.
	if got := e.numbers(t, alice, "demo"); !slices.Equal(got, []int{6, 5, 2}) {
		t.Errorf("versions after PruneVersions(keep 1) = %v, want [6 5 2]", got)
	}
	if last := e.rec.Events[len(e.rec.Events)-1]; last.Details["versions"] != "1,3,4" {
		t.Errorf("PruneVersions recorded %v, want versions 1,3,4", last.Details)
	}

	_, deleted, err = e.svc.PruneVersions(e.ctx, alice, "demo", 1, false)
	if err != nil || len(deleted) != 0 {
		t.Errorf("PruneVersions with nothing to prune = %v, %v; want none", deleted, err)
	}
	for _, keep := range []int{0, -1, artifact.MaxKeepVersions + 1} {
		_, _, err := e.svc.PruneVersions(e.ctx, alice, "demo", keep, false)
		wantErr(t, "PruneVersions(keep "+strconv.Itoa(keep)+")", err, artifact.ErrInvalidArgument)
	}
}

func TestSweepKeepsVersions(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	if _, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo", Files: files("index.html", "v1"),
		Metadata: map[string]string{artifact.MetaKeepVersions: "2"}}); err != nil {
		t.Fatal(err)
	}
	e.versions(t, alice, "demo", 5, false)
	e.create(t, alice, "other", "index.html", "v1")
	e.versions(t, alice, "other", 5, false)

	rep, err := e.svc.Sweep(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.VersionsDeleted != 3 {
		t.Errorf("Sweep VersionsDeleted = %d, want 3", rep.VersionsDeleted)
	}
	if got := e.numbers(t, alice, "demo"); !slices.Equal(got, []int{5, 4}) {
		t.Errorf("demo after Sweep = %v, want [5 4]", got)
	}
	if got := e.numbers(t, alice, "other"); len(got) != 5 {
		t.Errorf("other, without keep_versions, after Sweep = %v, want all five", got)
	}
	last := e.rec.Events[len(e.rec.Events)-1]
	if last.Actor != "scheduler" || last.Action != audit.ArtifactVersionDelete || last.Details["reason"] != "keep_versions" {
		t.Errorf("Sweep recorded %s by %s %v, want %s by scheduler for keep_versions", last.Action, last.Actor, last.Details, audit.ArtifactVersionDelete)
	}
	if rep, err := e.svc.Sweep(e.ctx); err != nil || rep.VersionsDeleted != 0 {
		t.Errorf("second Sweep = %+v, %v; want nothing deleted", rep, err)
	}
}

func TestKeepVersionsMetadata(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"1", true},
		{"10", true},
		{strconv.Itoa(artifact.MaxKeepVersions), true},
		{"0", false},
		{"-3", false},
		{"+3", false},
		{"03", false},
		{"ten", false},
		{strconv.Itoa(artifact.MaxKeepVersions + 1), false},
	} {
		err := artifact.ValidateMetadataPair(artifact.MetaKeepVersions, tc.value)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateMetadataPair(%s, %q) = %v, want ok %t", artifact.MetaKeepVersions, tc.value, err, tc.ok)
		}
	}
}

// Setting kazari.keep_versions has the sweep delete versions, so it takes
// what deleting them takes.
func TestKeepVersionsNeedsDeleteOrOwnership(t *testing.T) {
	e := newEnv(t, artifact.Options{WritePolicy: artifact.WriteShared})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	e.create(t, alice, "demo", "index.html", "v1")
	e.versions(t, alice, "demo", 5, false)

	keep := map[string]*string{artifact.MetaKeepVersions: new("1")}
	_, _, err := e.svc.Update(e.ctx, bob, artifact.UpdateRequest{Slug: "demo", Metadata: keep})
	wantErr(t, "bob setting keep_versions on alice's page", err, artifact.ErrForbidden)
	_, _, err = e.svc.Update(e.ctx, bob, artifact.UpdateRequest{Slug: "demo", Metadata: keep, BaseVersion: 5,
		Files: files("index.html", "v6")})
	wantErr(t, "bob setting keep_versions with a new version", err, artifact.ErrForbidden)
	_, _, err = e.svc.Update(e.ctx, bob, artifact.UpdateRequest{Slug: "demo", Metadata: map[string]*string{artifact.MetaKeepVersions: nil}})
	wantErr(t, "bob removing keep_versions", err, artifact.ErrForbidden)
	if _, err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.numbers(t, alice, "demo"); len(got) != 5 {
		t.Errorf("versions after bob's refused keep_versions and a sweep = %v, want all five", got)
	}

	for _, who := range []struct {
		name  string
		actor artifact.Actor
	}{
		{"the owner", alice},
		{"bob with artifacts:delete", artifact.Actor{Handle: "bob", CanDelete: true}},
	} {
		if _, _, err := e.svc.Update(e.ctx, who.actor, artifact.UpdateRequest{Slug: "demo", Metadata: keep}); err != nil {
			t.Errorf("%s setting keep_versions: %v", who.name, err)
		}
	}
}
