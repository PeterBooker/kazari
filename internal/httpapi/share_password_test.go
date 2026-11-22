package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
)

// plainPasswords stands in for user.Service's hashing.
type plainPasswords struct{}

func (plainPasswords) HashPassword(_ context.Context, password string) (string, error) {
	return "hash:" + password, nil
}

func (plainPasswords) VerifyPassword(_ context.Context, hash, password string) (bool, error) {
	return hash == "hash:"+password, nil
}

func TestShareLinkPasswordAPI(t *testing.T) {
	e := newEnv(t, config{artifact: artifact.Options{Passwords: plainPasswords{}}})
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) { r.Visibility = "members" })

	var created api.CreatedShareLink
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/share-links"), e.alice,
		api.ShareLinkRequest{Label: "client", Password: "correct horse battery"}, http.StatusCreated, &created)
	if !created.ShareLink.Password {
		t.Errorf("created link = %+v, want password true", created.ShareLink)
	}
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/share-links"), e.alice, api.ShareLinkRequest{Label: "plain"}, http.StatusCreated, nil)

	var list api.ShareLinkList
	e.call(http.MethodGet, e.u("/v1/artifacts/demo/share-links"), e.alice, nil, http.StatusOK, &list)
	got := map[string]bool{}
	for _, l := range list.Items {
		got[l.Label] = l.Password
	}
	if !got["client"] || got["plain"] || len(got) != 2 {
		t.Errorf("listed links' password = %v, want client true and plain false", got)
	}

	apiErr := e.apiErr(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/share-links"), e.alice,
		api.ShareLinkRequest{Password: "too short"}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	if !strings.Contains(apiErr.Message, "password") {
		t.Errorf("a short password's error = %q, want it to name the password", apiErr.Message)
	}
}
