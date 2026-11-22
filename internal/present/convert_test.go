package present_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

var links = present.Links{URL: func(sub string) string {
	if sub == "" {
		return "https://art.test/"
	}
	return "https://" + sub + ".art.test/"
}}

func day(n int) time.Time { return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC) }

// unsetFields lists the fields of v, and of the structs inside it, that hold
// their zero value.
func unsetFields(v any) []string {
	rv := reflect.Indirect(reflect.ValueOf(v))
	var out []string
	walkUnset(rv, rv.Type().Name(), &out)
	slices.Sort(out)
	return slices.Compact(out)
}

func walkUnset(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkUnset(v.Elem(), path, out)
		}
	case reflect.Slice:
		for i := range v.Len() {
			walkUnset(v.Index(i), path, out)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			name := path + "." + v.Type().Field(i).Name
			if v.Field(i).IsZero() {
				*out = append(*out, name)
				continue
			}
			walkUnset(v.Field(i), name, out)
		}
	}
}

// Every conversion is given a value with every field set, and a field of the
// wire type that is still zero afterwards has to be named in unset, so a
// field added to a wire type and forgotten in its conversion fails here. The
// share link is a revoked one, which is why it is not active.
func TestConversions(t *testing.T) {
	a := &artifact.Artifact{
		Slug: "demo", Name: "Demo", Description: "A page", OwnerHandle: "ana", Visibility: artifact.Members,
		Metadata:         map[string]string{"team": "docs"},
		PublishedVersion: 2, LatestVersion: 3, SizeBytes: 4096,
		StaleAt: new(day(3)), ExpiresAt: new(day(4)), UnpublishedAt: new(day(5)), DeletedAt: new(day(6)), PurgedAt: new(day(7)),
		CreatedAt: day(1), UpdatedAt: day(2),
	}
	wantArtifact := api.Artifact{
		Slug: "demo", Name: "Demo", Description: "A page", Owner: "ana", Visibility: "members",
		Metadata: map[string]string{"team": "docs"}, URL: "https://demo.art.test/",
		PublishedVersion: 2, LatestVersion: 3, SizeBytes: 4096,
		StaleAt: new(day(3)), ExpiresAt: new(day(4)), UnpublishedAt: new(day(5)), DeletedAt: new(day(6)), PurgedAt: new(day(7)),
		CreatedAt: day(1), UpdatedAt: day(2),
	}

	v := artifact.Version{ID: 9, Number: 3, Label: "fix", CreatedBy: "ben", CreatedAt: day(2), SizeBytes: 12,
		Files: []artifact.File{{Path: "index.html", Hash: "ab12", Size: 12, ContentType: "text/html"}}}
	wantVersion := api.Version{Number: 3, Label: "fix", CreatedBy: "ben", CreatedAt: day(2), SizeBytes: 12,
		URL:   "https://demo--v3.art.test/",
		Files: []api.File{{Path: "index.html", Hash: "ab12", Size: 12, ContentType: "text/html"}}}

	report := &artifact.ImportReport{Source: "https://example.com/", Fetched: 4,
		Skipped:        []artifact.SkippedFetch{{URL: "https://example.com/big.mp4", Reason: "too large"}},
		ScriptsRemoved: 2, Notes: []string{"forms do not submit"},
		Outline: []artifact.Landmark{{Line: 12, Tag: "<main>", Heading: "Welcome"}}}
	wantReport := &api.ImportReport{Source: "https://example.com/", Fetched: 4,
		Skipped:        []api.SkippedFetch{{URL: "https://example.com/big.mp4", Reason: "too large"}},
		ScriptsRemoved: 2, Notes: []string{"forms do not submit"},
		Outline: []api.Landmark{{Line: 12, Tag: "<main>", Heading: "Welcome"}}}

	sl := &artifact.ShareLink{Prefix: "kzs_ab12", Label: "for the client", CreatedBy: "ana", CreatedAt: day(1), ExpiresAt: day(9),
		RevokedAt: new(day(3)), LastUsedAt: new(day(2)), Uses: 5}
	wantShare := api.ShareLink{Prefix: "kzs_ab12", Label: "for the client", CreatedBy: "ana", CreatedAt: day(1), ExpiresAt: day(9),
		RevokedAt: new(day(3)), LastUsedAt: new(day(2)), Uses: 5}

	u := &user.User{ID: 7, Handle: "ana", Email: "ana@example.com", DisplayName: "Ana", Role: user.RoleAdmin,
		QuotaBytes: 1 << 20, DisabledAt: new(day(8)), CreatedAt: day(1)}
	wantUser := api.User{Handle: "ana", Email: "ana@example.com", DisplayName: "Ana", Role: "admin",
		QuotaBytes: 1 << 20, DisabledAt: new(day(8)), CreatedAt: day(1)}
	account := user.Account{Handle: "ana", HasPassword: true, Invited: true, Identities: 1, LiveKeys: 2, RecoveryCodes: 8, Passkeys: 3,
		PasswordLoginAt: new(day(2)), PasskeyLoginAt: new(day(5)), IdentityLoginAt: new(day(3)), KeyUsedAt: new(day(4))}
	wantAccount := wantUser
	wantAccount.Password, wantAccount.Invited, wantAccount.Keys = true, true, 2
	wantAccount.RecoveryCodes, wantAccount.Passkeys, wantAccount.LastActiveAt = 8, 3, new(day(5))

	link := &user.Link{ID: 4, UserHandle: "ana", Purpose: user.PurposeReset, Hash: "the-hash", RevokeAll: true, CreatedBy: "boss",
		CreatedAt: day(1), ExpiresAt: day(2), UsedAt: new(day(2)), RevokedAt: new(day(2))}
	wantLink := api.Link{ID: 4, User: "ana", Purpose: "reset", RevokeAll: true, CreatedBy: "boss", CreatedAt: day(1), ExpiresAt: day(2)}

	tests := []struct {
		name  string
		got   any
		want  any
		unset []string
	}{
		{name: "Artifact", got: links.Artifact(a), want: wantArtifact},
		{
			name: "Artifacts",
			got:  links.Artifacts(artifact.ListPage{Items: []artifact.Artifact{*a}, NextCursor: "next"}),
			want: api.ArtifactList{Items: []api.Artifact{wantArtifact}, NextCursor: "next"},
		},
		{
			name:  "Artifacts of an empty page",
			got:   links.Artifacts(artifact.ListPage{}),
			want:  api.ArtifactList{Items: []api.Artifact{}},
			unset: []string{"ArtifactList.NextCursor"},
		},
		{name: "Version", got: links.Version("demo", v), want: wantVersion},
		{
			name:  "Detail",
			got:   links.Detail(a, []artifact.Version{v}),
			want:  api.ArtifactDetail{Artifact: wantArtifact, Versions: []api.Version{wantVersion}},
			unset: []string{"ArtifactDetail.NextFilesBefore"},
		},
		{
			name:  "Result",
			got:   links.Result(a, 3),
			want:  api.PublishResult{URL: "https://demo.art.test/", Version: 3, VersionURL: "https://demo--v3.art.test/", Artifact: wantArtifact},
			unset: []string{"PublishResult.Import", "PublishResult.Unchanged", "PublishResult.Warnings"},
		},
		{
			name:  "Result of the published version",
			got:   links.Result(a, 0),
			want:  api.PublishResult{URL: "https://demo.art.test/", Version: 2, VersionURL: "https://demo--v2.art.test/", Artifact: wantArtifact},
			unset: []string{"PublishResult.Import", "PublishResult.Unchanged", "PublishResult.Warnings"},
		},
		{
			name: "Imported",
			got:  links.Imported(a, &artifact.Version{Number: 3, Unchanged: true, Warnings: []string{"no index.html"}}, report),
			want: api.PublishResult{URL: "https://demo.art.test/", Version: 3, VersionURL: "https://demo--v3.art.test/", Unchanged: true,
				Warnings: []string{"no index.html"}, Artifact: wantArtifact, Import: wantReport},
		},
		{name: "ShareLink", got: present.ShareLink(sl, day(8)), want: wantShare, unset: []string{"ShareLink.Active"}},
		{
			name:  "ShareLinks",
			got:   present.ShareLinks([]artifact.ShareLink{*sl}, day(8)),
			want:  api.ShareLinkList{Items: []api.ShareLink{wantShare}},
			unset: []string{"ShareLinkList.Items.Active"},
		},
		{
			name:  "CreatedShareLink",
			got:   links.CreatedShareLink(a, sl, "a/b&c", day(8)),
			want:  api.CreatedShareLink{URL: "https://demo.art.test" + gate.SharePath + "?token=a%2Fb%26c", ShareLink: wantShare, Artifact: wantArtifact},
			unset: []string{"CreatedShareLink.ShareLink.Active"},
		},
		{
			name: "Edit",
			got: links.Edit(a, &artifact.Version{Number: 3, Warnings: []string{"w"}}, "index.html", []artifact.EditOutcome{
				{Match: "exact", Replacements: 1, StartLine: 4, EndLine: 6, Excerpt: "<h1>Hi</h1>", ExcerptOf: 2}}),
			want: api.EditResult{URL: "https://demo.art.test/", Version: 3, VersionURL: "https://demo--v3.art.test/", Path: "index.html",
				Warnings: []string{"w"}, Artifact: wantArtifact,
				Edits: []api.EditOutcome{{Match: "exact", Replacements: 1, StartLine: 4, EndLine: 6, Excerpt: "<h1>Hi</h1>", ExcerptOf: 2}}},
		},
		{name: "Stale", got: links.Stale(a), want: api.StaleResult{StaleAt: day(3), ExpiresAt: day(4), Artifact: wantArtifact}},
		{
			name: "Upload",
			got: present.Upload(&upload.Upload{ID: "up1", Owner: "ana", Bytes: 5, CreatedAt: day(1), ExpiresAt: day(2),
				Files: []upload.StagedFile{{Path: "a.txt", Bytes: 5}}, Archive: &upload.Archive{SHA256: "ab12", Bytes: 5}}, 1024, 4096),
			want:  api.Upload{UploadID: "up1", ExpiresAt: day(2), MaxChunkBytes: 1024, MaxTotalBytes: 4096},
			unset: []string{"Upload.ArchiveURL"},
		},
		{
			name: "Chunk",
			got:  present.Chunk(upload.StagedFile{Path: "a.txt", StagingIndex: 1, Bytes: 5, ContentType: "text/plain"}),
			want: api.ChunkResult{Path: "a.txt", BytesReceived: 5},
		},
		{
			name: "User",
			got:  present.User(u),
			want: wantUser,
			unset: []string{"User.Identities", "User.Invited", "User.Keys", "User.LastActiveAt", "User.Passkeys", "User.Password",
				"User.RecoveryCodes", "User.Sessions", "User.UsedBytes"},
		},
		{
			name:  "WithAccount",
			got:   present.WithAccount(present.User(u), account),
			want:  wantAccount,
			unset: []string{"User.Identities", "User.Sessions", "User.UsedBytes"},
		},
		{name: "Link", got: present.Link(link), want: wantLink},
		{
			name: "CreatedLink",
			got:  links.CreatedLink(link, "kzl_abc"),
			want: api.CreatedLink{Link: wantLink, URL: "https://art.test/account/reset?token=kzl_abc"},
		},
		{
			name: "Identity",
			got:  present.Identity(user.Identity{Issuer: "https://idp.test", Subject: "s1", EmailAtLogin: "ana@example.com", LastLoginAt: day(2)}),
			want: api.Identity{Issuer: "https://idp.test", Subject: "s1", Email: "ana@example.com", LastLoginAt: new(day(2))},
		},
		{
			name: "Key",
			got: present.Key(&user.APIKey{ID: 3, UserHandle: "ana", Prefix: "kzk_ab12", Hash: "the-hash", Label: "laptop",
				Scopes: []string{auth.ScopeRead}, Slugs: []string{"docs-*"}, IssuedBy: "kzk_cd34",
				LastUsedAt: new(day(2)), ExpiresAt: new(day(9)), RevokedAt: new(day(3)), CreatedAt: day(1)}),
			want: api.APIKey{Prefix: "kzk_ab12", Label: "laptop", Scopes: []string{auth.ScopeRead}, Slugs: []string{"docs-*"},
				IssuedBy: "kzk_cd34", LastUsedAt: new(day(2)), ExpiresAt: new(day(9)), RevokedAt: new(day(3)), CreatedAt: day(1)},
		},
		{
			name: "Me",
			got: present.Me(auth.Principal{Handle: "ana", Admin: true, Scopes: []string{auth.ScopeRead}, Source: auth.SourceAPIKey,
				KeyID: "kzk_ab12", QuotaBytes: 1 << 20, ClientID: "client", Slugs: []string{"docs-*"}, ExpiresAt: new(day(9))}, 512),
			want: api.Me{Handle: "ana", Admin: true, Scopes: []string{auth.ScopeRead}, Source: "api_key", Slugs: []string{"docs-*"},
				ExpiresAt: new(day(9)), QuotaBytes: 1 << 20, UsedBytes: 512},
		},
		{
			name: "Job",
			got: present.Job(scheduler.JobStatus{Name: "gc", Every: 90 * time.Minute, LastRun: new(day(1)), LastError: "disk full",
				NextRun: new(day(2)), Running: true}),
			want: api.JobStatus{Name: "gc", Every: "1h30m0s", LastRun: new(day(1)), LastError: "disk full", NextRun: new(day(2)), Running: true},
		},
		{
			name: "AuditEvent",
			got: present.AuditEvent(audit.Event{ID: 5, At: day(1), Actor: "ana", Action: "artifact.create", Slug: "demo", Subject: "ben",
				Details: map[string]string{"version": "1"}}),
			want: api.AuditEvent{At: day(1), Actor: "ana", Action: "artifact.create", Slug: "demo", Subject: "ben",
				Details: map[string]string{"version": "1"}},
		},
		{
			name: "Ready",
			got: present.Ready(preflight.Report{At: day(1), Findings: []preflight.Finding{
				{Check: "disk", Status: preflight.Warn, Detail: "low"}}}),
			want:  api.Ready{Ready: true, CheckedAt: day(1), Checks: []api.Check{{Check: "disk", Status: "warn", Detail: "low"}}},
			unset: []string{"Ready.Update"},
		},
		{
			name: "Update",
			got:  present.Update(update.Result{Latest: "0.8.0", Newer: true, CheckedAt: day(1)}),
			want: &api.Update{Latest: "0.8.0", Newer: true, CheckedAt: day(1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("%s (-want +got):\n%s", tt.name, diff)
			}
			if diff := cmp.Diff(tt.unset, unsetFields(tt.got)); diff != "" {
				t.Errorf("%s leaves fields unset (-want +got):\n%s", tt.name, diff)
			}
		})
	}
}

