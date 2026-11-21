package backup_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

func TestBackupTelemetry(t *testing.T) {
	rec := instrumenttest.New(t)
	in := newInstance(t)
	svc := in.service(backup.Options{Telemetry: rec.Providers})
	if _, ok := rec.Sum("kazari.backup.last_success.timestamp"); ok {
		t.Error("a last success is reported before any backup")
	}

	ctx, end := rec.Start("POST /v1/backup")
	sum, err := svc.Write(ctx, io.Discard, backup.WriteOptions{})
	end()
	if err != nil {
		t.Fatal(err)
	}
	rec.Want("kazari.backup.duration", 1)
	rec.Want("kazari.backup.size", 1)
	rec.Want("kazari.backup.last_success.timestamp", float64(start.Unix()))
	span, ok := rec.Span("backup.write")
	if !ok || span.Status().Code == codes.Error {
		t.Fatalf("backup.write span = %v, found %v", span, ok)
	}
	if got := rec.SpanAttr("backup.write", "kazari.backup.file_count"); got == "" || got == "0" || sum.Files == 0 {
		t.Errorf("kazari.backup.file_count = %q for %d files", got, sum.Files)
	}
	if got := rec.SpanAttr("backup.write", "kazari.backup.certificates"); got != "false" {
		t.Errorf("kazari.backup.certificates = %q, want false", got)
	}

	// A refusal is timed and typed, and is no success.
	full := in.service(backup.Options{Telemetry: rec.Providers, Free: func(string) (uint64, error) { return 0, nil }})
	ctx, end = rec.Start("POST /system/backup")
	_, err = full.Write(ctx, io.Discard, backup.WriteOptions{})
	end()
	if !errors.Is(err, backup.ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	rec.Want("kazari.backup.duration", 1, attribute.String("error.type", "no_space"))
	rec.Want("kazari.backup.duration", 2)
	rec.Want("kazari.backup.size", 1)

	// Without a recording parent a backup starts no trace of its own.
	if _, err := svc.Write(context.Background(), io.Discard, backup.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rec.Want("kazari.backup.duration", 3)
}
