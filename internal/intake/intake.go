// Package intake turns the files a create or update names, inline, by url or
// staged in an upload, into the artifact service's inputs, the same way for
// the REST API and the MCP server.
package intake

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/textedit"
	"github.com/PeterBooker/kazari/internal/upload"
)

// Deps are the services an Intake reads files from.
type Deps struct {
	// Uploads holds the files staged under an upload id. Required.
	Uploads *upload.Service
	// Fetch downloads files named by url; nil refuses them.
	Fetch  *fetch.Client
	Logger *slog.Logger
}

// Options are the limits of one call: MaxFileBytes caps one file named by
// url, MaxVersionBytes the files one call names by url, together.
type Options struct {
	artifact.Limits
}

// Intake converts the files of a call.
type Intake struct {
	d   Deps
	o   Options
	log *slog.Logger
}

// New returns an Intake.
func New(d Deps, o Options) *Intake {
	o.Limits = o.Limits.WithDefaults()
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Intake{d: d, o: o, log: log}
}

// File is one entry of a call's files: content, a url, edits or a deletion.
type File struct {
	Path string
	// Content is nil when the entry gives none, which is not an empty file.
	Content     *string
	Encoding    string
	ContentType string
	URL         string
	Edits       []textedit.Edit
	Delete      bool
}

// FromWire converts the files of a REST body, where an entry without url
// or content is an empty file.
func FromWire(in []api.FileInput) []File {
	out := make([]File, len(in))
	for i, f := range in {
		out[i] = File{Path: f.Path, Encoding: f.Encoding, ContentType: f.ContentType, URL: f.URL, Delete: f.Delete}
		if f.URL == "" || f.Content != "" {
			out[i].Content = &f.Content
		}
	}
	return out
}

// Call is what one create or update names.
type Call struct {
	// Owner is the handle the upload belongs to.
	Owner    string
	UploadID string
	Files    []File
	// Bundle holds the files of an archive sent with the call, which the
	// inline files replace on a path clash.
	Bundle []artifact.FileInput
	// Create refuses deletions.
	Create bool
}

// Files returns the files of c: the inline ones, those named by url once
// downloaded, the bundle's and the upload's, a file earlier in that order
// winning a path clash. Call done once the publish has succeeded or failed:
// it removes the downloads, and the upload only when consumed, so a refused
// call can be corrected and sent again with the same upload id.
func (in *Intake) Files(ctx context.Context, c Call) ([]artifact.FileInput, func(consumed bool), error) {
	release := func(bool) error { return nil }
	var staged []artifact.FileInput
	if c.UploadID != "" {
		files, r, err := in.d.Uploads.Files(ctx, c.Owner, c.UploadID)
		if err != nil {
			return nil, nil, err
		}
		staged, release = files, r
	}
	inline, closeDownloads, err := in.inline(ctx, c.Files, c.Create)
	if err != nil {
		in.release(ctx, c.UploadID, release, false)
		return nil, nil, err
	}
	done := func(consumed bool) {
		closeDownloads()
		in.release(ctx, c.UploadID, release, consumed)
	}
	return artifact.MergeFiles(artifact.MergeFiles(inline, c.Bundle), staged), done, nil
}

func (in *Intake) release(ctx context.Context, id string, release func(bool) error, consumed bool) {
	if err := release(consumed); err != nil {
		in.log.ErrorContext(ctx, "intake: upload cleanup failed", "upload", id, "err", err)
	}
}

// inline converts files, downloading those named by url; the func it
// returns removes the downloads.
func (in *Intake) inline(ctx context.Context, files []File, create bool) ([]artifact.FileInput, func(), error) {
	dl := in.d.Fetch.Downloads(ctx, in.o.MaxFileBytes, in.o.MaxVersionBytes)
	out := make([]artifact.FileInput, 0, len(files))
	for _, f := range files {
		var (
			fi  artifact.FileInput
			err error
		)
		switch {
		case f.Delete && create:
			err = artifact.Errorf(artifact.ErrInvalidArgument, "cannot delete %q when creating an artifact", f.Path)
		case f.Delete:
			fi = artifact.FileInput{Path: f.Path, Delete: true}
		case len(f.Edits) > 0 && (f.URL != "" || f.Content != nil):
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has edits and also content or url; give one", f.Path)
		case len(f.Edits) > 0:
			fi = artifact.FileInput{Path: f.Path, Edits: f.Edits, ContentType: f.ContentType}
		case f.URL != "" && f.Content != nil:
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has both url and content", f.Path)
		case f.URL != "":
			fi, err = dl.Add(f.Path, f.URL, f.ContentType)
		case f.Content == nil:
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has no content: give content (\"\" for an empty "+
				"file), url, edits or delete: true", f.Path)
		default:
			var r io.Reader
			if r, err = Decode(f.Path, *f.Content, f.Encoding); err == nil {
				fi = artifact.FileInput{Path: f.Path, Content: r, ContentType: f.ContentType}
			}
		}
		if err != nil {
			dl.Close()
			return nil, nil, err
		}
		out = append(out, fi)
	}
	return out, dl.Close, nil
}

// Decode reads content in encoding: utf8 (also "", "utf-8" and "text") or
// base64, in any case. Base64 may be unpadded, hold whitespace and start
// with a data: URL's "data:<type>;base64,".
func Decode(path, content, encoding string) (io.Reader, error) {
	switch strings.ToLower(encoding) {
	case "", "utf8", "utf-8", "text":
		return strings.NewReader(content), nil
	case "base64":
		data, err := decodeBase64(content)
		if err != nil {
			return nil, artifact.Errorf(artifact.ErrInvalidArgument, "content of %q is not valid base64", path)
		}
		return bytes.NewReader(data), nil
	}
	return nil, artifact.Errorf(artifact.ErrInvalidArgument, "encoding of %q must be utf8 or base64, not %q", path, encoding)
}

func decodeBase64(s string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(s, "data:"); ok {
		if _, data, ok := strings.Cut(rest, ";base64,"); ok {
			s = data
		}
	}
	// The decoder skips line breaks itself, but not spaces or tabs.
	if strings.ContainsAny(s, " \t") {
		s = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, s)
	}
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return data, err
}

// EncodedLen is the size of n bytes in padded base64, for a body limit
// that must carry them so.
func EncodedLen(n int64) int64 { return (n + 2) / 3 * 4 }
