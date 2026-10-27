package present

import (
	"errors"
	"net/http"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
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
