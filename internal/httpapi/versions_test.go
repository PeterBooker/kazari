package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
)

func TestDeleteAndPruneVersions(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo")
	for _, body := range []string{"v2", "v3", "v4"} {
		e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice,
			api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "index.html", Content: body}}}, http.StatusOK, nil)
	}

	var res api.DeletedVersions
	e.call(http.MethodDelete, e.u("/v1/artifacts/demo/versions/2"), e.alice, nil, http.StatusOK, &res)
	if !cmp.Equal(res.Deleted, []int{2}) || res.Artifact.LatestVersion != 4 {
		t.Errorf("DELETE versions/2 = %+v, want version 2 deleted and latest 4", res)
	}
	e.apiErr(e.jsonReq(http.MethodDelete, e.u("/v1/artifacts/demo/versions/4"), e.alice, nil), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(e.jsonReq(http.MethodDelete, e.u("/v1/artifacts/demo/versions/2"), e.alice, nil), http.StatusNotFound, api.CodeVersionNotFound)
	e.apiErr(e.jsonReq(http.MethodDelete, e.u("/v1/artifacts/demo/versions/x"), e.alice, nil), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/demo/files/index.html?version=2"), e.alice, nil), http.StatusNotFound, api.CodeVersionNotFound)

	e.call(http.MethodPost, e.u("/v1/artifacts/demo/versions/prune"), e.alice, api.PruneRequest{Keep: 1, DryRun: true}, http.StatusOK, &res)
	if !cmp.Equal(res.Deleted, []int{1, 3}) || !res.DryRun {
		t.Errorf("prune keep 1, dry run = %+v, want versions 1 and 3, dry run", res)
	}
	bob := e.member("bob", []string{auth.ScopeRead, auth.ScopeWrite})
	e.apiErr(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/versions/prune"), bob, api.PruneRequest{Keep: 1}), http.StatusForbidden, api.CodeForbidden)
	e.apiErr(e.jsonReq(http.MethodPost, e.u("/v1/artifacts/demo/versions/prune"), e.alice, api.PruneRequest{}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)

	e.call(http.MethodPost, e.u("/v1/artifacts/demo/versions/prune"), e.alice, api.PruneRequest{Keep: 1}, http.StatusOK, &res)
	if !cmp.Equal(res.Deleted, []int{1, 3}) || res.DryRun {
		t.Errorf("prune keep 1 = %+v, want versions 1 and 3 deleted", res)
	}
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/versions/prune"), e.alice, api.PruneRequest{Keep: 1}, http.StatusOK, &res)
	if res.Deleted == nil || len(res.Deleted) != 0 {
		t.Errorf("prune with nothing left = %#v, want an empty list", res.Deleted)
	}
}
