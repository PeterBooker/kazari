package present

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{artifact.ErrNotFound, 404, api.CodeNotFound},
		{artifact.Errorf(artifact.ErrSlugTaken, "x"), 409, api.CodeSlugTaken},
		{artifact.ErrVersionConflict, 412, api.CodeVersionConflict},
		{artifact.ErrTooLarge, 413, api.CodeTooLarge},
		{artifact.ErrInvalidSlug, 422, api.CodeInvalidSlug},
		{artifact.ErrForbidden, 403, api.CodeForbidden},
		{artifact.ErrGone, 410, api.CodeGone},
		{artifact.ErrQuotaExceeded, 403, api.CodeQuotaExceeded},
		{&upload.OffsetMismatchError{Path: "a", Current: 7}, 409, api.CodeOffsetMismatch},
		{auth.ErrInvalidToken, 401, api.CodeUnauthorized},
		{user.ErrNotFound, 404, api.CodeNotFound},
		{user.ErrHandleTaken, 409, api.CodeHandleTaken},
		{user.ErrInvalidScope, 422, api.CodeInvalidArgument},
		{user.ErrLastAdmin, 409, api.CodeLastAdmin},
		{user.ErrSelf, 403, api.CodeForbidden},
		{user.ErrTooManyKeys, 403, api.CodeTooManyKeys},
		{user.ErrSetupDone, 409, api.CodeSetupDone},
		{user.ErrLocalOff, 403, api.CodeForbidden},
		{user.ErrLinkNotFound, 404, api.CodeNotFound},
		{user.ErrWeakPassword, 422, api.CodeInvalidArgument},
		{user.ErrBadPassword, 401, api.CodeUnauthorized},
		{&http.MaxBytesError{Limit: 10}, 413, api.CodeTooLarge},
		{errors.New("boom"), 500, api.CodeInternal},
	}
	for _, c := range cases {
		status, e := Error(c.err)
		if status != c.status || e.Code != c.code {
			t.Errorf("Error(%v) = %d %s, want %d %s", c.err, status, e.Code, c.status, c.code)
		}
	}
	_, e := Error(&upload.OffsetMismatchError{Path: "a", Current: 7})
	if e.Current == nil || *e.Current != 7 {
		t.Errorf("offset mismatch did not carry current size: %+v", e)
	}
	_, e = Error(errors.New("secret detail"))
	if e.Message != "internal error" {
		t.Errorf("internal error leaked message %q", e.Message)
	}
}

func TestActorOperator(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{auth.SourceAdminToken, true},
		{auth.SourceLocal, true},
		{auth.SourceAPIKey, false},
		{auth.SourceOAuth, false},
		{"", false},
	}
	for _, tt := range tests {
		// An admin-role key acts as its user, under any handle.
		if got := Actor(auth.Principal{Handle: "admin", Admin: true, Source: tt.source}).Operator; got != tt.want {
			t.Errorf("Actor(source %q).Operator = %v, want %v", tt.source, got, tt.want)
		}
	}
}

func TestActorCredential(t *testing.T) {
	tests := []struct {
		p    auth.Principal
		want string
	}{
		{auth.Principal{Handle: "alice", Source: auth.SourceAPIKey, KeyID: "kzk_abc123"}, "api_key:kzk_abc123"},
		{auth.Principal{Handle: "alice", Source: auth.SourceOAuth, KeyID: "oauth:42"}, "oauth:42"},
		{auth.Principal{Handle: "alice", Source: auth.SourceOAuth, KeyID: "grant-7"}, "oauth:grant-7"},
		{auth.Principal{Handle: "admin", Admin: true, Source: auth.SourceAdminToken}, "admin_token"},
		{auth.Principal{Handle: "alice"}, ""},
	}
	for _, tt := range tests {
		a := Actor(tt.p)
		if a.Credential != tt.want || a.Handle != tt.p.Handle || a.Admin != tt.p.Admin {
			t.Errorf("Actor(%+v) = %+v, want credential %q", tt.p, a, tt.want)
		}
	}
}

