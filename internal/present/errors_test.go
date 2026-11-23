package present_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

// errorRows has a row for every error sentinel of the packages whose errors
// reach Error. A sentinel that another package answers before Error sees it
// says where in elsewhere; Error then knows nothing of it.
var errorRows = []struct {
	name      string
	err       error
	status    int
	code      string
	elsewhere string
}{
	{name: "artifact.ErrNotFound", err: artifact.ErrNotFound, status: http.StatusNotFound, code: api.CodeNotFound},
	{name: "artifact.ErrVersionNotFound", err: artifact.ErrVersionNotFound, status: http.StatusNotFound, code: api.CodeVersionNotFound},
	{name: "artifact.ErrFileNotFound", err: artifact.ErrFileNotFound, status: http.StatusNotFound, code: api.CodeFileNotFound},
	{name: "artifact.ErrSlugTaken", err: artifact.ErrSlugTaken, status: http.StatusConflict, code: api.CodeSlugTaken},
	{name: "artifact.ErrVersionConflict", err: artifact.ErrVersionConflict, status: http.StatusPreconditionFailed, code: api.CodeVersionConflict},
	{name: "artifact.ErrPublishedNotLatest", err: artifact.ErrPublishedNotLatest, status: http.StatusConflict, code: api.CodePublishedNotLatest},
	{name: "artifact.ErrTooLarge", err: artifact.ErrTooLarge, status: http.StatusRequestEntityTooLarge, code: api.CodeTooLarge},
	{name: "artifact.ErrTooManyFiles", err: artifact.ErrTooManyFiles, status: http.StatusUnprocessableEntity, code: api.CodeTooManyFiles},
	{name: "artifact.ErrQuotaExceeded", err: artifact.ErrQuotaExceeded, status: http.StatusForbidden, code: api.CodeQuotaExceeded},
	{name: "artifact.ErrInvalidSlug", err: artifact.ErrInvalidSlug, status: http.StatusUnprocessableEntity, code: api.CodeInvalidSlug},
	{name: "artifact.ErrInvalidPath", err: artifact.ErrInvalidPath, status: http.StatusUnprocessableEntity, code: api.CodeInvalidPath},
	{name: "artifact.ErrInvalidMetadata", err: artifact.ErrInvalidMetadata, status: http.StatusUnprocessableEntity, code: api.CodeInvalidMetadata},
	{name: "artifact.ErrInvalidArgument", err: artifact.ErrInvalidArgument, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "artifact.ErrForbidden", err: artifact.ErrForbidden, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "artifact.ErrGone", err: artifact.ErrGone, status: http.StatusGone, code: api.CodeGone},
	{name: "artifact.ErrOffsetMismatch", err: artifact.ErrOffsetMismatch, status: http.StatusConflict, code: api.CodeOffsetMismatch},
	{name: "artifact.ErrUploadNotFound", err: artifact.ErrUploadNotFound, status: http.StatusNotFound, code: api.CodeUploadNotFound},
	{name: "artifact.ErrLinkNotFound", err: artifact.ErrLinkNotFound, status: http.StatusNotFound, code: api.CodeLinkNotFound},
	{name: "artifact.ErrLinkPassword", err: artifact.ErrLinkPassword,
		elsewhere: "a marker the share-link forms match; the refusals it marks carry invalid_argument"},
	{name: "artifact.ErrTooManyUploads", err: artifact.ErrTooManyUploads, status: http.StatusForbidden, code: api.CodeTooManyUploads},
	{name: "artifact.ErrDigestMismatch", err: artifact.ErrDigestMismatch, status: http.StatusUnprocessableEntity, code: api.CodeDigestMismatch},
	{name: "artifact.ErrEditNotFound", err: artifact.ErrEditNotFound, status: http.StatusUnprocessableEntity, code: api.CodeEditNotFound},
	{name: "artifact.ErrEditAmbiguous", err: artifact.ErrEditAmbiguous, status: http.StatusUnprocessableEntity, code: api.CodeEditAmbiguous},
	{name: "artifact.ErrAnchorMismatch", err: artifact.ErrAnchorMismatch, status: http.StatusConflict, code: api.CodeAnchorMismatch},
	{name: "artifact.ErrFetchFailed", err: artifact.ErrFetchFailed, status: http.StatusBadGateway, code: api.CodeFetchFailed},
	{name: "artifact.ErrFileRead", err: artifact.ErrFileRead, elsewhere: "httpapi: it ends a body that is already being streamed"},

	{name: "auth.ErrInvalidToken", err: auth.ErrInvalidToken, status: http.StatusUnauthorized, code: api.CodeUnauthorized},

	{name: "user.ErrNotFound", err: user.ErrNotFound, status: http.StatusNotFound, code: api.CodeNotFound},
	{name: "user.ErrKeyNotFound", err: user.ErrKeyNotFound, status: http.StatusNotFound, code: api.CodeNotFound},
	{name: "user.ErrHandleTaken", err: user.ErrHandleTaken, status: http.StatusConflict, code: api.CodeHandleTaken},
	{name: "user.ErrEmailTaken", err: user.ErrEmailTaken, status: http.StatusConflict, code: api.CodeEmailTaken},
	{name: "user.ErrInvalidHandle", err: user.ErrInvalidHandle, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrInvalidRole", err: user.ErrInvalidRole, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrInvalidScope", err: user.ErrInvalidScope, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrInvalidKey", err: user.ErrInvalidKey, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrKeyTooWide", err: user.ErrKeyTooWide, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "user.ErrDisabled", err: user.ErrDisabled, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "user.ErrNotProvisioned", err: user.ErrNotProvisioned, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "user.ErrLastAdmin", err: user.ErrLastAdmin, status: http.StatusConflict, code: api.CodeLastAdmin},
	{name: "user.ErrSelf", err: user.ErrSelf, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "user.ErrTooManyKeys", err: user.ErrTooManyKeys, status: http.StatusForbidden, code: api.CodeTooManyKeys},
	{name: "user.ErrBadPassword", err: user.ErrBadPassword, status: http.StatusUnauthorized, code: api.CodeUnauthorized},
	{name: "user.ErrWeakPassword", err: user.ErrWeakPassword, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrLinkNotFound", err: user.ErrLinkNotFound, status: http.StatusNotFound, code: api.CodeNotFound},
	{name: "user.ErrSetupDone", err: user.ErrSetupDone, status: http.StatusConflict, code: api.CodeSetupDone},
	{name: "user.ErrBusy", err: user.ErrBusy, status: http.StatusServiceUnavailable, code: api.CodeBusy},
	{name: "user.ErrHasPassword", err: user.ErrHasPassword, status: http.StatusUnprocessableEntity, code: api.CodeInvalidArgument},
	{name: "user.ErrLocalOff", err: user.ErrLocalOff, status: http.StatusForbidden, code: api.CodeForbidden},
	{name: "user.ErrInvalidIdentity", err: user.ErrInvalidIdentity, elsewhere: "httpapi: only the admin API links identities"},
	{name: "user.ErrBadRecoveryCode", err: user.ErrBadRecoveryCode, elsewhere: "account: the recovery page"},
	{name: "user.ErrPasskeyNotFound", err: user.ErrPasskeyNotFound, elsewhere: "account: the passkey pages"},
	{name: "user.ErrPasskeyExists", err: user.ErrPasskeyExists, elsewhere: "account: the passkey pages"},
	{name: "user.ErrTooManyPasskeys", err: user.ErrTooManyPasskeys, elsewhere: "account: the passkey pages"},
	{name: "user.ErrPasskeyStale", err: user.ErrPasskeyStale, elsewhere: "account: the passkey pages"},

	{name: "upload.OffsetMismatchError", err: &upload.OffsetMismatchError{Path: "a", Current: 7}, status: http.StatusConflict, code: api.CodeOffsetMismatch},
	{name: "http.MaxBytesError", err: &http.MaxBytesError{Limit: 10}, status: http.StatusRequestEntityTooLarge, code: api.CodeTooLarge},
	{name: "an error no package declares", err: errors.New("boom"), status: http.StatusInternalServerError, code: api.CodeInternal},
}

// sentinels lists the exported Err variables a package declares, as
// "pkg.ErrName", read from its source.
func sentinels(t *testing.T, pkg string) []string {
	t.Helper()
	dir := filepath.Join("..", pkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				for _, name := range s.(*ast.ValueSpec).Names {
					if strings.HasPrefix(name.Name, "Err") {
						names = append(names, pkg+"."+name.Name)
					}
				}
			}
		}
	}
	return names
}