// A list that is empty goes out as [], which a client can range over, never
// as null.
func TestEmptyListsAreNotNull(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{
			name: "Import of a report with nothing in it",
			got:  present.Import(&artifact.ImportReport{}),
			want: &api.ImportReport{Skipped: []api.SkippedFetch{}, Notes: []string{}, Outline: []api.Landmark{}},
		},
		{name: "Detail without versions", got: links.Detail(&artifact.Artifact{}, nil).Versions, want: []api.Version{}},
		{name: "ShareLinks of none", got: present.ShareLinks(nil, day(1)), want: api.ShareLinkList{Items: []api.ShareLink{}}},
		{name: "Edit without outcomes", got: links.Edit(&artifact.Artifact{}, &artifact.Version{Number: 1}, "a", nil).Edits, want: []api.EditOutcome{}},
		{name: "Key without scopes", got: present.Key(&user.APIKey{}).Scopes, want: []string{}},
		{name: "Me without scopes", got: present.Me(auth.Principal{}, 0).Scopes, want: []string{}},
		{name: "Artifact without metadata", got: links.Artifact(&artifact.Artifact{}).Metadata, want: map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.got); diff != "" {
				t.Errorf("%s (-want +got):\n%s", tt.name, diff)
			}
		})
	}
}

func TestShareLinkActive(t *testing.T) {
	tests := []struct {
		name string
		link artifact.ShareLink
		now  time.Time
		want bool
	}{
		{name: "before it expires", link: artifact.ShareLink{ExpiresAt: day(5)}, now: day(4), want: true},
		{name: "after it expires", link: artifact.ShareLink{ExpiresAt: day(5)}, now: day(6), want: false},
		{name: "revoked", link: artifact.ShareLink{ExpiresAt: day(5), RevokedAt: new(day(2))}, now: day(4), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := present.ShareLink(&tt.link, tt.now).Active; got != tt.want {
				t.Errorf("ShareLink(%+v, %v).Active = %v, want %v", tt.link, tt.now, got, tt.want)
			}
		})
	}
}