func TestLinks(t *testing.T) {
	l := Links{URL: func(sub string) string { return "https://" + sub + ".art.test/" }}
	a := &artifact.Artifact{Slug: "demo", PublishedVersion: 2, LatestVersion: 3}
	got := l.Artifact(a)
	if got.URL != "https://demo.art.test/" || got.Metadata == nil {
		t.Errorf("Artifact = %+v", got)
	}
	v := l.Version("demo", artifact.Version{Number: 3})
	if v.URL != "https://demo--v3.art.test/" {
		t.Errorf("Version URL = %q", v.URL)
	}
	if r := l.Result(a, 0); r.Version != 2 {
		t.Errorf("Result version = %d, want published 2", r.Version)
	}
	if (Links{}).ArtifactURL("x") != "" {
		t.Error("nil URL func should yield empty URL")
	}
}

func TestArchiveURL(t *testing.T) {
	got := ArchiveURL("https://api.art.example.com/v1/", "Zm9v", "tok-en_1")
	if want := "https://api.art.example.com/v1/uploads/Zm9v/archive?token=tok-en_1"; got != want {
		t.Errorf("ArchiveURL = %q, want %q", got, want)
	}
	if status, body := Error(artifact.ErrDigestMismatch); status != http.StatusUnprocessableEntity || body.Code != api.CodeDigestMismatch {
		t.Errorf("Error(ErrDigestMismatch) = %d %+v", status, body)
	}
}

