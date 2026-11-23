package artifact_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

func TestValidateSlugPattern(t *testing.T) {
	long := strings.Repeat("a", artifact.SlugMaxLen)
	for _, tt := range []struct {
		pattern string
		ok      bool
	}{
		{"weekly-report", true},
		{"abc", true},
		{long, true},
		{"reports-*", true},
		{"reports*", true},
		{"a*", true},
		{"2026-*", true},
		{long + "*", true},
		{"", false},
		{"*", false},
		{"ab", false},
		{long + "a", false},
		{long + "a*", false},
		{"-reports*", false},
		{"reports--*", false},
		{"Reports-*", false},
		{"reports-**", false},
		{"*-reports", false},
		{"re*ports", false},
		{"reports-?", false},
		{"reports_*", false},
		{"reports-%", false},
		{"reports *", false},
		{"weekly-", false},
	} {
		err := artifact.ValidateSlugPattern(tt.pattern)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateSlugPattern(%q) = %v, want ok = %t", tt.pattern, err, tt.ok)
		}
		if err != nil && !errors.Is(err, artifact.ErrInvalidSlug) {
			t.Errorf("ValidateSlugPattern(%q) = %v, want ErrInvalidSlug", tt.pattern, err)
		}
	}

	many := make([]string, artifact.MaxSlugPatterns)
	for i := range many {
		many[i] = fmt.Sprintf("page-%d", i)
	}
	if err := artifact.ValidateSlugPatterns(many); err != nil {
		t.Errorf("%d patterns: %v", len(many), err)
	}
	if err := artifact.ValidateSlugPatterns(append(many, "one-more")); !errors.Is(err, artifact.ErrInvalidArgument) {
		t.Errorf("%d patterns: %v, want ErrInvalidArgument", len(many)+1, err)
	}
	if err := artifact.ValidateSlugPatterns([]string{"fine", "*"}); !errors.Is(err, artifact.ErrInvalidSlug) {
		t.Errorf("a bad pattern among good ones: %v, want ErrInvalidSlug", err)
	}
}

func TestMatchSlug(t *testing.T) {
	for _, tt := range []struct {
		patterns []string
		slug     string
		want     bool
	}{
		{nil, "anything", true},
		{[]string{}, "anything", true},
		{[]string{"weekly-report"}, "weekly-report", true},
		{[]string{"weekly-report"}, "weekly-report-2", false},
		{[]string{"weekly-report"}, "weekly", false},
		{[]string{"weekly-*"}, "weekly-report", true},
		{[]string{"weekly-*"}, "weekly-", true},
		{[]string{"weekly-*"}, "weekly", false},
		{[]string{"weekly-*"}, "biweekly-report", false},
		{[]string{"weekly*"}, "weekly", true},
		{[]string{"other", "weekly-*"}, "weekly-a", true},
		{[]string{"other", "weekly-*"}, "others", false},
	} {
		if got := artifact.MatchSlug(tt.patterns, tt.slug); got != tt.want {
			t.Errorf("MatchSlug(%v, %q) = %t, want %t", tt.patterns, tt.slug, got, tt.want)
		}
	}
}

func TestSlugPatternsWithin(t *testing.T) {
	for _, tt := range []struct {
		inner, outer []string
		want         bool
	}{
		{nil, nil, true},
		{[]string{"weekly-*"}, nil, true},
		{nil, []string{"weekly-*"}, false},
		{[]string{"weekly-report"}, []string{"weekly-report"}, true},
		{[]string{"weekly-report"}, []string{"weekly-*"}, true},
		{[]string{"weekly-2026-*"}, []string{"weekly-*"}, true},
		{[]string{"weekly-*"}, []string{"weekly-*"}, true},
		{[]string{"weekly-*"}, []string{"weekly-report"}, false},
		{[]string{"week*"}, []string{"weekly-*"}, false},
		{[]string{"weekly"}, []string{"weekly-*"}, false},
		{[]string{"weekly-a", "other"}, []string{"weekly-*"}, false},
		{[]string{"weekly-a", "other"}, []string{"weekly-*", "other"}, true},
		{[]string{"other-*"}, []string{"weekly-*", "other"}, false},
	} {
		if got := artifact.SlugPatternsWithin(tt.inner, tt.outer); got != tt.want {
			t.Errorf("SlugPatternsWithin(%v, %v) = %t, want %t", tt.inner, tt.outer, got, tt.want)
		}
	}
}

