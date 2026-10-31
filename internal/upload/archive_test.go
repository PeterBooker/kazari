package upload_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/upload"
)

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (e *env) beginArchive(t *testing.T, owner, body string) (*upload.Upload, string) {
	t.Helper()
	u, token, err := e.svc.BeginArchive(context.Background(), owner, digest(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	return u, token
}

func inputs(files map[string]string) []artifact.FileInput {
	out := make([]artifact.FileInput, 0, len(files))
	for _, p := range slices.Sorted(maps.Keys(files)) {
		out = append(out, artifact.FileInput{Path: p, Content: strings.NewReader(files[p])})
	}
	return out
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestBeginArchive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{MaxTotalBytes: 1 << 20})
	u, token := e.beginArchive(t, "alice", "archive bytes")
	if len(token) != 43 || strings.ContainsAny(token, "+/=") {
		t.Errorf("token = %q, want 43 base64url characters", token)
	}
	a := u.Archive
	if a == nil || a.SHA256 != digest("archive bytes") || a.Bytes != 13 || a.TokenHash == "" || a.TokenHash == token {
		t.Fatalf("Archive = %+v", a)
	}
	got, err := e.svc.Archive(ctx, u.ID, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("Archive = %+v, %v", got, err)
	}
	if _, _, err := e.svc.BeginArchive(ctx, "alice", strings.ToUpper(digest("x")), 1); err != nil {
		t.Errorf("upper-case digest: %v", err)
	}
	for _, tt := range []struct {
		name string
		sum  string
		size int64
		want error
	}{
		{"short digest", "abc", 1, artifact.ErrInvalidArgument},
		{"not hex", strings.Repeat("g", 64), 1, artifact.ErrInvalidArgument},
		{"zero size", digest("x"), 0, artifact.ErrInvalidArgument},
		{"negative size", digest("x"), -1, artifact.ErrInvalidArgument},
		{"over the total", digest("x"), 1<<20 + 1, artifact.ErrTooLarge},
	} {
		if _, _, err := e.svc.BeginArchive(ctx, "alice", tt.sum, tt.size); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

// Every miss is the same not-found, and a miss changes nothing.
func TestArchiveMisses(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{TTL: time.Hour})
	u, token := e.beginArchive(t, "alice", "body")
	plain := e.begin(t, "alice")
	for name, tc := range map[string]struct{ id, token string }{
		"wrong token":    {u.ID, strings.Repeat("A", len(token))},
		"short token":    {u.ID, token[:10]},
		"unknown upload": {"AAAAAAAAAAAAAAAAAAAAAA", token},
		"malformed id":   {"../x", token},
		"chunked upload": {plain.ID, token},
	} {
		if _, err := e.svc.Archive(ctx, tc.id, tc.token); !errors.Is(err, artifact.ErrUploadNotFound) {
			t.Errorf("%s: Archive err = %v, want ErrUploadNotFound", name, err)
		}
		if _, err := e.svc.RejectArchive(ctx, tc.id, tc.token); !errors.Is(err, artifact.ErrUploadNotFound) {
			t.Errorf("%s: RejectArchive err = %v, want ErrUploadNotFound", name, err)
		}
		files := inputs(map[string]string{"index.html": "x"})
		if _, err := e.svc.ReceiveArchive(ctx, tc.id, tc.token, files); !errors.Is(err, artifact.ErrUploadNotFound) {
			t.Errorf("%s: ReceiveArchive err = %v, want ErrUploadNotFound", name, err)
		}
	}
	if _, err := e.svc.Archive(ctx, u.ID, token); err != nil {
		t.Fatalf("after misses: %v", err)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.svc.Archive(ctx, u.ID, token); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("expired: err = %v, want ErrUploadNotFound", err)
	}
}

func TestReceiveArchive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u, token := e.beginArchive(t, "alice", "body")
	e.appendChunk(t, u.ID, "notes.txt", 0, "chunked first")

	got, err := e.svc.ReceiveArchive(ctx, u.ID, token, inputs(map[string]string{"index.html": "<h1>hi</h1>", "./assets/app.js": "js"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len("chunked first") + len("<h1>hi</h1>") + len("js")); got.Bytes != want || got.Archive.ReceivedAt.IsZero() {
		t.Fatalf("received = %+v (archive %+v), want %d bytes", got, got.Archive, want)
	}
	var paths []string
	for _, f := range got.Files {
		paths = append(paths, f.Path)
	}
	if want := []string{"assets/app.js", "index.html", "notes.txt"}; !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}

	if _, err := e.svc.Archive(ctx, u.ID, token); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("spent token still opens: %v", err)
	}
	if _, err := e.svc.ReceiveArchive(ctx, u.ID, token, inputs(map[string]string{"again.txt": "x"})); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("second receive: err = %v, want ErrUploadNotFound", err)
	}

	e.appendChunk(t, u.ID, "more.txt", 0, "x")
	files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	want := map[string]string{"assets/app.js": "js", "index.html": "<h1>hi</h1>", "notes.txt": "chunked first", "more.txt": "x"}
	if diff := cmp.Diff(want, readFiles(t, files)); diff != "" {
		t.Errorf("Files (-want +got):\n%s", diff)
	}
}

