package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/user"
)

// slugCase is a request to a route that names a slug, valid in everything
// but the slug. want is the status for a slug the credential may reach.
type slugCase struct {
	suffix string // after /v1/artifacts/<slug>
	body   any
	want   int
}

// slugRoutes has a case for every user API route with {slug} in its
// pattern. TestRestrictionOnEveryRoute fails for a route that is missing
// here, so a new route cannot go without the check.
var slugRoutes = map[string]slugCase{
	"GET /v1/artifacts/{slug}":                         {"", nil, http.StatusOK},
	"PUT /v1/artifacts/{slug}":                         {"", api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "index.html", Content: "changed"}}}, http.StatusOK},
	"PATCH /v1/artifacts/{slug}":                       {"", map[string]any{"name": "renamed", "visibility": "unlisted"}, http.StatusOK},
	"DELETE /v1/artifacts/{slug}":                      {"?hard=true", nil, http.StatusNoContent},
	"POST /v1/artifacts/{slug}/edit":                   {"/edit", api.EditRequest{Path: "index.html", Edits: []api.Edit{{OldText: "<h1>", NewText: "<h2>"}}}, http.StatusOK},
	"POST /v1/artifacts/{slug}/publish":                {"/publish", api.PublishRequest{Version: 1}, http.StatusOK},
	"POST /v1/artifacts/{slug}/stale":                  {"/stale", nil, http.StatusOK},
	"DELETE /v1/artifacts/{slug}/stale":                {"/stale", nil, http.StatusOK},
	"GET /v1/artifacts/{slug}/files/{path...}":         {"/files/index.html", nil, http.StatusOK},
	"POST /v1/artifacts/{slug}/share-links":            {"/share-links", nil, http.StatusCreated},
	"GET /v1/artifacts/{slug}/share-links":             {"/share-links", nil, http.StatusOK},
	"DELETE /v1/artifacts/{slug}/share-links/{prefix}": {"/share-links/aaaaaaaa", nil, http.StatusNotFound},
}

const restrictedTo = "restricted to slugs matching mine-*"

func TestRestrictionOnEveryRoute(t *testing.T) {
	e := newEnv(t, config{})
	_, narrow := e.key(user.IssueKeyRequest{Handle: "alice", Slugs: []string{"mine-*"}})
	e.create(e.alice, "other-page")

	seen := 0
	for _, pattern := range httpapi.UserRoutes() {
		if !strings.Contains(pattern, "{slug}") {
			continue
		}
		seen++
		c, ok := slugRoutes[pattern]
		if !ok {
			t.Errorf("route %q names a slug but has no case in slugRoutes", pattern)
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			e := e.with(t)
			method, _, _ := strings.Cut(pattern, " ")
			if got := e.apiErr(e.jsonReq(method, e.u("/v1/artifacts/other-page"+c.suffix), narrow, c.body), http.StatusForbidden, api.CodeForbidden); !strings.Contains(got.Message, restrictedTo) {
				t.Errorf("message = %q, want the restriction named", got.Message)
			}
			// The same request for a slug the key reaches goes through, so
			// the refusal above was the restriction's and nothing else's.
			slug := "mine-" + strings.ToLower(method) + strings.NewReplacer("/", "", "?", "", "=", "", ".", "").Replace(c.suffix)
			e.create(e.alice, slug)
			e.expect(e.jsonReq(method, e.u("/v1/artifacts/"+slug+c.suffix), narrow, c.body), c.want, nil)
		})
	}
	if seen != len(slugRoutes) {
		t.Errorf("slugRoutes has %d cases for %d routes that name a slug", len(slugRoutes), seen)
	}

	var d api.ArtifactDetail
	e.call(http.MethodGet, e.u("/v1/artifacts/other-page?files=true"), e.alice, nil, http.StatusOK, &d)
	if a := d.Artifact; a.LatestVersion != 1 || a.Name != "" || a.Visibility != "public" || a.StaleAt != nil || a.DeletedAt != nil {
		t.Errorf("the refused calls changed the artifact: %+v", a)
	}
	var links api.ShareLinkList
	e.call(http.MethodGet, e.u("/v1/artifacts/other-page/share-links"), e.alice, nil, http.StatusOK, &links)
	if len(links.Items) != 0 {
		t.Errorf("a refused call created a share link: %+v", links.Items)
	}
}

func (e *env) jsonReq(method, url, token string, in any) *http.Response {
	e.t.Helper()
	r := req{method: method, url: url, token: token}
	if in != nil {
		r.body, r.contentType = bytes.NewReader(mustJSON(e.t, in)), "application/json"
	}
	return e.send(r)
}