// restricted reports whether err is the refusal of a slug outside an
// actor's restriction.
func restricted(err error) bool {
	return errors.Is(err, artifact.ErrForbidden) && strings.Contains(err.Error(), "restricted to slugs matching weekly-*")
}

// slugOps is every Service method that takes an actor and a slug. fresh
// says the method creates the slug rather than acting on an existing one.
var slugOps = []struct {
	name  string
	fresh bool
	run   func(e *env, actor artifact.Actor, slug string) error
}{
	{"Create", true, func(e *env, actor artifact.Actor, slug string) error {
		_, _, err := e.svc.Create(e.ctx, actor, artifact.CreateRequest{Slug: slug, Files: files("index.html", "x")})
		return err
	}},
	{"Import", true, func(e *env, actor artifact.Actor, slug string) error {
		_, _, _, err := e.svc.Import(e.ctx, actor, artifact.ImportRequest{URL: "https://example.test/", Slug: slug})
		return err
	}},
	{"Get", false, func(e *env, actor artifact.Actor, slug string) error {
		_, _, err := e.svc.Get(e.ctx, actor, slug)
		return err
	}},
	{"OpenFile", false, func(e *env, actor artifact.Actor, slug string) error {
		_, rc, err := e.svc.OpenFile(e.ctx, actor, slug, "index.html", 0)
		if err == nil {
			rc.Close()
		}
		return err
	}},
	{"Update fields", false, func(e *env, actor artifact.Actor, slug string) error {
		_, _, err := e.svc.Update(e.ctx, actor, artifact.UpdateRequest{Slug: slug, Name: new("renamed")})
		return err
	}},
	{"Update files", false, func(e *env, actor artifact.Actor, slug string) error {
		_, _, err := e.svc.Update(e.ctx, actor, artifact.UpdateRequest{Slug: slug, Files: files("index.html", "changed")})
		return err
	}},
	{"Edit", false, func(e *env, actor artifact.Actor, slug string) error {
		_, _, _, err := e.svc.Edit(e.ctx, actor, artifact.EditRequest{Slug: slug, Path: "index.html",
			Edits: []artifact.Edit{{OldText: "<h1>", NewText: "<h2>"}}})
		return err
	}},
	{"Publish", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.Publish(e.ctx, actor, slug, 1)
		return err
	}},
	{"MarkStale", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.MarkStale(e.ctx, actor, slug, time.Hour)
		return err
	}},
	{"UnmarkStale", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.UnmarkStale(e.ctx, actor, slug)
		return err
	}},
	{"SetVisibility", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.SetVisibility(e.ctx, actor, slug, string(artifact.Unlisted))
		return err
	}},
	{"Transfer", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.Transfer(e.ctx, actor, slug, "bob")
		return err
	}},
	{"Delete", false, func(e *env, actor artifact.Actor, slug string) error {
		return e.svc.Delete(e.ctx, actor, slug, false)
	}},
	{"Delete hard", false, func(e *env, actor artifact.Actor, slug string) error {
		return e.svc.Delete(e.ctx, actor, slug, true)
	}},
	{"CreateShareLink", false, func(e *env, actor artifact.Actor, slug string) error {
		_, _, err := e.svc.CreateShareLink(e.ctx, actor, slug, "", time.Hour, "")
		return err
	}},
	{"ListShareLinks", false, func(e *env, actor artifact.Actor, slug string) error {
		_, err := e.svc.ListShareLinks(e.ctx, actor, slug)
		return err
	}},
	{"RevokeShareLink", false, func(e *env, actor artifact.Actor, slug string) error {
		err := e.svc.RevokeShareLink(e.ctx, actor, slug, "aaaaaaaa")
		if errors.Is(err, artifact.ErrLinkNotFound) {
			return nil // past every check: the artifact has no such link
		}
		return err
	}},
}

