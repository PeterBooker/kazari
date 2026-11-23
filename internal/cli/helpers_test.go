package cli_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"testing"
	"time"
)

// The exit codes a script tests, as docs/cli.md lists them.
const (
	exitError        = 1
	exitUsage        = 2
	exitNotFound     = 3
	exitConflict     = 4
	exitUnauthorized = 6
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type bundleRequest struct {
	method, path string
	header       http.Header
	manifest     map[string]any
	bundleType   string
	files        map[string]string
	// raw is the bundle part as sent.
	raw []byte
}

func decodeBundleRequest(t *testing.T, r *http.Request) bundleRequest {
	t.Helper()
	out := bundleRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), files: map[string]string{}}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		return out
	}
	mr, err := r.MultipartReader()
	if err != nil {
		t.Errorf("multipart: %v", err)
		return out
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("next part: %v", err)
			return out
		}
		switch p.FormName() {
		case "manifest":
			if err := json.NewDecoder(p).Decode(&out.manifest); err != nil {
				t.Errorf("manifest: %v", err)
			}
		case "bundle":
			out.bundleType = p.Header.Get("Content-Type")
			if out.raw, err = io.ReadAll(p); err != nil {
				t.Errorf("bundle: %v", err)
			}
			for _, e := range listTarGz(t, out.raw) {
				out.files[e.Name] = e.Content
			}
		default:
			t.Errorf("unexpected part %q", p.FormName())
		}
	}
	return out
}

type tarEntry struct {
	Name    string
	Content string
	Mode    int64
	ModTime time.Time
	Uid     int
	Type    byte
}

func listTarGz(t *testing.T, b []byte) []tarEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Errorf("gzip: %v", err)
		return nil
	}
	tr := tar.NewReader(zr)
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Errorf("tar: %v", err)
			return out
		}
		c, _ := io.ReadAll(tr)
		out = append(out, tarEntry{h.Name, string(c), h.Mode, h.ModTime.UTC(), h.Uid, h.Typeflag})
	}
}