// Creating is the one write that names its slug in the body.
func TestRestrictionOnCreate(t *testing.T) {
	e := newEnv(t, config{})
	_, narrow := e.key(user.IssueKeyRequest{Handle: "alice", Slugs: []string{"mine-*", "exact-one"}})
	fetched := 0
	page := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched++
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<title>Source</title><h1>hi</h1>")
	}))
	files := []api.FileInput{{Path: "index.html", Content: "<h1>x</h1>"}}
	multipartCreate := func(slug string) *http.Response {
		body, ct := multipartBody(t,
			part{"manifest", mustJSON(t, api.CreateManifest{Slug: slug})},
			part{"bundle", tarGz(t, tarEntry{name: "index.html", body: "x"})})
		return e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: narrow, body: body, contentType: ct})
	}

	for name, create := range map[string]func(slug string) *http.Response{
		"inline files": func(slug string) *http.Response {
			return e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: slug, Files: files})
		},
		"a web page": func(slug string) *http.Response {
			return e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: slug, SourceURL: page})
		},
		"a bundle": multipartCreate,
	} {
		t.Run(name, func(t *testing.T) {
			e := e.with(t)
			tag := strings.ReplaceAll(name, " ", "-")
			before := fetched
			if got := e.apiErr(create("other-"+tag), http.StatusForbidden, api.CodeForbidden); !strings.Contains(got.Message, "restricted to slugs matching mine-*, exact-one") {
				t.Errorf("message = %q, want the patterns named", got.Message)
			}
			if fetched != before {
				t.Error("a refused import fetched the page")
			}
			e.expect(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/other-" + tag), token: e.alice}), http.StatusNotFound, nil)
			e.expect(create("mine-"+tag), http.StatusCreated, nil)
		})
	}
	e.expect(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: "exact-one", Files: files}), http.StatusCreated, nil)
	e.apiErr(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: "exact-one-more", Files: files}),
		http.StatusForbidden, api.CodeForbidden)
}

func TestRestrictionOnList(t *testing.T) {
	e := newEnv(t, config{})
	_, narrow := e.key(user.IssueKeyRequest{Handle: "alice", Slugs: []string{"mine-*"}})
	for _, slug := range []string{"mine-a", "other-a", "mine-b", "other-b", "mine-c"} {
		e.create(e.alice, slug)
	}
	var got []string
	cursor := ""
	for range 5 {
		var page api.ArtifactList
		e.call(http.MethodGet, e.u("/v1/artifacts?limit=2&cursor="+cursor), narrow, nil, http.StatusOK, &page)
		for _, a := range page.Items {
			got = append(got, a.Slug)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if diff := cmp.Diff([]string{"mine-c", "mine-b", "mine-a"}, got); diff != "" {
		t.Errorf("restricted listing (-want +got):\n%s", diff)
	}
}

// Uploads belong to the user and name no slug until they are published.
func TestRestrictionLeavesUploadsAlone(t *testing.T) {
	e := newEnv(t, config{})
	_, narrow := e.key(user.IssueKeyRequest{Handle: "alice", Slugs: []string{"mine-*"}})
	var up api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), narrow, nil, http.StatusCreated, &up)
	e.expect(e.send(req{method: http.MethodPatch, url: e.u("/v1/uploads/" + up.UploadID + "/files/index.html?offset=0"),
		token: narrow, body: strings.NewReader("<h1>staged</h1>")}), http.StatusOK, nil)
	e.apiErr(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: "other-up", UploadID: up.UploadID}),
		http.StatusForbidden, api.CodeForbidden)

	e.call(http.MethodPost, e.u("/v1/uploads"), narrow, nil, http.StatusCreated, &up)
	e.expect(e.send(req{method: http.MethodPatch, url: e.u("/v1/uploads/" + up.UploadID + "/files/index.html?offset=0"),
		token: narrow, body: strings.NewReader("<h1>staged</h1>")}), http.StatusOK, nil)
	e.expect(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), narrow, api.CreateArtifactRequest{Slug: "mine-up", UploadID: up.UploadID}),
		http.StatusCreated, nil)
}

func TestMeShowsRestrictionAndExpiry(t *testing.T) {
	e := newEnv(t, config{})
	k, narrow := e.key(user.IssueKeyRequest{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*", "report"}, ExpiresIn: 90 * day})
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), narrow, nil, http.StatusOK, &me)
	want := api.Me{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Source: auth.SourceAPIKey,
		Slugs: []string{"weekly-*", "report"}, ExpiresAt: k.ExpiresAt}
	if diff := cmp.Diff(want, me); diff != "" {
		t.Errorf("GET /me (-want +got):\n%s", diff)
	}
	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/me"), token: e.alice})
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "slugs") || strings.Contains(string(raw), "expires_at") {
		t.Errorf("GET /me of an unrestricted key without expiry: %s", raw)
	}
}

