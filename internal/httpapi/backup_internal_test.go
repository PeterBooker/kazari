package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/middleware"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

// A backup of any size is written under a deadline that every write renews,
// never with the deadline lifted: a reader that stalls ends the download
// and frees the backup for the next caller.
func TestArchiveWriterRenewsTheWriteDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	out := &archiveWriter{w: rec, body: middleware.Streaming(rec, backupStall), name: "kazari.tar"}
	began := time.Now()
	for range 3 {
		if _, err := out.Write([]byte("part of the archive")); err != nil {
			t.Fatal(err)
		}
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-tar" || rec.Body.Len() != 3*len("part of the archive") {
		t.Errorf("status %d, Content-Type %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if len(rec.deadlines) != 3 {
		t.Fatalf("deadlines set = %d, want one per write", len(rec.deadlines))
	}
	for _, d := range rec.deadlines {
		if d.Before(began.Add(backupStall)) || d.After(time.Now().Add(backupStall)) {
			t.Errorf("deadline %v is not %v after its write", d, backupStall)
		}
	}
}