func TestStaleWithoutDates(t *testing.T) {
	got := links.Stale(&artifact.Artifact{Slug: "demo"})
	if !got.StaleAt.IsZero() || !got.ExpiresAt.IsZero() {
		t.Errorf("Stale of an artifact that is not stale = %v and %v, want zero times", got.StaleAt, got.ExpiresAt)
	}
}

func TestLinksWithoutURL(t *testing.T) {
	var l present.Links
	if got := l.ArtifactURL("demo"); got != "" {
		t.Errorf("Links{}.ArtifactURL(%q) = %q, want none", "demo", got)
	}
	if got := l.VersionURL("demo", 3); got != "" {
		t.Errorf("Links{}.VersionURL(%q, 3) = %q, want none", "demo", got)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
		err  error
	}{
		{name: "empty", in: "", want: 0},
		{name: "blank", in: "  ", want: 0},
		{name: "hours", in: "72h", want: 72 * time.Hour},
		{name: "days", in: "14d", want: 14 * 24 * time.Hour},
		{name: "weeks", in: "2w", want: 14 * 24 * time.Hour},
		{name: "days and hours", in: "1d12h", want: 36 * time.Hour},
		{name: "weeks and days", in: "1w2d", want: 9 * 24 * time.Hour},
		{name: "days after hours", in: "12h1d", err: artifact.ErrInvalidArgument},
		{name: "signed after days", in: "1d-2h", err: artifact.ErrInvalidArgument},
		{name: "spaces around", in: " 90m ", want: 90 * time.Minute},
		{name: "zero", in: "0", err: artifact.ErrInvalidArgument},
		{name: "zero days", in: "0d", err: artifact.ErrInvalidArgument},
		{name: "negative", in: "-1h", err: artifact.ErrInvalidArgument},
		{name: "negative days", in: "-3d", err: artifact.ErrInvalidArgument},
		{name: "part of a day", in: "1.5d", err: artifact.ErrInvalidArgument},
		{name: "no unit", in: "72", err: artifact.ErrInvalidArgument},
		{name: "not a duration", in: "soon", err: artifact.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := present.ParseDuration("expires_in", tt.in)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Errorf("ParseDuration(%q) = %v, %v, want %v, %v", tt.in, got, err, tt.want, tt.err)
			}
		})
	}
}

