package cli_test

import (
	"encoding/json"
	"net/http"
	"path"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
)

func TestRunVersions(t *testing.T) {
	var deleted []string
	var prune api.PruneRequest
	del := func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, path.Base(r.URL.Path))
		reply(w, 200, api.DeletedVersions{Deleted: []int{2}, Artifact: demo})
	}
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /v1/artifacts/hello/versions/2": del,
		"DELETE /v1/artifacts/hello/versions/3": del,
		"POST /v1/artifacts/hello/versions/prune": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&prune); err != nil {
				t.Errorf("prune body: %v", err)
			}
			reply(w, 200, api.DeletedVersions{Deleted: []int{1, 3}, DryRun: prune.DryRun, Artifact: demo})
		},
	})
	base := []string{"--url", srv.URL, "--token", "secret-key"}

	r := runCLI(t, nil, append(base, "versions", "rm", "hello", "2", "3")...)
	if want := "deleted version 2 of hello\ndeleted version 3 of hello\n"; r.code != 0 || r.stdout != want {
		t.Errorf("versions rm hello 2 3: %+v, want stdout %q", r, want)
	}
	if len(deleted) != 2 || deleted[0] != "2" || deleted[1] != "3" {
		t.Errorf("versions rm sent deletes for %v, want [2 3]", deleted)
	}
	r = runCLI(t, nil, append(base, "versions", "rm", "hello", "x")...)
	if r.code != exitUsage {
		t.Errorf("versions rm hello x: exit %d, want %d", r.code, exitUsage)
	}

	r = runCLI(t, nil, append(base, "versions", "prune", "hello", "--keep", "2", "--dry-run")...)
	if want := "would delete versions 1, 3 of hello\n"; r.code != 0 || r.stdout != want {
		t.Errorf("versions prune --dry-run: %+v, want stdout %q", r, want)
	}
	if prune.Keep != 2 || !prune.DryRun {
		t.Errorf("versions prune --keep 2 --dry-run sent %+v", prune)
	}
	r = runCLI(t, nil, append(base, "versions", "prune", "hello", "--keep", "2")...)
	if want := "deleted versions 1, 3 of hello\n"; r.code != 0 || r.stdout != want {
		t.Errorf("versions prune: %+v, want stdout %q", r, want)
	}
	r = runCLI(t, nil, append(base, "versions", "prune", "hello", "--keep", "0")...)
	if r.code != exitUsage {
		t.Errorf("versions prune --keep 0: exit %d, want %d", r.code, exitUsage)
	}
}
