package intake_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/intake"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/textedit"
	"github.com/PeterBooker/kazari/internal/upload"
)

func TestDecode(t *testing.T) {
	cases := []struct {
		name, content, encoding, want string
	}{
		{"text by default", "hi", "", "hi"},
		{"utf8", "hi", "utf8", "hi"},
		{"utf-8 in capitals", "hi", "UTF-8", "hi"},
		{"text", "aGk=", "text", "aGk="},
		{"base64", "aGk=", "base64", "hi"},
		{"base64 in capitals", "aGk=", "BASE64", "hi"},
		{"unpadded", "aGk", "base64", "hi"},
		{"line breaks", "aG\r\nk=\n", "base64", "hi"},
		{"spaces and tabs", " aG\tk= ", "base64", "hi"},
		{"unpadded with a line break", "aG\nk", "base64", "hi"},
		{"data URL", "data:text/plain;base64,aGk=", "base64", "hi"},
		{"empty", "", "base64", ""},
	}
	for _, c := range cases {
		r, err := intake.Decode("p", c.content, c.encoding)
		if err != nil {
			t.Errorf("%s: Decode(p, %q, %q) = %v", c.name, c.content, c.encoding, err)
			continue
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: Decode(p, %q, %q) reads %q, %v; want %q", c.name, c.content, c.encoding, got, err, c.want)
		}
	}
	for _, c := range []struct{ content, encoding string }{{"!!", "base64"}, {"aGk=", "hex"}, {"data:text/plain,hi", "base64"}} {
		if _, err := intake.Decode("p", c.content, c.encoding); !errors.Is(err, artifact.ErrInvalidArgument) {
			t.Errorf("Decode(p, %q, %q) = %v, want invalid_argument", c.content, c.encoding, err)
		}
	}
}

func TestFromWire(t *testing.T) {
	empty, body := "", "x"
	in := []api.FileInput{
		{Path: "empty.txt"},
		{Path: "a.txt", Content: "x", Encoding: "utf8", ContentType: "text/plain"},
		{Path: "b.json", URL: "https://example.com/b.json"},
		{Path: "c.json", URL: "https://example.com/c.json", Content: "x"},
		{Path: "old.txt", Delete: true},
	}
	want := []intake.File{
		{Path: "empty.txt", Content: &empty},
		{Path: "a.txt", Content: &body, Encoding: "utf8", ContentType: "text/plain"},
		{Path: "b.json", URL: "https://example.com/b.json"},
		{Path: "c.json", URL: "https://example.com/c.json", Content: &body},
		{Path: "old.txt", Content: &empty, Delete: true},
	}
	if diff := cmp.Diff(want, intake.FromWire(in)); diff != "" {
		t.Errorf("FromWire (-want +got):\n%s", diff)
	}
}