func TestParseOverlap(t *testing.T) {
	const def = 24 * time.Hour
	tests := []struct {
		name string
		in   string
		want time.Duration
		err  error
	}{
		{name: "empty is the default", in: "", want: def},
		{name: "zero", in: "0", want: 0},
		{name: "zero seconds", in: " 0s ", want: 0},
		{name: "zero days", in: "0d", want: 0},
		{name: "hours", in: "2h", want: 2 * time.Hour},
		{name: "days", in: "3d", want: 72 * time.Hour},
		{name: "negative", in: "-1h", err: artifact.ErrInvalidArgument},
		{name: "not a duration", in: "soon", err: artifact.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := present.ParseOverlap("overlap", tt.in, def)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Errorf("ParseOverlap(%q) = %v, %v, want %v, %v", tt.in, got, err, tt.want, tt.err)
			}
		})
	}
}

func TestRequireShare(t *testing.T) {
	tests := []struct {
		name       string
		p          auth.Principal
		visibility string
		err        error
	}{
		{name: "no visibility named", p: auth.Principal{Scopes: []string{auth.ScopeWrite}}, visibility: ""},
		{name: "with the scope", p: auth.Principal{Scopes: []string{auth.ScopeWrite, auth.ScopeShare}}, visibility: "public"},
		{name: "an admin", p: auth.Principal{Admin: true}, visibility: "members"},
		{name: "without the scope", p: auth.Principal{Scopes: []string{auth.ScopeWrite}}, visibility: "public", err: artifact.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := present.RequireShare(tt.p, tt.visibility); !errors.Is(err, tt.err) {
				t.Errorf("RequireShare(%+v, %q) = %v, want %v", tt.p, tt.visibility, err, tt.err)
			}
		})
	}
}
