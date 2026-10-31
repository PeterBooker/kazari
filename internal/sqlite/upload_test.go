package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/upload"
)

func uploadTestStore(t *testing.T, handles ...string) *UploadStore {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, h := range handles {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES (?, 'member', 0, ?)`,
			h, nowNanos()); err != nil {
			t.Fatal(err)
		}
	}
	return NewUploadStore(db)
}

var uploadT0 = time.Date(2026, 10, 24, 12, 0, 0, 123456789, time.UTC)

func uploadAt(id, owner string, expires time.Time) *upload.Upload {
	return &upload.Upload{ID: id, Owner: owner, CreatedAt: uploadT0, ExpiresAt: expires}
}

func TestUploadStoreCreateGet(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	want := uploadAt("u1", "alice", uploadT0.Add(time.Hour))
	if err := s.Create(ctx, want, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}
}

func TestUploadStoreCreateErrors(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	if err := s.Create(ctx, uploadAt("u1", "alice", uploadT0), 0); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		u    *upload.Upload
	}{
		{"unknown owner", uploadAt("u2", "nobody", uploadT0)},
		{"duplicate id", uploadAt("u1", "alice", uploadT0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Create(ctx, tt.u, 0); err == nil {
				t.Fatal("Create succeeded, want error")
			}
		})
	}
	if _, err := s.Get(ctx, "u2"); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Get after failed create: err = %v, want ErrUploadNotFound", err)
	}
}

func TestUploadStoreNotFound(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	tests := []struct {
		name string
		fn   func() error
	}{
		{"Get", func() error { _, err := s.Get(ctx, "missing"); return err }},
		{"UpsertFile", func() error {
			return s.UpsertFile(ctx, "missing", upload.StagedFile{Path: "a"}, 1)
		}},
		{"Delete", func() error { return s.Delete(ctx, "missing") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); !errors.Is(err, artifact.ErrUploadNotFound) {
				t.Errorf("err = %v, want ErrUploadNotFound", err)
			}
		})
	}
}

func TestUploadStoreUpsertFile(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	if err := s.Create(ctx, uploadAt("u1", "alice", uploadT0.Add(time.Hour)), 0); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		f     upload.StagedFile
		total int64
	}{
		{upload.StagedFile{Path: "index.html", StagingIndex: 0, Bytes: 10, ContentType: "text/html"}, 10},
		{upload.StagedFile{Path: "app.js", StagingIndex: 1, Bytes: 5, ContentType: "text/javascript"}, 15},
		{upload.StagedFile{Path: "index.html", StagingIndex: 0, Bytes: 20, ContentType: "text/html"}, 25},
	}
	for _, st := range steps {
		if err := s.UpsertFile(ctx, "u1", st.f, st.total); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	want := uploadAt("u1", "alice", uploadT0.Add(time.Hour))
	want.Bytes = 25
	want.Files = []upload.StagedFile{
		{Path: "app.js", StagingIndex: 1, Bytes: 5, ContentType: "text/javascript"},
		{Path: "index.html", StagingIndex: 0, Bytes: 20, ContentType: "text/html"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}
}

func TestUploadStoreDelete(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	if err := s.Create(ctx, uploadAt("u1", "alice", uploadT0), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFile(ctx, "u1", upload.StagedFile{Path: "a", Bytes: 1}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "u1"); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrUploadNotFound", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM upload_file`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("upload_file rows after Delete = %d, want 0", n)
	}
	if err := s.Delete(ctx, "u1"); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("second Delete: err = %v, want ErrUploadNotFound", err)
	}
}

