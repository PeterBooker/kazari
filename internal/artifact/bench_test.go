package artifact_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// BenchmarkCreate2000Files publishes 2,000 new 1 KiB files at once, the
// default file limit.
func BenchmarkCreate2000Files(b *testing.B) {
	e := newEnv(b, artifact.Options{MaxFiles: 2000})
	alice := e.user(b, "alice")
	pad := strings.Repeat("x", 1000)
	n := 0
	for b.Loop() {
		b.StopTimer()
		n++
		in := make([]artifact.FileInput, 2000)
		for i := range in {
			in[i] = artifact.FileInput{Path: fmt.Sprintf("f/%04d.txt", i),
				Content: strings.NewReader(fmt.Sprintf("%d %d %s", n, i, pad))}
		}
		b.StartTimer()
		if _, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: fmt.Sprintf("bench-%d", n), Files: in}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCollectGarbageBatch collects one full batch of 1,000
// unreferenced blobs.
func BenchmarkCollectGarbageBatch(b *testing.B) {
	e := newEnv(b, artifact.Options{BlobGrace: time.Hour})
	n := 0
	for b.Loop() {
		b.StopTimer()
		for i := range 1000 {
			n++
			hash, size, err := e.blobs.Put(e.ctx, strings.NewReader(fmt.Sprintf("garbage %d %d", n, i)))
			if err != nil {
				b.Fatal(err)
			}
			if err := e.store.RecordBlob(e.ctx, hash, size, e.clock.Now()); err != nil {
				b.Fatal(err)
			}
		}
		e.clock.Advance(2 * time.Hour)
		b.StartTimer()
		got, err := e.svc.CollectGarbage(e.ctx)
		if err != nil || got != 1000 {
			b.Fatalf("CollectGarbage = %d, %v; want 1000", got, err)
		}
	}
}