func TestShareScope(t *testing.T) {
	e := newEnv(t, config{artifact: artifactOptions("members")})
	_, writer := e.key(user.IssueKeyRequest{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeWrite, auth.ScopeDelete}})
	_, sharer := e.key(user.IssueKeyRequest{Handle: "alice", Scopes: []string{auth.ScopeRead, auth.ScopeShare}})
	e.create(e.alice, "demo")
	files := []api.FileInput{{Path: "index.html", Content: "<h1>x</h1>"}}
	wantShare := func(resp *http.Response) {
		t.Helper()
		if got := e.apiErr(resp, http.StatusForbidden, api.CodeForbidden); !strings.Contains(got.Message, `"artifacts:share"`) {
			t.Errorf("message = %q, want the share scope named", got.Message)
		}
	}

	wantShare(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), writer, api.CreateArtifactRequest{Slug: "open-page", Visibility: "public", Files: files}))
	wantShare(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), writer, api.CreateArtifactRequest{Slug: "open-page", Visibility: "members", Files: files}))
	body, ct := multipartBody(t,
		part{"manifest", mustJSON(t, api.CreateManifest{Slug: "open-page", Visibility: "public"})},
		part{"bundle", tarGz(t, tarEntry{name: "index.html", body: "x"})})
	wantShare(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: writer, body: body, contentType: ct}))
	wantShare(e.jsonReq(http.MethodPatch, e.u("/v1/artifacts/demo"), writer, map[string]any{"visibility": "public"}))
	wantShare(e.jsonReq(http.MethodPatch, e.u("/v1/artifacts/demo"), writer, map[string]any{"visibility": "unpublished"}))
	wantShare(e.jsonReq(http.MethodPut, e.u("/v1/artifacts/demo"), writer, map[string]any{"visibility": "public", "files": files}))
	wantShare(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/share-links"), writer, nil))
	wantShare(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/demo/share-links/aaaaaaaa"), token: writer}))

	// Without the scope a new page gets the instance default, and every
	// other change is still open to artifacts:write.
	var res api.PublishResult
	e.expect(e.jsonReq(http.MethodPost, e.u("/v1/artifacts"), writer, api.CreateArtifactRequest{Slug: "by-default", Files: files}), http.StatusCreated, &res)
	if res.Artifact.Visibility != "members" {
		t.Errorf("visibility of a page created without the share scope = %q, want the instance default", res.Artifact.Visibility)
	}
	e.expect(e.jsonReq(http.MethodPut, e.u("/v1/artifacts/demo"), writer, map[string]any{"name": "renamed", "files": files}), http.StatusOK, &res)
	if res.Version != 2 || res.Artifact.Name != "renamed" || res.Artifact.Visibility != "members" {
		t.Errorf("update without the share scope = %+v", res)
	}
	e.expect(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/stale"), writer, nil), http.StatusOK, nil)
	e.expect(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/share-links"), token: writer}), http.StatusOK, nil)

	// The share scope alone changes who can open a page and nothing else.
	e.expect(e.jsonReq(http.MethodPatch, e.u("/v1/artifacts/demo"), sharer, map[string]any{"visibility": "unlisted"}), http.StatusOK, &res)
	if res.Artifact.Visibility != "unlisted" {
		t.Errorf("visibility = %q, want unlisted", res.Artifact.Visibility)
	}
	var link api.CreatedShareLink
	e.expect(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/share-links"), sharer, nil), http.StatusCreated, &link)
	e.expect(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/demo/share-links/" + link.ShareLink.Prefix), token: sharer}), http.StatusNoContent, nil)
	e.apiErr(e.jsonReq(http.MethodPut, e.u("/v1/artifacts/demo"), sharer, map[string]any{"files": files}), http.StatusForbidden, api.CodeForbidden)

	// A key issued without naming scopes holds it, as before the split.
	e.expect(e.jsonReq(http.MethodPatch, e.u("/v1/artifacts/demo"), e.alice, map[string]any{"visibility": "public"}), http.StatusOK, nil)
}

// oauthAs authenticates one fixed token as an OAuth connection of handle.
func oauthAs(token, handle string) auth.Authenticator {
	return auth.AuthenticatorFunc(func(_ context.Context, got string) (auth.Principal, error) {
		if got != token {
			return auth.Principal{}, auth.ErrInvalidToken
		}
		return auth.Principal{Handle: handle, Scopes: auth.MemberScopes, Source: auth.SourceOAuth, KeyID: "oauth:7"}, nil
	})
}
