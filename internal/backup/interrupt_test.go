package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/backup"
)

// limitedWriter takes n bytes and fails from then on, as a client that goes
// away part way through a download does.
type limitedWriter struct {
	n   int
	err error
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		w.n = 0
		return 0, w.err
	}
	w.n -= len(p)
	return len(p), nil
}

// leftovers lists what a backup left in the instance's temporary directory,
// where it keeps its copy of the database while it writes.
func leftovers(t *testing.T, in *instance) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(in.dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBackupWriterFails(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	svc := in.service(backup.Options{})
	var whole bytes.Buffer
	if _, err := svc.Write(t.Context(), &whole, backup.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// The archive ends with the one blob, a block for its header and one
	// for its content, and two blocks that close it.
	const block = 512
	beforeBlob := whole.Len() - 4*block

	tests := []struct {
		name  string
		limit int
		files int
	}{
		{name: "at the first write", limit: 0, files: 0},
		{name: "after the database", limit: beforeBlob, files: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errGone := errors.New("the client went away")
			sum, err := svc.Write(t.Context(), &limitedWriter{n: tt.limit, err: errGone}, backup.WriteOptions{})
			if !errors.Is(err, errGone) || sum.Files != tt.files {
				t.Errorf("Write to a writer that fails after %d bytes = %+v, %v, want %d files and the writer's error", tt.limit, sum, err, tt.files)
			}
			if got := leftovers(t, in); len(got) != 0 {
				t.Errorf("the failed backup left %v in the temporary directory", got)
			}
			if _, err := svc.Write(t.Context(), io.Discard, backup.WriteOptions{}); err != nil {
				t.Errorf("Write after a failed backup: %v", err)
			}
		})
	}
}

func TestBackupCanceledWhileWriting(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	svc := in.service(backup.Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := svc.Write(ctx, &hookWriter{w: io.Discard, hook: cancel}, backup.WriteOptions{})
	if !errors.Is(err, context.Canceled) || sum.Files != 0 {
		t.Errorf("Write under a context that ends at the first write = %+v, %v, want no files and context.Canceled", sum, err)
	}
	if got := leftovers(t, in); len(got) != 0 {
		t.Errorf("the canceled backup left %v in the temporary directory", got)
	}
	release, ok := svc.HoldGC()
	if !ok {
		t.Fatal("HoldGC after a canceled backup refused: the backup still holds the collector off")
	}
	release()
}