func TestActorCanDelete(t *testing.T) {
	tests := []struct {
		p    auth.Principal
		want bool
	}{
		{auth.Principal{Handle: "alice", Scopes: auth.MemberScopes}, true},
		{auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}}, false},
		{auth.Principal{Handle: "root", Admin: true}, true},
	}
	for _, tt := range tests {
		if got := Actor(tt.p).CanDelete; got != tt.want {
			t.Errorf("Actor(%+v).CanDelete = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestAccountLinkURL(t *testing.T) {
	l := Links{URL: func(sub string) string {
		if sub == "" {
			return "https://art.example.com/"
		}
		return "https://" + sub + ".art.example.com/"
	}}
	for purpose, want := range map[user.Purpose]string{
		user.PurposeInvite: "https://art.example.com/account/invite?token=kzl_abc",
		user.PurposeReset:  "https://art.example.com/account/reset?token=kzl_abc",
		user.PurposeSetup:  "https://art.example.com/account/setup?token=kzl_abc",
	} {
		if got := l.AccountLinkURL(purpose, "kzl_abc"); got != want {
			t.Errorf("AccountLinkURL(%s) = %q, want %q", purpose, got, want)
		}
	}
	// The token is a query value whatever it holds, never part of the path.
	if got := l.AccountLinkURL(user.PurposeInvite, "a/b&c=d"); got != "https://art.example.com/account/invite?token=a%2Fb%26c%3Dd" {
		t.Errorf("an odd token = %q", got)
	}

	link := &user.Link{ID: 4, UserHandle: "ana", Purpose: user.PurposeReset, Hash: "the-hash", RevokeAll: true, CreatedBy: "boss"}
	created := l.CreatedLink(link, "kzl_abc")
	if created.URL != "https://art.example.com/account/reset?token=kzl_abc" || created.ID != 4 || created.User != "ana" ||
		created.Purpose != "reset" || !created.RevokeAll || created.CreatedBy != "boss" {
		t.Errorf("CreatedLink = %+v", created)
	}
	raw, err := json.Marshal(created)
	if err != nil || strings.Contains(string(raw), "the-hash") {
		t.Errorf("the wire type carries the token's hash: %s, %v", raw, err)
	}
}

func TestReady(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ok := Ready(preflight.Report{At: at, Findings: []preflight.Finding{
		{Check: "config", Status: preflight.OK, Detail: "valid"}, {Check: "disk", Status: preflight.Warn, Detail: "low"}}})
	if !ok.Ready || !ok.CheckedAt.Equal(at) || len(ok.Checks) != 2 || ok.Checks[1] != (api.Check{Check: "disk", Status: "warn", Detail: "low"}) {
		t.Errorf("Ready = %+v", ok)
	}
	failed := Ready(preflight.Report{At: at, Findings: []preflight.Finding{{Check: "dns", Status: preflight.Fail, Detail: "missing"}}})
	if failed.Ready {
		t.Errorf("Ready with a failed check = %+v", failed)
	}
	if empty := Ready(preflight.Report{}); !empty.Ready || empty.Checks == nil {
		t.Errorf("Ready of no findings = %+v, want ready with an empty list", empty)
	}
}

func TestWithAccount(t *testing.T) {
	last := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	u := WithAccount(api.User{Handle: "ana"}, user.Account{Handle: "ana", HasPassword: true, LiveKeys: 2, KeyUsedAt: &last})
	if !u.Password || u.Invited || u.Keys != 2 || u.LastActiveAt == nil || !u.LastActiveAt.Equal(last) {
		t.Errorf("WithAccount = %+v", u)
	}
	if u := WithAccount(api.User{Handle: "ben"}, user.Account{}); u.Password || u.Invited || u.Keys != 0 || u.LastActiveAt != nil {
		t.Errorf("WithAccount of no account = %+v", u)
	}
}

func TestPageCode(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusOK: "", http.StatusSeeOther: "", http.StatusBadRequest: api.CodeInvalidArgument,
		http.StatusUnauthorized: api.CodeUnauthorized, http.StatusForbidden: api.CodeForbidden, http.StatusNotFound: api.CodeNotFound,
		http.StatusConflict: api.CodeInvalidArgument, http.StatusUnprocessableEntity: api.CodeInvalidArgument,
		http.StatusTooManyRequests: api.CodeRateLimited, http.StatusInternalServerError: api.CodeInternal,
		http.StatusInsufficientStorage: api.CodeInternal,
	} {
		if got := PageCode(status); got != want {
			t.Errorf("PageCode(%d) = %q, want %q", status, got, want)
		}
		if got := PageCode(status); got != "" && !slices.Contains(api.ErrorCodes, got) {
			t.Errorf("PageCode(%d) = %q, which is not one of the API's codes", status, got)
		}
	}
}

func TestIdentityNeverSignedIn(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := Identity(user.Identity{Issuer: "https://idp.test", Subject: "s", LastLoginAt: at}); got.LastLoginAt == nil || !got.LastLoginAt.Equal(at) {
		t.Errorf("LastLoginAt = %v, want %v", got.LastLoginAt, at)
	}
	// Linked by an admin and not used yet: no sign-in to report.
	got := Identity(user.Identity{Issuer: "https://idp.test", Subject: "s"})
	raw, err := json.Marshal(got)
	if err != nil || got.LastLoginAt != nil || strings.Contains(string(raw), "last_login_at") {
		t.Errorf("an identity that never signed in = %s, %v", raw, err)
	}
}

func TestUpdate(t *testing.T) {
	if got := Update(update.Result{}); got != nil {
		t.Errorf("Update before any answer = %+v, want nil", got)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	got := Update(update.Result{Latest: "0.8.0", Newer: true, CheckedAt: at})
	if got == nil || *got != (api.Update{Latest: "0.8.0", Newer: true, CheckedAt: at}) {
		t.Errorf("Update = %+v", got)
	}
}

func TestAccountErrors(t *testing.T) {
	status, body := Error(fmt.Errorf("user: invite %q: %w", "ana", user.ErrHasPassword))
	if status != http.StatusUnprocessableEntity || body.Code != api.CodeInvalidArgument || !strings.Contains(body.Message, "reset link") {
		t.Errorf("ErrHasPassword = %d %+v", status, body)
	}
	status, body = Error(fmt.Errorf("user: check password: %w", user.ErrBusy))
	if status != http.StatusServiceUnavailable || body.Code != api.CodeBusy || !slices.Contains(api.ErrorCodes, api.CodeBusy) {
		t.Errorf("ErrBusy = %d %+v", status, body)
	}
}