type env struct {
	ctx     context.Context
	uploads *upload.Service
	fetch   *fetch.Client
	srv     *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, role, quota_bytes, created_at) VALUES ('alice', 'member', 0, ?)`,
		time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC).UnixNano()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.Repeat("d", 8))
	}))
	t.Cleanup(srv.Close)
	return &env{
		ctx:     ctx,
		uploads: upload.New(sqlite.NewUploadStore(db), upload.Options{Dir: filepath.Join(dir, "uploads")}),
		// Loopback is allowed so the test can fetch from srv.
		fetch: fetch.New(fetch.Options{Transport: http.DefaultTransport, Dir: dir}),
		srv:   srv,
	}
}

func (e *env) intake(o intake.Options) *intake.Intake {
	return intake.New(intake.Deps{Uploads: e.uploads, Fetch: e.fetch}, o)
}

// read returns each file of files as path=content, or path=delete.
func read(t *testing.T, files []artifact.FileInput) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		if f.Delete {
			out = append(out, f.Path+"=delete")
			continue
		}
		b, err := io.ReadAll(f.Content)
		if err != nil {
			t.Fatalf("reading %s: %v", f.Path, err)
		}
		out = append(out, f.Path+"="+string(b))
	}
	return out
}

func TestFilesRefuses(t *testing.T) {
	e := newEnv(t)
	edit := []textedit.Edit{{OldText: "a", NewText: "b"}}
	cases := []struct {
		name   string
		file   intake.File
		create bool
		want   error
	}{
		{"a deletion on create", intake.File{Path: "a.txt", Delete: true}, true, artifact.ErrInvalidArgument},
		{"edits and content", intake.File{Path: "a.txt", Content: new(""), Edits: edit}, false, artifact.ErrInvalidArgument},
		{"edits and url", intake.File{Path: "a.txt", URL: e.srv.URL, Edits: edit}, false, artifact.ErrInvalidArgument},
		{"url and content", intake.File{Path: "a.txt", URL: e.srv.URL, Content: new("x")}, false, artifact.ErrInvalidArgument},
		{"no content", intake.File{Path: "a.txt"}, false, artifact.ErrInvalidArgument},
		{"an unknown encoding", intake.File{Path: "a.txt", Content: new("x"), Encoding: "hex"}, false, artifact.ErrInvalidArgument},
	}
	for _, c := range cases {
		_, _, err := e.intake(intake.Options{}).Files(e.ctx, intake.Call{Owner: "alice", Files: []intake.File{c.file}, Create: c.create})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: Files(%+v) = %v, want %v", c.name, c.file, err, c.want)
		}
	}
}

// The files named by url are capped together at the version size, whatever
// the size of an upload.
func TestFilesDownloadCap(t *testing.T) {
	e := newEnv(t)
	files := []intake.File{{Path: "a.txt", URL: e.srv.URL + "/a"}, {Path: "b.txt", URL: e.srv.URL + "/b"}}
	got, done, err := e.intake(intake.Options{MaxVersionBytes: 16}).Files(e.ctx, intake.Call{Owner: "alice", Files: files})
	if err != nil {
		t.Fatalf("Files with 16 bytes by url under a 16-byte version: %v", err)
	}
	if diff := cmp.Diff([]string{"a.txt=dddddddd", "b.txt=dddddddd"}, read(t, got)); diff != "" {
		t.Errorf("downloaded files (-want +got):\n%s", diff)
	}
	done(true)

	_, _, err = e.intake(intake.Options{MaxVersionBytes: 8}).Files(e.ctx, intake.Call{Owner: "alice", Files: files})
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Errorf("Files with 16 bytes by url under an 8-byte version = %v, want too_large", err)
	}
}

// An inline file wins a path clash over the bundle's, and both over the
// upload's; done keeps an upload a failed publish did not consume.
func TestFilesMerge(t *testing.T) {
	e := newEnv(t)
	up, err := e.uploads.Begin(e.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := e.uploads.AppendChunk(e.ctx, "alice", up.ID, p, 0, strings.NewReader("staged"), ""); err != nil {
			t.Fatal(err)
		}
	}
	call := func() intake.Call {
		return intake.Call{
			Owner:    "alice",
			UploadID: up.ID,
			Files:    []intake.File{{Path: "a.txt", Content: new("inline")}, {Path: "gone.txt", Delete: true}},
			Bundle: []artifact.FileInput{{Path: "a.txt", Content: strings.NewReader("bundle")},
				{Path: "b.txt", Content: strings.NewReader("bundle")}},
		}
	}
	in := e.intake(intake.Options{})
	got, done, err := in.Files(e.ctx, call())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt=inline", "gone.txt=delete", "b.txt=bundle", "c.txt=staged"}
	if diff := cmp.Diff(want, read(t, got)); diff != "" {
		t.Errorf("merged files (-want +got):\n%s", diff)
	}
	done(false)

	got, done, err = in.Files(e.ctx, call())
	if err != nil {
		t.Fatalf("Files after a publish that failed: %v", err)
	}
	if diff := cmp.Diff(want, read(t, got)); diff != "" {
		t.Errorf("merged files after a failed publish (-want +got):\n%s", diff)
	}
	done(true)
	if _, _, err := in.Files(e.ctx, call()); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("Files after the upload was consumed = %v, want upload_not_found", err)
	}
}
