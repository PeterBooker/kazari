package httpapi_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
)

func TestShareLinks(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) { r.Visibility = "members" })
	links := e.u("/v1/artifacts/demo/share-links")

	var created api.CreatedShareLink
	e.call(http.MethodPost, links, e.alice, api.ShareLinkRequest{Label: "for the client", ExpiresIn: "3d"}, http.StatusCreated, &created)
	u, err := url.Parse(created.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := u.Query().Get("token")
	if u.Scheme+"://"+u.Host+u.Path != "https://demo.art.test/.kazari/share" || !artifact.IsLinkToken(token) {
		t.Fatalf("url = %q", created.URL)
	}
	sl := created.ShareLink
	if !strings.Contains(token, sl.Prefix) || sl.Label != "for the client" || sl.CreatedBy != "alice" ||
		!sl.ExpiresAt.Equal(t0.Add(72*time.Hour)) || !sl.Active || sl.Uses != 0 {
		t.Fatalf("share link = %+v", sl)
	}
	if created.Artifact.Visibility != "members" {
		t.Fatalf("artifact = %+v", created.Artifact)
	}

	// The default lifetime is a week; an empty body is fine.
	var second api.CreatedShareLink
	e.call(http.MethodPost, links, e.alice, nil, http.StatusCreated, &second)
	if !second.ShareLink.ExpiresAt.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("default expiry = %v", second.ShareLink.ExpiresAt)
	}

	var list api.ShareLinkList
	e.call(http.MethodGet, links, e.alice, nil, http.StatusOK, &list)
	if len(list.Items) != 2 || list.Items[0].Prefix != second.ShareLink.Prefix || list.Items[1].Prefix != sl.Prefix {
		t.Fatalf("list = %+v", list)
	}
	resp := e.send(req{method: http.MethodGet, url: links, token: e.alice})
	if raw, _ := io.ReadAll(resp.Body); strings.Contains(string(raw), token) {
		t.Fatal("the listing holds a token")
	}

	e.call(http.MethodDelete, links+"/"+sl.Prefix, e.alice, nil, http.StatusNoContent, nil)
	e.call(http.MethodGet, links, e.alice, nil, http.StatusOK, &list)
	if list.Items[1].Active || list.Items[1].RevokedAt == nil {
		t.Fatalf("after revoke: %+v", list.Items[1])
	}
	e.apiErr(e.send(req{method: http.MethodDelete, url: links + "/zzzzzzzz", token: e.alice}), 404, api.CodeLinkNotFound)

	for _, bad := range []string{"soon", "-1h", "0d"} {
		e.apiErr(e.send(req{method: http.MethodPost, url: links, token: e.alice, contentType: "application/json",
			body: strings.NewReader(`{"expires_in":"` + bad + `"}`)}), 422, api.CodeInvalidArgument)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: links, token: e.alice, contentType: "application/json",
		body: strings.NewReader(`{"expires_in":"1000d"}`)}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/missing/share-links"), token: e.alice}), 404, api.CodeNotFound)

	// Creating and revoking need artifacts:write; listing needs read.
	reader := e.member("reader", []string{auth.ScopeRead})
	e.apiErr(e.send(req{method: http.MethodPost, url: links, token: reader}), 403, api.CodeForbidden)
	e.call(http.MethodGet, links, reader, nil, http.StatusOK, &list)
	e.apiErr(e.send(req{method: http.MethodDelete, url: links + "/" + second.ShareLink.Prefix, token: reader}), 403, api.CodeForbidden)
}

func TestMembersVisibility(t *testing.T) {
	e := newEnv(t, config{})
	res := e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) { r.Visibility = "members" })
	if res.Artifact.Visibility != "members" {
		t.Fatalf("created %q", res.Artifact.Visibility)
	}
	url := e.u("/v1/artifacts/demo")
	var out api.PublishResult
	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{Visibility: ptr("public")}, http.StatusOK, &out)
	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{Visibility: ptr("members")}, http.StatusOK, &out)
	if out.Artifact.Visibility != "members" {
		t.Fatalf("after PATCH: %q", out.Artifact.Visibility)
	}

	// Members artifacts are listed to every member.
	bob := e.member("bob", nil)
	var list api.ArtifactList
	e.call(http.MethodGet, e.u("/v1/artifacts"), bob, nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Slug != "demo" {
		t.Fatalf("bob's listing = %+v", list.Items)
	}
}