func TestUploadStoreListExpired(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice", "bob")
	for _, u := range []*upload.Upload{
		uploadAt("late", "alice", uploadT0.Add(2*time.Hour)),
		uploadAt("early", "bob", uploadT0.Add(-time.Hour)),
		uploadAt("exact", "alice", uploadT0),
	} {
		if err := s.Create(ctx, u, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertFile(ctx, "early", upload.StagedFile{Path: "a", Bytes: 3}, 3); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		now  time.Time
		want []string
	}{
		{"before all", uploadT0.Add(-2 * time.Hour), nil},
		{"at boundary", uploadT0, []string{"early", "exact"}},
		{"after all", uploadT0.Add(3 * time.Hour), []string{"early", "exact", "late"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListExpired(ctx, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, u := range got {
				ids = append(ids, u.ID)
			}
			if diff := cmp.Diff(tt.want, ids); diff != "" {
				t.Errorf("ids (-want +got):\n%s", diff)
			}
		})
	}
	got, err := s.ListExpired(ctx, uploadT0)
	if err != nil {
		t.Fatal(err)
	}
	want := uploadAt("early", "bob", uploadT0.Add(-time.Hour))
	want.Bytes = 3
	want.Files = []upload.StagedFile{{Path: "a", Bytes: 3}}
	if diff := cmp.Diff(*want, got[0]); diff != "" {
		t.Errorf("expired upload (-want +got):\n%s", diff)
	}
}

func TestUploadStoreCountOpen(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice", "bob")
	for _, u := range []*upload.Upload{
		uploadAt("a1", "alice", uploadT0),
		uploadAt("a2", "alice", uploadT0),
		uploadAt("b1", "bob", uploadT0),
	} {
		if err := s.Create(ctx, u, 0); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		owner string
		want  int
	}{
		{"alice", 2},
		{"bob", 1},
		{"nobody", 0},
	}
	for _, tt := range tests {
		t.Run(tt.owner, func(t *testing.T) {
			got, err := s.CountOpen(ctx, tt.owner)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("CountOpen = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestUploadStoreCreateLimit(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice", "bob")
	if err := s.Create(ctx, uploadAt("b1", "bob", uploadT0), 1); err != nil {
		t.Fatal(err)
	}
	err := s.Create(ctx, uploadAt("b2", "bob", uploadT0), 1)
	if !errors.Is(err, artifact.ErrTooManyUploads) {
		t.Fatalf("Create past the limit: err = %v, want ErrTooManyUploads", err)
	}
	if _, err := s.Get(ctx, "b2"); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Fatalf("refused upload was stored: %v", err)
	}

	const limit, tries = 3, 12
	errs := make([]error, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() { errs[i] = s.Create(ctx, uploadAt(fmt.Sprintf("a%d", i), "alice", uploadT0), limit) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, artifact.ErrTooManyUploads):
		default:
			t.Errorf("concurrent Create: %v", err)
		}
	}
	if n, err := s.CountOpen(ctx, "alice"); err != nil || wins != limit || n != limit {
		t.Fatalf("%d concurrent creates succeeded, %d open (%v); want %d", wins, n, err, limit)
	}
}

func TestUploadStoreArchive(t *testing.T) {
	ctx := context.Background()
	s := uploadTestStore(t, "alice")
	want := uploadAt("u1", "alice", uploadT0.Add(time.Hour))
	want.Archive = &upload.Archive{TokenHash: "hash", SHA256: "digest", Bytes: 42}
	if err := s.Create(ctx, want, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get (-want +got):\n%s", diff)
	}

	for i := 1; i <= 2; i++ {
		if n, err := s.CountArchiveAttempt(ctx, "u1"); err != nil || n != i {
			t.Fatalf("CountArchiveAttempt = %d, %v; want %d, nil", n, err, i)
		}
	}
	if _, err := s.CountArchiveAttempt(ctx, "nope"); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("unknown upload: err = %v, want ErrUploadNotFound", err)
	}

	files := []upload.StagedFile{{Path: "a", StagingIndex: 0, Bytes: 1}, {Path: "b", StagingIndex: 1, Bytes: 2}}
	at := uploadT0.Add(time.Minute)
	if err := s.ReceiveArchive(ctx, "u1", files, 3, at); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	want.Bytes, want.Files = 3, files
	want.Archive.Attempts, want.Archive.ReceivedAt = 2, at
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get after receive (-want +got):\n%s", diff)
	}
	if err := s.ReceiveArchive(ctx, "u1", nil, 3, at); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("second receive: err = %v, want ErrUploadNotFound", err)
	}

	plain := uploadAt("u2", "alice", uploadT0.Add(time.Hour))
	if err := s.Create(ctx, plain, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReceiveArchive(ctx, "u2", nil, 0, at); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("chunked upload: err = %v, want ErrUploadNotFound", err)
	}

	expired, err := s.ListExpired(ctx, uploadT0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 2 || expired[0].Archive == nil || !expired[0].Archive.ReceivedAt.Equal(at) || expired[1].Archive != nil {
		t.Errorf("ListExpired = %+v", expired)
	}
}