func TestRestriction(t *testing.T) {
	for _, policy := range []string{artifact.WriteShared, artifact.WriteOwner} {
		t.Run(policy, func(t *testing.T) {
			e := newEnv(t, artifact.Options{WritePolicy: policy, Pages: &fakePages{}})
			e.user(t, "bob")
			// An admin, so that Transfer passes everything but the restriction.
			alice := e.user(t, "alice")
			alice.Admin = true
			narrow := alice
			narrow.Slugs = []string{"weekly-*"}

			for i, op := range slugOps {
				t.Run(op.name, func(t *testing.T) {
					inside, outside := fmt.Sprintf("weekly-%d", i), fmt.Sprintf("other-%d", i)
					if !op.fresh {
						e.create(t, alice, inside)
						e.create(t, alice, outside)
					}
					events := len(e.rec.Events)
					if err := op.run(e, narrow, outside); !restricted(err) {
						t.Errorf("restricted, slug outside: err = %v, want the restriction's forbidden", err)
					}
					if err := op.run(e, narrow, "missing-"+outside); !restricted(err) {
						t.Errorf("restricted, unknown slug outside: err = %v, want the restriction's forbidden", err)
					}
					if n := len(e.rec.Events) - events; n != 0 {
						t.Errorf("refused calls recorded %d audit events", n)
					}
					if !op.fresh {
						got, err := e.store.Get(e.ctx, outside)
						if err != nil || got.LatestVersion != 1 || got.Name != "" || got.DeletedAt != nil || got.StaleAt != nil ||
							got.Visibility != artifact.Public || got.OwnerHandle != "alice" {
							t.Errorf("refused call changed the artifact: %+v, %v", got, err)
						}
					}
					if err := op.run(e, narrow, inside); err != nil {
						t.Errorf("restricted, slug inside: %v", err)
					}
					if err := op.run(e, alice, outside); err != nil {
						t.Errorf("unrestricted: %v", err)
					}
				})
			}
		})
	}
}

func TestRestrictionNarrowsOnly(t *testing.T) {
	e := newEnv(t, artifact.Options{WritePolicy: artifact.WriteOwner})
	alice := e.user(t, "alice")
	bob := e.user(t, "bob")
	bob.Slugs = []string{"weekly-*"}
	e.create(t, alice, "weekly-report")
	e.create(t, alice, "weekly-draft")
	if _, err := e.svc.SetVisibility(e.ctx, alice, "weekly-draft", artifact.Unpublished); err != nil {
		t.Fatal(err)
	}

	_, _, err := e.svc.Update(e.ctx, bob, artifact.UpdateRequest{Slug: "weekly-report", Name: new("mine now")})
	if !errors.Is(err, artifact.ErrForbidden) || restricted(err) {
		t.Errorf("update of another's artifact under the owner policy: err = %v, want the policy's forbidden", err)
	}
	if _, err := e.svc.MarkStale(e.ctx, bob, "weekly-report", time.Hour); !errors.Is(err, artifact.ErrForbidden) {
		t.Errorf("mark stale of another's artifact: err = %v, want forbidden", err)
	}
	if _, _, err := e.svc.Get(e.ctx, bob, "weekly-draft"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("get of another's unpublished artifact: err = %v, want not found", err)
	}
	if _, _, err := e.svc.Get(e.ctx, bob, "weekly-report"); err != nil {
		t.Errorf("get of a live matching artifact: %v", err)
	}
}

func TestRestrictionList(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	for _, slug := range []string{"weekly-a", "weekly-b", "weekly-c", "weekly", "other", "reports-q1"} {
		e.create(t, alice, slug)
	}
	list := func(actor artifact.Actor, q artifact.ListQuery) []string {
		t.Helper()
		var got []string
		for range 10 {
			page, err := e.svc.List(e.ctx, actor, q)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range page.Items {
				got = append(got, a.Slug)
			}
			if q.Cursor = page.NextCursor; q.Cursor == "" {
				break
			}
		}
		return got
	}
	narrow := alice
	narrow.Slugs = []string{"weekly-*", "other"}
	want := []string{"other", "weekly-c", "weekly-b", "weekly-a"}
	if diff := cmp.Diff(want, list(narrow, artifact.ListQuery{})); diff != "" {
		t.Errorf("restricted listing (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, list(narrow, artifact.ListQuery{Limit: 1, Mine: "created"})); diff != "" {
		t.Errorf("restricted listing, one a page (-want +got):\n%s", diff)
	}
	// A caller cannot widen its own restriction through the query.
	if diff := cmp.Diff(want, list(narrow, artifact.ListQuery{Slugs: []string{"reports-*"}, All: true})); diff != "" {
		t.Errorf("restricted listing with its own patterns (-want +got):\n%s", diff)
	}
	admin := narrow
	admin.Admin = true
	if diff := cmp.Diff(want, list(admin, artifact.ListQuery{All: true})); diff != "" {
		t.Errorf("restricted admin listing everything (-want +got):\n%s", diff)
	}
	if got := list(alice, artifact.ListQuery{}); len(got) != 6 {
		t.Errorf("unrestricted listing = %v, want all 6", got)
	}
}