// Once the token is accepted, a failure removes the upload: the bytes already
// matched, so sending them again could only fail the same way.
func TestReceiveArchiveFailureRemovesUpload(t *testing.T) {
	ctx := context.Background()
	coded := artifact.Errorf(artifact.ErrInvalidArgument, "entry cannot be read")
	tests := []struct {
		name  string
		opts  upload.Options
		files []artifact.FileInput
		want  error
	}{
		{"no files", upload.Options{}, nil, artifact.ErrInvalidArgument},
		{"escaping path", upload.Options{}, inputs(map[string]string{"../x": "y"}), artifact.ErrInvalidPath},
		{"path twice", upload.Options{}, []artifact.FileInput{
			{Path: "a", Content: strings.NewReader("1")}, {Path: "./a", Content: strings.NewReader("2")}}, artifact.ErrInvalidArgument},
		{"too many files", upload.Options{MaxFiles: 2}, inputs(map[string]string{"a": "1", "b": "2", "c": "3"}), artifact.ErrTooManyFiles},
		{"too large", upload.Options{MaxTotalBytes: 5}, inputs(map[string]string{"a": "123", "b": "456"}), artifact.ErrTooLarge},
		{"coded reader error", upload.Options{}, []artifact.FileInput{{Path: "a", Content: errReader{coded}}}, artifact.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.opts)
			u, token := e.beginArchive(t, "alice", "body")
			_, err := e.svc.ReceiveArchive(ctx, u.ID, token, tt.files)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if _, err := e.svc.Get(ctx, "alice", u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
				t.Errorf("upload kept: Get err = %v", err)
			}
			if exists(t, filepath.Join(e.dir, u.ID)) {
				t.Error("staging directory kept")
			}
		})
	}

	t.Run("plain reader error", func(t *testing.T) {
		e := newEnv(t, upload.Options{})
		u, token := e.beginArchive(t, "alice", "body")
		_, err := e.svc.ReceiveArchive(ctx, u.ID, token, []artifact.FileInput{{Path: "a", Content: errReader{errors.New("disk")}}})
		if err == nil || artifact.CodeOf(err) != "" {
			t.Fatalf("err = %v, want an uncoded error", err)
		}
		if _, err := e.svc.Get(ctx, "alice", u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
			t.Errorf("upload kept: Get err = %v", err)
		}
	})

	t.Run("path staged by a chunk", func(t *testing.T) {
		e := newEnv(t, upload.Options{})
		u, token := e.beginArchive(t, "alice", "body")
		e.appendChunk(t, u.ID, "index.html", 0, "chunked")
		_, err := e.svc.ReceiveArchive(ctx, u.ID, token, inputs(map[string]string{"index.html": "archived"}))
		if !errors.Is(err, artifact.ErrInvalidArgument) {
			t.Fatalf("err = %v, want ErrInvalidArgument", err)
		}
	})
}

func TestRejectArchive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u, token := e.beginArchive(t, "alice", "body")
	for want := upload.MaxArchiveAttempts - 1; want > 0; want-- {
		left, err := e.svc.RejectArchive(ctx, u.ID, token)
		if err != nil || left != want {
			t.Fatalf("RejectArchive = %d, %v; want %d, nil", left, err, want)
		}
		if _, err := e.svc.Archive(ctx, u.ID, token); err != nil {
			t.Fatalf("after %d attempts left: %v", left, err)
		}
	}
	left, err := e.svc.RejectArchive(ctx, u.ID, token)
	if err != nil || left != 0 {
		t.Fatalf("last RejectArchive = %d, %v; want 0, nil", left, err)
	}
	if _, err := e.svc.Archive(ctx, u.ID, token); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Archive err = %v, want ErrUploadNotFound", err)
	}
	if _, err := e.svc.Get(ctx, "alice", u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Get err = %v, want ErrUploadNotFound", err)
	}
	if exists(t, filepath.Join(e.dir, u.ID)) {
		t.Error("staging directory kept")
	}
}

// Bodies racing for one token: exactly one is received, the rest see the
// token spent without disturbing the upload.
func TestReceiveArchiveOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u, token := e.beginArchive(t, "alice", "body")
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.ReceiveArchive(ctx, u.ID, token, inputs(map[string]string{"index.html": "x"}))
		}()
	}
	wg.Wait()
	received := 0
	for _, err := range errs {
		switch {
		case err == nil:
			received++
		case !errors.Is(err, artifact.ErrUploadNotFound):
			t.Errorf("err = %v, want nil or ErrUploadNotFound", err)
		}
	}
	if received != 1 {
		t.Fatalf("%d bodies received, want 1", received)
	}
	files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if diff := cmp.Diff(map[string]string{"index.html": "x"}, readFiles(t, files)); diff != "" {
		t.Errorf("Files (-want +got):\n%s", diff)
	}
}
