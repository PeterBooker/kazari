package upload_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/dbtemplate"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/upload"
)

// emptyDB is a database at the latest migration, migrated once for the
// package's tests and copied for each.
var emptyDB = dbtemplate.New(func(ctx context.Context, path string) error {
	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		return err
	}
	return db.Close()
})

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	svc   *upload.Service
	store *sqlite.UploadStore
	dir   string
	clock *clock
}

func newEnv(t *testing.T, opts upload.Options) *env {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "kazari.db")
	emptyDB.Copy(t, path)
	db, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, h := range []string{"alice", "bob"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES (?, 'member', 0, ?)`,
			h, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	c := &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	opts.Dir = filepath.Join(tmp, "uploads")
	opts.Now = c.Now
	store := sqlite.NewUploadStore(db)
	return &env{svc: upload.New(store, opts), store: store, dir: opts.Dir, clock: c}
}

func (e *env) begin(t *testing.T, owner string) *upload.Upload {
	t.Helper()
	u, err := e.svc.Begin(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *env) appendChunk(t *testing.T, id, path string, offset int64, data string) upload.StagedFile {
	t.Helper()
	f, err := e.svc.AppendChunk(context.Background(), "alice", id, path, offset, strings.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func readFiles(t *testing.T, files []artifact.FileInput) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, f := range files {
		b, err := io.ReadAll(f.Content)
		if err != nil {
			t.Fatal(err)
		}
		got[f.Path] = string(b)
	}
	return got
}

func TestBegin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{TTL: 30 * time.Minute, MaxOpenPerOwner: 2})
	u := e.begin(t, "alice")
	if len(u.ID) != 22 || strings.ContainsAny(u.ID, "+/=") {
		t.Errorf("ID = %q, want 22 base64url characters", u.ID)
	}
	if want := e.clock.Now().Add(30 * time.Minute); !u.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", u.ExpiresAt, want)
	}
	if !exists(t, filepath.Join(e.dir, u.ID)) {
		t.Error("staging directory not created")
	}
	got, err := e.svc.Get(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != "alice" || got.Bytes != 0 || len(got.Files) != 0 {
		t.Errorf("Get = %+v, want an empty upload of alice", got)
	}

	if u2 := e.begin(t, "alice"); u2.ID == u.ID {
		t.Errorf("second upload reused id %q", u.ID)
	}
	if _, err := e.svc.Begin(ctx, "alice"); !errors.Is(err, artifact.ErrTooManyUploads) {
		t.Errorf("third Begin: err = %v, want ErrTooManyUploads", err)
	}
	e.begin(t, "bob")
	if _, err := e.svc.Begin(ctx, "nobody"); err == nil {
		t.Error("Begin for unknown owner succeeded")
	}
}

func TestBeginConcurrentLimit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{MaxOpenPerOwner: 5})
	const tries = 30
	errs := make([]error, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() { _, errs[i] = e.svc.Begin(ctx, "alice") })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, artifact.ErrTooManyUploads):
		default:
			t.Errorf("concurrent Begin: %v", err)
		}
	}
	n, err := e.store.CountAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wins != 5 || n != 5 {
		t.Fatalf("%d concurrent Begins succeeded and %d uploads are open, want 5 and 5", wins, n)
	}
}

func TestAppendChunk(t *testing.T) {
	type step struct {
		owner       string // default alice
		advance     time.Duration
		id          string // default the upload's id
		path        string
		offset      int64
		data        string
		contentType string
		want        upload.StagedFile
		wantErr     error
		wantCurrent int64
	}
	tests := []struct {
		name      string
		opts      upload.Options
		steps     []step
		wantFiles map[string]string // nil skips the content check
	}{
		{
			name: "in order",
			steps: []step{
				{path: "a.txt", offset: 0, data: "hello ", want: upload.StagedFile{Path: "a.txt", Bytes: 6}},
				{path: "a.txt", offset: 6, data: "world", want: upload.StagedFile{Path: "a.txt", Bytes: 11}},
				{path: "b.txt", offset: 0, data: "b", want: upload.StagedFile{Path: "b.txt", StagingIndex: 1, Bytes: 1}},
			},
			wantFiles: map[string]string{"a.txt": "hello world", "b.txt": "b"},
		},
		{
			name: "retry of the last chunk is acknowledged without growth",
			steps: []step{
				{path: "a.txt", offset: 0, data: "hello ", want: upload.StagedFile{Path: "a.txt", Bytes: 6}},
				{path: "a.txt", offset: 6, data: "world", want: upload.StagedFile{Path: "a.txt", Bytes: 11}},
				{path: "a.txt", offset: 6, data: "world", want: upload.StagedFile{Path: "a.txt", Bytes: 11}},
			},
			wantFiles: map[string]string{"a.txt": "hello world"},
		},
		{
			name: "empty file",
			steps: []step{
				{path: "empty", offset: 0, data: "", want: upload.StagedFile{Path: "empty"}},
			},
			wantFiles: map[string]string{"empty": ""},
		},
		{
			name: "offset ahead of the staged size",
			steps: []step{
				{path: "a.txt", offset: 0, data: "abc", want: upload.StagedFile{Path: "a.txt", Bytes: 3}},
				{path: "a.txt", offset: 5, data: "x", wantErr: artifact.ErrOffsetMismatch, wantCurrent: 3},
			},
			wantFiles: map[string]string{"a.txt": "abc"},
		},
		{
			name: "offset behind without ending at the staged size",
			steps: []step{
				{path: "a.txt", offset: 0, data: "abcdef", want: upload.StagedFile{Path: "a.txt", Bytes: 6}},
				{path: "a.txt", offset: 2, data: "x", wantErr: artifact.ErrOffsetMismatch, wantCurrent: 6},
			},
			wantFiles: map[string]string{"a.txt": "abcdef"},
		},
		{
			name: "first chunk not at zero",
			steps: []step{
				{path: "a.txt", offset: 4, data: "x", wantErr: artifact.ErrOffsetMismatch, wantCurrent: 0},
			},
		},
		{
			name: "negative offset",
			steps: []step{
				{path: "a.txt", offset: -1, data: "x", wantErr: artifact.ErrInvalidArgument},
			},
		},
		{
			name: "chunk cap",
			opts: upload.Options{MaxChunkBytes: 4},
			steps: []step{
				{path: "a.txt", offset: 0, data: "abcd", want: upload.StagedFile{Path: "a.txt", Bytes: 4}},
				{path: "a.txt", offset: 4, data: "efghi", wantErr: artifact.ErrTooLarge},
			},
			wantFiles: map[string]string{"a.txt": "abcd"},
		},
		{
			name: "total cap across files",
			opts: upload.Options{MaxVersionBytes: 10},
			steps: []step{
				{path: "a.txt", offset: 0, data: "123456", want: upload.StagedFile{Path: "a.txt", Bytes: 6}},
				{path: "b.txt", offset: 0, data: "12345", wantErr: artifact.ErrTooLarge},
				{path: "b.txt", offset: 0, data: "1234", want: upload.StagedFile{Path: "b.txt", StagingIndex: 1, Bytes: 4}},
			},
			wantFiles: map[string]string{"a.txt": "123456", "b.txt": "1234"},
		},
		{
			name: "file count cap",
			opts: upload.Options{MaxFiles: 2},
			steps: []step{
				{path: "a", data: "a", want: upload.StagedFile{Path: "a", Bytes: 1}},
				{path: "b", data: "b", want: upload.StagedFile{Path: "b", StagingIndex: 1, Bytes: 1}},
				{path: "c", data: "c", wantErr: artifact.ErrTooManyFiles},
				{path: "a", offset: 1, data: "a", want: upload.StagedFile{Path: "a", Bytes: 2}},
			},
			wantFiles: map[string]string{"a": "aa", "b": "b"},
		},
		{
			name: "invalid paths",
			steps: []step{
				{path: "../escape", data: "x", wantErr: artifact.ErrInvalidPath},
				{path: "/", data: "x", wantErr: artifact.ErrInvalidPath},
				{path: "dir/", data: "x", wantErr: artifact.ErrInvalidPath},
				{path: "", data: "x", wantErr: artifact.ErrInvalidPath},
			},
		},
		{
			name: "path is normalised",
			steps: []step{
				{path: "dir/./a.txt", data: "ab", want: upload.StagedFile{Path: "dir/a.txt", Bytes: 2}},
				{path: "dir/a.txt", offset: 2, data: "c", want: upload.StagedFile{Path: "dir/a.txt", Bytes: 3}},
			},
			wantFiles: map[string]string{"dir/a.txt": "abc"},
		},
		{
			name: "content type comes from the first chunk",
			steps: []step{
				{path: "a", data: "a", contentType: "text/plain",
					want: upload.StagedFile{Path: "a", Bytes: 1, ContentType: "text/plain"}},
				{path: "a", offset: 1, data: "b", contentType: "text/html",
					want: upload.StagedFile{Path: "a", Bytes: 2, ContentType: "text/plain"}},
			},
		},
		{
			// The type is replayed as a Content-Type header at publish.
			name: "invalid content types are refused",
			steps: []step{
				{path: "a", data: "a", contentType: "text/html\r\nSet-Cookie: a=b", wantErr: artifact.ErrInvalidArgument},
				{path: "a", data: "a", contentType: "text/html; charset=é", wantErr: artifact.ErrInvalidArgument},
				{path: "a", data: "a", contentType: "no slash here;;", wantErr: artifact.ErrInvalidArgument},
				{path: "a", data: "a", contentType: "text/" + strings.Repeat("a", artifact.ContentTypeMaxLen), wantErr: artifact.ErrInvalidArgument},
				{path: "a", data: "a", contentType: "text/plain; charset=utf-8",
					want: upload.StagedFile{Path: "a", Bytes: 1, ContentType: "text/plain; charset=utf-8"}},
				{path: "a", offset: 1, data: "b", contentType: "text/html\x00", wantErr: artifact.ErrInvalidArgument},
			},
			wantFiles: map[string]string{"a": "a"},
		},
		{
			name: "wrong owner, unknown and malformed ids are not found",
			steps: []step{
				{owner: "bob", path: "a", data: "a", wantErr: artifact.ErrUploadNotFound},
				{owner: "nobody", path: "a", data: "a", wantErr: artifact.ErrUploadNotFound},
				{id: "AAAAAAAAAAAAAAAAAAAAAA", path: "a", data: "a", wantErr: artifact.ErrUploadNotFound},
				{id: "../../etc", path: "a", data: "a", wantErr: artifact.ErrUploadNotFound},
			},
		},
		{
			name: "expired upload is not found",
			opts: upload.Options{TTL: time.Minute},
			steps: []step{
				{path: "a", data: "a", want: upload.StagedFile{Path: "a", Bytes: 1}},
				{advance: time.Minute, path: "a", offset: 1, data: "b", wantErr: artifact.ErrUploadNotFound},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t, tt.opts)
			u := e.begin(t, "alice")
			for i, st := range tt.steps {
				e.clock.Advance(st.advance)
				owner, id := st.owner, st.id
				if owner == "" {
					owner = "alice"
				}
				if id == "" {
					id = u.ID
				}
				got, err := e.svc.AppendChunk(ctx, owner, id, st.path, st.offset, strings.NewReader(st.data), st.contentType)
				if st.wantErr != nil {
					if !errors.Is(err, st.wantErr) {
						t.Fatalf("step %d: err = %v, want %v", i, err, st.wantErr)
					}
					if errors.Is(st.wantErr, artifact.ErrOffsetMismatch) {
						var om *upload.OffsetMismatchError
						if !errors.As(err, &om) {
							t.Fatalf("step %d: err = %T, want *OffsetMismatchError", i, err)
						}
						if om.Current != st.wantCurrent {
							t.Errorf("step %d: Current = %d, want %d", i, om.Current, st.wantCurrent)
						}
						if code := artifact.CodeOf(err); code != artifact.ErrOffsetMismatch.Code {
							t.Errorf("step %d: CodeOf = %q, want %q", i, code, artifact.ErrOffsetMismatch.Code)
						}
					}
					continue
				}
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if diff := cmp.Diff(st.want, got); diff != "" {
					t.Errorf("step %d: StagedFile (-want +got):\n%s", i, diff)
				}
			}

			if tt.wantFiles == nil {
				return
			}
			stored, err := e.store.Get(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, c := range tt.wantFiles {
				total += int64(len(c))
			}
			if stored.Bytes != total {
				t.Errorf("upload Bytes = %d, want %d", stored.Bytes, total)
			}
			files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup(true)
			if diff := cmp.Diff(tt.wantFiles, readFiles(t, files)); diff != "" {
				t.Errorf("staged content (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFiles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	steps := []struct {
		path, data, contentType string
		offset                  int64
	}{
		{"z/last.js", "console.log(1)", "text/javascript", 0},
		{"index.html", "<h1>", "text/html", 0},
		{"index.html", "hi</h1>", "", 4},
		{"a.css", "body{}", "", 0},
	}
	for _, st := range steps {
		if _, err := e.svc.AppendChunk(ctx, "alice", u.ID, st.path, st.offset, strings.NewReader(st.data), st.contentType); err != nil {
			t.Fatal(err)
		}
	}
	extra, err := os.OpenFile(filepath.Join(e.dir, u.ID, "0"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.WriteString("garbage"); err != nil {
		t.Fatal(err)
	}
	extra.Close()

	if _, _, err := e.svc.Files(ctx, "bob", u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Files for bob: err = %v, want ErrUploadNotFound", err)
	}
	files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var meta [][2]string
	for _, f := range files {
		meta = append(meta, [2]string{f.Path, f.ContentType})
	}
	wantMeta := [][2]string{
		{"a.css", ""},
		{"index.html", "text/html"},
		{"z/last.js", "text/javascript"},
	}
	if diff := cmp.Diff(wantMeta, meta); diff != "" {
		t.Errorf("paths and content types (-want +got):\n%s", diff)
	}
	wantContent := map[string]string{
		"a.css":      "body{}",
		"index.html": "<h1>hi</h1>",
		"z/last.js":  "console.log(1)",
	}
	if diff := cmp.Diff(wantContent, readFiles(t, files)); diff != "" {
		t.Errorf("content (-want +got):\n%s", diff)
	}

	if err := cleanup(true); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if exists(t, filepath.Join(e.dir, u.ID)) {
		t.Error("staging directory survived cleanup")
	}
	if _, err := e.store.Get(ctx, u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("store Get after cleanup: err = %v, want ErrUploadNotFound", err)
	}
	if err := cleanup(true); err != nil {
		t.Errorf("second cleanup: %v", err)
	}
}

// A publish that was refused leaves the upload as it was, for the corrected
// call to use.
func TestFilesKeptWhenNotConsumed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	e.appendChunk(t, u.ID, "index.html", 0, "<h1>hi</h1>")
	_, done, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := done(false); err != nil {
		t.Fatalf("done(false): %v", err)
	}
	files, done, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatalf("Files after a refused publish: %v", err)
	}
	if diff := cmp.Diff(map[string]string{"index.html": "<h1>hi</h1>"}, readFiles(t, files)); diff != "" {
		t.Errorf("content after a refused publish (-want +got):\n%s", diff)
	}
	if err := done(true); err != nil {
		t.Fatalf("done(true): %v", err)
	}
	if _, _, err := e.svc.Files(ctx, "alice", u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Files after a publish: err = %v, want ErrUploadNotFound", err)
	}
}

// A chunk that arrives after Files opened the staged file, which the publish
// may still be reading, is not published: Content stops at the recorded size.
func TestFilesCapAtRecordedSize(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	e.appendChunk(t, u.ID, "a", 0, "abc")
	files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup(true)
	e.appendChunk(t, u.ID, "a", 3, "def")
	if diff := cmp.Diff(map[string]string{"a": "abc"}, readFiles(t, files)); diff != "" {
		t.Errorf("content (-want +got):\n%s", diff)
	}
}

// Files opens no staging file: each opens when the publish reads it, so a
// publish of many files does not hold them all open at once.
func TestFilesOpenOnRead(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	staged := map[string]upload.StagedFile{}
	for p, body := range map[string]string{"a": "abc", "b": "bcde", "c": "cdefg"} {
		staged[p] = e.appendChunk(t, u.ID, p, 0, body)
	}
	files, done, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer done(false)

	if b, err := io.ReadAll(files[0].Content); err != nil || string(b) != "abc" {
		t.Fatalf("read %s = %q, %v; want %q", files[0].Path, b, err, "abc")
	}
	// Gone before it is read: an open file would still give its content.
	if err := os.Remove(filepath.Join(e.dir, u.ID, strconv.Itoa(staged["b"].StagingIndex))); err != nil {
		t.Fatal(err)
	}
	if n, ok := files[1].Content.(interface{ Size() int64 }); !ok || n.Size() != 4 {
		t.Errorf("%s: Size before reading = %v, want 4", files[1].Path, n)
	}
	if _, err := io.ReadAll(files[1].Content); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("read %s after its staging file is removed: err = %v, want ErrUploadNotFound", files[1].Path, err)
	}
	// Cut short after Files checked it: an error, not a shorter file.
	if err := os.Truncate(filepath.Join(e.dir, u.ID, strconv.Itoa(staged["c"].StagingIndex)), 2); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(files[2].Content); err == nil {
		t.Errorf("read %s after its staging file is cut to 2 bytes = %q, nil; want an error", files[2].Path, b)
	}
	if err := done(false); err != nil {
		t.Errorf("done: %v", err)
	}
}

func TestFilesEmptyUpload(t *testing.T) {
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	if _, _, err := e.svc.Files(context.Background(), "alice", u.ID); !errors.Is(err, artifact.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestAbort(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	e.appendChunk(t, u.ID, "a", 0, "abc")

	tests := []struct {
		name    string
		owner   string
		wantErr error
	}{
		{"wrong owner", "bob", artifact.ErrUploadNotFound},
		{"owner", "alice", nil},
		{"already aborted", "alice", artifact.ErrUploadNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := e.svc.Abort(ctx, tt.owner, u.ID); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	if exists(t, filepath.Join(e.dir, u.ID)) {
		t.Error("staging directory survived Abort")
	}
	if _, err := e.store.Get(ctx, u.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("store Get after Abort: err = %v, want ErrUploadNotFound", err)
	}
	if n, err := e.store.CountAll(ctx); err != nil || n != 0 {
		t.Errorf("CountAll = %d, %v; want 0, nil", n, err)
	}
}

func TestExpireStale(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{TTL: time.Hour})

	if n, err := e.svc.ExpireStale(ctx); err != nil || n != 0 {
		t.Fatalf("ExpireStale on empty = %d, %v; want 0, nil", n, err)
	}

	old := e.begin(t, "alice")
	e.appendChunk(t, old.ID, "a", 0, "abc")
	e.clock.Advance(30 * time.Minute)
	fresh := e.begin(t, "alice")
	e.appendChunk(t, fresh.ID, "a", 0, "abc")
	e.clock.Advance(31 * time.Minute)

	orphan := filepath.Join(e.dir, "orphanorphanorphan0000")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "0"), []byte("leaked"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(e.dir, "not-an-upload")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := e.svc.ExpireStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2 (one expired, one orphan)", n)
	}
	present := map[string]bool{
		"expired": exists(t, filepath.Join(e.dir, old.ID)),
		"fresh":   exists(t, filepath.Join(e.dir, fresh.ID)),
		"orphan":  exists(t, orphan),
		"foreign": exists(t, foreign),
	}
	wantPresent := map[string]bool{"expired": false, "fresh": true, "orphan": false, "foreign": true}
	if diff := cmp.Diff(wantPresent, present); diff != "" {
		t.Errorf("directories present (-want +got):\n%s", diff)
	}
	if _, err := e.store.Get(ctx, old.ID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("expired row: err = %v, want ErrUploadNotFound", err)
	}
	if _, err := e.svc.Get(ctx, "alice", fresh.ID); err != nil {
		t.Errorf("fresh upload: %v", err)
	}

	if n, err := e.svc.ExpireStale(ctx); err != nil || n != 0 {
		t.Errorf("second ExpireStale = %d, %v; want 0, nil", n, err)
	}
}

func TestAppendChunkConcurrent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, upload.Options{})
	u := e.begin(t, "alice")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := e.svc.AppendChunk(ctx, "alice", u.ID, "a", 0, strings.NewReader("abc"), "")
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	files, cleanup, err := e.svc.Files(ctx, "alice", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup(true)
	if diff := cmp.Diff(map[string]string{"a": "abc"}, readFiles(t, files)); diff != "" {
		t.Errorf("content (-want +got):\n%s", diff)
	}
}