func TestErrorRowForEverySentinel(t *testing.T) {
	var want []string
	for _, pkg := range []string{"artifact", "auth", "upload", "user"} {
		want = append(want, sentinels(t, pkg)...)
	}
	if len(want) == 0 {
		t.Fatal("found no sentinels in the packages' source")
	}
	var got []string
	for _, row := range errorRows {
		if _, name, ok := strings.Cut(row.name, "."); ok && strings.HasPrefix(name, "Err") {
			got = append(got, row.name)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("errorRows differs from the declared sentinels (-declared +rows):\n%s", diff)
	}
}

func TestError(t *testing.T) {
	for _, tt := range errorRows {
		t.Run(tt.name, func(t *testing.T) {
			wantStatus, wantCode := tt.status, tt.code
			if tt.elsewhere != "" {
				wantStatus, wantCode = http.StatusInternalServerError, api.CodeInternal
			}
			status, body := present.Error(tt.err)
			if status != wantStatus || body.Code != wantCode {
				t.Errorf("Error(%v) = %d %s, want %d %s", tt.err, status, body.Code, wantStatus, wantCode)
			}
			if !slices.Contains(api.ErrorCodes, body.Code) {
				t.Errorf("Error(%v) has code %q, which api.ErrorCodes does not list", tt.err, body.Code)
			}
			if body.Message == "" {
				t.Errorf("Error(%v) has no message", tt.err)
			}

			wrapped := fmt.Errorf("list things: %w", tt.err)
			status, body = present.Error(wrapped)
			if status != wantStatus || body.Code != wantCode {
				t.Errorf("Error(%v) = %d %s, want %d %s", wrapped, status, body.Code, wantStatus, wantCode)
			}
		})
	}
}

func TestErrorDetail(t *testing.T) {
	t.Run("an artifact error keeps its own message", func(t *testing.T) {
		err := artifact.Errorf(artifact.ErrSlugTaken, "slug %q is already taken", "demo")
		status, body := present.Error(err)
		want := api.Error{Code: api.CodeSlugTaken, Message: `slug "demo" is already taken`}
		if diff := cmp.Diff(want, body); status != http.StatusConflict || diff != "" {
			t.Errorf("Error(%v) = %d, want %d; body (-want +got):\n%s", err, status, http.StatusConflict, diff)
		}
	})
	t.Run("an offset mismatch carries the staged size", func(t *testing.T) {
		err := fmt.Errorf("append: %w", &upload.OffsetMismatchError{Path: "a", Current: 7})
		_, body := present.Error(err)
		if body.Current == nil || *body.Current != 7 {
			t.Errorf("Error(%v).Current = %v, want 7", err, body.Current)
		}
	})
	t.Run("a body over the limit names the limit", func(t *testing.T) {
		err := &http.MaxBytesError{Limit: 10}
		_, body := present.Error(err)
		if want := "request body exceeds 10 B"; body.Message != want {
			t.Errorf("Error(%v).Message = %q, want %q", err, body.Message, want)
		}
	})
	t.Run("an unknown error keeps its text to itself", func(t *testing.T) {
		err := errors.New("secret detail")
		_, body := present.Error(err)
		if want := "internal error"; body.Message != want {
			t.Errorf("Error(%v).Message = %q, want %q", err, body.Message, want)
		}
	})
	t.Run("a code Error does not know is a server error", func(t *testing.T) {
		err := &artifact.Error{Code: "made_up", Message: "x"}
		if status, _ := present.Error(err); status != http.StatusInternalServerError {
			t.Errorf("Error(%v) = %d, want %d", err, status, http.StatusInternalServerError)
		}
	})
}
