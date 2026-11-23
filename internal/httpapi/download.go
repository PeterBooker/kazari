package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/present"
)

// downloadBundle streams a version's files as a gzip-compressed tar, the
// form a publish takes, so a pulled directory can be deployed again.
func (h *userAPI) downloadBundle(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	version, err := queryInt(r, "version")
	if err != nil {
		return err
	}
	slug := r.PathValue("slug")
	v, open, err := h.d.Artifacts.OpenVersion(r.Context(), present.Actor(p), slug, version)
	if err != nil {
		return err
	}
	w.Header().Set(api.VersionHeader, strconv.Itoa(v.Number))
	out := &archiveWriter{w: w, body: middleware.Streaming(w, downloadStall),
		name: slug + "-v" + strconv.Itoa(v.Number) + ".tar.gz", contentType: "application/gzip"}
	if err := writeBundle(r.Context(), out, v, open); err != nil {
		if !out.started {
			return err
		}
		if r.Context().Err() == nil {
			h.log.ErrorContext(r.Context(), "httpapi: bundle failed mid-stream", "slug", slug, "version", v.Number, "err", err)
		}
		// A handler that returns ends the body cleanly, and the client would
		// keep a truncated archive as a whole one.
		panic(http.ErrAbortHandler)
	}
	out.start()
	return nil
}

// writeBundle writes the files in path order with the version's time and
// fixed modes, so a version always gives the same archive.
func writeBundle(ctx context.Context, w io.Writer, v *artifact.Version, open artifact.FileOpener) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	mtime := v.CreatedAt.UTC().Truncate(time.Second)
	for _, f := range v.Files {
		if err := writeBundleFile(ctx, tw, f, mtime, open); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close bundle: %w", err)
	}
	return nil
}

func writeBundleFile(ctx context.Context, tw *tar.Writer, f artifact.File, mtime time.Time, open artifact.FileOpener) error {
	rc, err := open(ctx, f)
	if err != nil {
		return err
	}
	defer rc.Close()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Size: f.Size, Mode: 0o644, ModTime: mtime}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("bundle %s: %w", f.Path, err)
	}
	if _, err := io.Copy(tw, rc); err != nil {
		return fmt.Errorf("bundle %s: %w", f.Path, err)
	}
	return nil
}
