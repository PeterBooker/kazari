package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agalma/internal/api"
	"agalma/internal/artifact"
	"agalma/internal/auth"
	"agalma/internal/present"
)

// FileInput is an inline file of create_artifact.
type FileInput struct {
	Path        string `json:"path" jsonschema:"path inside the artifact, relative, e.g. index.html or assets/app.js"`
	Content     string `json:"content,omitempty" jsonschema:"file content: UTF-8 text, or base64 when encoding is base64"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default, for text) or base64 (for binary)"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type; derived from the file extension when omitted"`
}

// UpdateFileInput is an inline file or a deletion of update_artifact.
type UpdateFileInput struct {
	Path        string `json:"path" jsonschema:"path inside the artifact, relative, e.g. index.html or assets/app.js"`
	Content     string `json:"content,omitempty" jsonschema:"file content: UTF-8 text, or base64 when encoding is base64"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default, for text) or base64 (for binary)"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type; derived from the file extension when omitted"`
	Delete      bool   `json:"delete,omitempty" jsonschema:"true removes this path from the new version; content is ignored"`
}

// CreateInput is the input of create_artifact.
type CreateInput struct {
	Slug        string            `json:"slug" jsonschema:"permanent URL name: 3-63 of a-z 0-9 and -, no leading/trailing hyphen, no --"`
	Name        string            `json:"name" jsonschema:"human-readable title"`
	Description string            `json:"description" jsonschema:"one or two sentences on what the page is"`
	Metadata    map[string]string `json:"metadata,omitempty" jsonschema:"string labels; agalma.spa, agalma.indexable and agalma.csp are interpreted by the server"`
	Visibility  string            `json:"visibility,omitempty" jsonschema:"public (default) or unlisted"`
	Label       string            `json:"label,omitempty" jsonschema:"optional note on this version"`
	Files       []FileInput       `json:"files,omitempty" jsonschema:"inline files; index.html is served at the root URL"`
	UploadID    string            `json:"upload_id,omitempty" jsonschema:"upload from begin_upload whose staged files to include"`
}

// UpdateInput is the input of update_artifact.
type UpdateInput struct {
	Slug            string             `json:"slug" jsonschema:"artifact to update"`
	Files           []UpdateFileInput  `json:"files,omitempty" jsonschema:"files to add or replace, and {path, delete: true} entries to remove; other files carry over"`
	UploadID        string             `json:"upload_id,omitempty" jsonschema:"upload from begin_upload whose staged files to include"`
	Name            *string            `json:"name,omitempty" jsonschema:"new title"`
	Description     *string            `json:"description,omitempty" jsonschema:"new description"`
	Metadata        map[string]*string `json:"metadata,omitempty" jsonschema:"keys to set; a null value deletes the key"`
	Visibility      *string            `json:"visibility,omitempty" jsonschema:"public or unlisted"`
	Label           string             `json:"label,omitempty" jsonschema:"optional note on the new version"`
	ExpectedVersion int                `json:"expected_version,omitempty" jsonschema:"latest_version you last saw; a newer one fails with version_conflict"`
	KeepStale       bool               `json:"keep_stale,omitempty" jsonschema:"keep a pending stale mark instead of clearing it"`
}

// BeginUploadInput is the (empty) input of begin_upload.
type BeginUploadInput struct{}

// ChunkInput is the input of upload_chunk.
type ChunkInput struct {
	UploadID    string `json:"upload_id" jsonschema:"id returned by begin_upload"`
	Path        string `json:"path" jsonschema:"path of the file in the artifact"`
	Offset      int64  `json:"offset" jsonschema:"0 for the first chunk of a path, then the bytes_received of the previous chunk"`
	Content     string `json:"content" jsonschema:"chunk content in the given encoding"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default, for text) or base64 (for binary)"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type of the file, read from the first chunk"`
}

// GetInput is the input of get_artifact.
type GetInput struct {
	Slug         string `json:"slug" jsonschema:"artifact to fetch"`
	IncludeFiles bool   `json:"include_files,omitempty" jsonschema:"include each version's file list"`
}

// ReadFileInput is the input of read_file.
type ReadFileInput struct {
	Slug    string `json:"slug" jsonschema:"artifact to read from"`
	Path    string `json:"path" jsonschema:"file path, e.g. index.html"`
	Version int    `json:"version,omitempty" jsonschema:"version number; default the published version"`
}

// ListInput is the input of list_artifacts.
type ListInput struct {
	Mine     string            `json:"mine,omitempty" jsonschema:"created, updated or any: only artifacts you created/updated"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"metadata pairs that must all match"`
	Stale    string            `json:"stale,omitempty" jsonschema:"include (default), exclude or only"`
	Cursor   string            `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
	Limit    int               `json:"limit,omitempty" jsonschema:"page size, default 50, at most 200"`
}

// PublishInput is the input of publish_version.
type PublishInput struct {
	Slug    string `json:"slug" jsonschema:"artifact to change"`
	Version int    `json:"version" jsonschema:"version number to serve, 1 to latest_version"`
}

// StaleInput is the input of mark_stale.
type StaleInput struct {
	Slug  string `json:"slug" jsonschema:"artifact to mark"`
	Grace string `json:"grace,omitempty" jsonschema:"grace period before expiry, e.g. 72h or 14d; default the instance setting"`
}

// SlugInput is the input of tools that take only a slug.
type SlugInput struct {
	Slug string `json:"slug" jsonschema:"artifact to change"`
}

// VisibilityInput is the input of set_visibility.
type VisibilityInput struct {
	Slug       string `json:"slug" jsonschema:"artifact to change"`
	Visibility string `json:"visibility" jsonschema:"public, unlisted or unpublished (offline)"`
}

// DeleteInput is the input of delete_artifact.
type DeleteInput struct {
	Slug string `json:"slug" jsonschema:"artifact to delete"`
	Hard bool   `json:"hard,omitempty" jsonschema:"purge now and permanently instead of a soft delete"`
}

// ReadFileResult is the structured result of read_file.
type ReadFileResult struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Hash        string `json:"hash"`
	// Encoding says where the content is: utf8 (inline text), base64
	// (embedded blob) or url (too large; download from URL).
	Encoding string `json:"encoding"`
	URL      string `json:"url,omitempty"`
}

// handler is a tool body: it returns the structured result and a one-line
// summary for the text content.
type handler[In, Out any] func(ctx context.Context, in In) (Out, string, error)

// register adds a tool whose handler runs only for principals holding
// scope, maps errors to "<code>: <message>" tool errors and returns the
// summary line followed by the JSON of the result.
func register[In, Out any](b *builder, s *mcp.Server, p auth.Principal, t *mcp.Tool, scope string, h handler[In, Out]) {
	t.Description = b.text.tools[t.Name]
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if err := requireScope(p, scope); err != nil {
			return nil, zero, b.toolError(ctx, t.Name, err)
		}
		out, summary, err := h(ctx, in)
		if err != nil {
			return nil, zero, b.toolError(ctx, t.Name, err)
		}
		js, err := json.Marshal(out)
		if err != nil {
			return nil, zero, fmt.Errorf("mcpserver: %s: encode result: %w", t.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
			&mcp.TextContent{Text: string(js)},
		}}, out, nil
	})
}

func requireScope(p auth.Principal, scope string) error {
	if !p.HasScope(scope) {
		return artifact.Errorf(artifact.ErrForbidden, "scope %q required", scope)
	}
	return nil
}

// toolError is the error a tool returns to the model: the wire error code
// and message, plus where to resume after an offset mismatch.
type toolError struct {
	e     api.Error
	cause error
}

func (e *toolError) Error() string {
	s := e.e.Code + ": " + e.e.Message
	if e.e.Current != nil {
		s += fmt.Sprintf(" (current=%d: resend this path from offset %d)", *e.e.Current, *e.e.Current)
	}
	return s
}

func (e *toolError) Unwrap() error { return e.cause }

// toolError maps err with present.Error; internal errors are logged, as
// their message is replaced by a generic one.
func (b *builder) toolError(ctx context.Context, tool string, err error) error {
	_, e := present.Error(err)
	if e.Code == api.CodeInternal {
		b.log.ErrorContext(ctx, "mcp tool failed", "tool", tool, "err", err)
	}
	e.Message = strings.TrimPrefix(e.Message, e.Code+": ")
	return &toolError{e: e, cause: err}
}

func boolPtr(v bool) *bool { return &v }

// annotations builds tool hints; every tool works on a closed world.
func annotations(title string, readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	a := &mcp.ToolAnnotations{Title: title, ReadOnlyHint: readOnly, IdempotentHint: idempotent, OpenWorldHint: boolPtr(false)}
	if !readOnly {
		a.DestructiveHint = boolPtr(destructive)
	}
	return a
}

func (b *builder) addTools(s *mcp.Server, p auth.Principal) {
	actor := present.Actor(p)
	arts := b.d.Artifacts
	links := b.d.Links

	register(b, s, p, &mcp.Tool{Name: "create_artifact", Annotations: annotations("Create artifact", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, in CreateInput) (api.PublishResult, string, error) {
			files, cleanup, err := b.files(ctx, p, in.UploadID, createFiles(in.Files))
			defer cleanup()
			if err != nil {
				return api.PublishResult{}, "", err
			}
			a, v, err := arts.Create(ctx, actor, artifact.CreateRequest{
				Slug:        in.Slug,
				Name:        in.Name,
				Description: in.Description,
				Visibility:  artifact.Visibility(in.Visibility),
				Metadata:    in.Metadata,
				Label:       in.Label,
				Files:       files,
			})
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, v.Number)
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "update_artifact", Annotations: annotations("Update artifact", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, in UpdateInput) (api.PublishResult, string, error) {
			files, cleanup, err := b.files(ctx, p, in.UploadID, in.Files)
			defer cleanup()
			if err != nil {
				return api.PublishResult{}, "", err
			}
			req := artifact.UpdateRequest{
				Slug:            in.Slug,
				Name:            in.Name,
				Description:     in.Description,
				Metadata:        in.Metadata,
				Label:           in.Label,
				ExpectedVersion: in.ExpectedVersion,
				KeepStale:       in.KeepStale,
				Files:           files,
			}
			if in.Visibility != nil {
				v := artifact.Visibility(*in.Visibility)
				req.Visibility = &v
			}
			a, v, err := arts.Update(ctx, actor, req)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			n := 0
			if v != nil {
				n = v.Number
			}
			res := links.Result(a, n)
			if v == nil {
				return res, "Updated fields, no new version. " + publishedLine(res), nil
			}
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "begin_upload", Annotations: annotations("Begin chunked upload", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, _ BeginUploadInput) (api.Upload, string, error) {
			u, err := b.d.Uploads.Begin(ctx, p.Handle)
			if err != nil {
				return api.Upload{}, "", err
			}
			out := present.Upload(u, b.o.MaxChunkBytes, b.o.MaxTotalBytes)
			return out, fmt.Sprintf("Upload %s open until %s; send chunks of at most %d bytes, starting at offset 0.",
				out.UploadID, out.ExpiresAt.Format(time.RFC3339), out.MaxChunkBytes), nil
		})

	register(b, s, p, &mcp.Tool{Name: "upload_chunk", Annotations: annotations("Upload chunk", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, in ChunkInput) (api.ChunkResult, string, error) {
			data, err := decode(in.Path, in.Content, in.Encoding)
			if err != nil {
				return api.ChunkResult{}, "", err
			}
			f, err := b.d.Uploads.AppendChunk(ctx, p.Handle, in.UploadID, in.Path, in.Offset, strings.NewReader(data), in.ContentType)
			if err != nil {
				return api.ChunkResult{}, "", err
			}
			out := present.Chunk(f)
			return out, fmt.Sprintf("Staged %s: %d bytes received; next offset %d.", out.Path, out.BytesReceived, out.BytesReceived), nil
		})

	register(b, s, p, &mcp.Tool{Name: "get_artifact", Annotations: annotations("Get artifact", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, in GetInput) (api.ArtifactDetail, string, error) {
			d, err := b.detail(ctx, actor, in.Slug, in.IncludeFiles)
			if err != nil {
				return api.ArtifactDetail{}, "", err
			}
			return d, artifactLine(d.Artifact), nil
		})

	b.addReadFile(s, p, actor)

	register(b, s, p, &mcp.Tool{Name: "list_artifacts", Annotations: annotations("List artifacts", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, in ListInput) (api.ArtifactList, string, error) {
			page, err := arts.List(ctx, actor, artifact.ListQuery{
				Mine:     in.Mine,
				Metadata: in.Metadata,
				Stale:    in.Stale,
				Cursor:   in.Cursor,
				Limit:    in.Limit,
			})
			if err != nil {
				return api.ArtifactList{}, "", err
			}
			out := links.Artifacts(page)
			line := fmt.Sprintf("%d artifacts.", len(out.Items))
			if out.NextCursor != "" {
				line += " More with cursor=" + out.NextCursor + "."
			}
			return out, line, nil
		})

	register(b, s, p, &mcp.Tool{Name: "publish_version", Annotations: annotations("Publish version", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, in PublishInput) (api.PublishResult, string, error) {
			a, err := arts.Publish(ctx, actor, in.Slug, in.Version)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, in.Version)
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "mark_stale", Annotations: annotations("Mark stale", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, in StaleInput) (api.StaleResult, string, error) {
			grace, err := parseGrace(in.Grace)
			if err != nil {
				return api.StaleResult{}, "", err
			}
			a, err := arts.MarkStale(ctx, actor, in.Slug, grace)
			if err != nil {
				return api.StaleResult{}, "", err
			}
			out := links.Stale(a)
			return out, fmt.Sprintf("Marked %s stale; it expires at %s unless updated or unmarked.",
				a.Slug, out.ExpiresAt.Format(time.RFC3339)), nil
		})

	register(b, s, p, &mcp.Tool{Name: "unmark_stale", Annotations: annotations("Unmark stale", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, in SlugInput) (api.PublishResult, string, error) {
			a, err := arts.UnmarkStale(ctx, actor, in.Slug)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, 0)
			return res, "No longer stale. " + publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "set_visibility", Annotations: annotations("Set visibility", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, in VisibilityInput) (api.PublishResult, string, error) {
			a, err := arts.SetVisibility(ctx, actor, in.Slug, in.Visibility)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, 0)
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "delete_artifact", Annotations: annotations("Delete artifact", false, true, true)},
		auth.ScopeDelete, func(ctx context.Context, in DeleteInput) (api.OK, string, error) {
			if err := arts.Delete(ctx, actor, in.Slug, in.Hard); err != nil {
				return api.OK{}, "", err
			}
			if in.Hard {
				return api.OK{OK: true}, fmt.Sprintf("Purged %s permanently.", in.Slug), nil
			}
			return api.OK{OK: true}, fmt.Sprintf("Deleted %s; it is offline and will be purged after the retention period.", in.Slug), nil
		})
}

// publishedLine is the summary of every tool that returns a PublishResult.
func publishedLine(r api.PublishResult) string {
	line := fmt.Sprintf("Published %s (version %d)", r.URL, r.Version)
	switch {
	case r.Artifact.DeletedAt != nil:
		line += "; the artifact is deleted, so the URL is offline"
	case r.Artifact.UnpublishedAt != nil:
		line = fmt.Sprintf("Unpublished %s: offline until set_visibility public or unlisted (version %d kept)", r.URL, r.Version)
	}
	return line
}

// artifactLine summarises an artifact record.
func artifactLine(a api.Artifact) string {
	line := fmt.Sprintf("%s: %s serves version %d of %d (%s)", a.Slug, a.URL, a.PublishedVersion, a.LatestVersion, a.Visibility)
	switch {
	case a.DeletedAt != nil:
		line += ", deleted"
	case a.UnpublishedAt != nil:
		line += ", unpublished (offline)"
	}
	if a.ExpiresAt != nil {
		line += ", stale until " + a.ExpiresAt.Format(time.RFC3339)
	}
	return line + "."
}

// detail returns an artifact with its versions, optionally with each
// version's files sorted by path.
func (b *builder) detail(ctx context.Context, actor artifact.Actor, slug string, withFiles bool) (api.ArtifactDetail, error) {
	a, vs, err := b.d.Artifacts.Get(ctx, actor, slug)
	if err != nil {
		return api.ArtifactDetail{}, err
	}
	if withFiles {
		for i := range vs {
			m, err := b.d.Artifacts.Files(ctx, vs[i].ID)
			if err != nil {
				return api.ArtifactDetail{}, fmt.Errorf("mcpserver: files of %s v%d: %w", slug, vs[i].Number, err)
			}
			files := slices.Collect(maps.Values(m))
			slices.SortFunc(files, func(x, y artifact.File) int { return strings.Compare(x.Path, y.Path) })
			vs[i].Files = files
		}
	}
	return b.d.Links.Detail(a, vs), nil
}

// files decodes the inline files and merges them with the staged files of
// uploadID (if any), inline winning on a path clash. The returned cleanup
// removes the upload; it is never nil and must be called even when err is
// set, so a failed call still consumes the upload.
func (b *builder) files(ctx context.Context, p auth.Principal, uploadID string, raw []UpdateFileInput) ([]artifact.FileInput, func(), error) {
	cleanup := func() {}
	var staged []artifact.FileInput
	if uploadID != "" {
		files, done, err := b.d.Uploads.Files(ctx, p.Handle, uploadID)
		if err != nil {
			return nil, cleanup, err
		}
		staged = files
		cleanup = func() {
			if err := done(); err != nil {
				b.log.ErrorContext(ctx, "mcp upload cleanup failed", "upload", uploadID, "err", err)
			}
		}
	}
	inline, err := inlineFiles(raw)
	if err != nil {
		return nil, cleanup, err
	}
	taken := make(map[string]bool, len(inline))
	for _, in := range inline {
		taken[cleanPath(in.Path)] = true
	}
	for _, f := range staged {
		if !taken[cleanPath(f.Path)] {
			inline = append(inline, f)
		}
	}
	return inline, cleanup, nil
}

// cleanPath normalises a path for clash detection; an invalid path is kept
// as is and rejected later by the service.
func cleanPath(p string) string {
	if c, err := artifact.ValidatePath(p); err == nil {
		return c
	}
	return p
}

func createFiles(in []FileInput) []UpdateFileInput {
	out := make([]UpdateFileInput, len(in))
	for i, f := range in {
		out[i] = UpdateFileInput{Path: f.Path, Content: f.Content, Encoding: f.Encoding, ContentType: f.ContentType}
	}
	return out
}

// inlineFiles decodes inline files into service inputs.
func inlineFiles(in []UpdateFileInput) ([]artifact.FileInput, error) {
	out := make([]artifact.FileInput, 0, len(in))
	for _, f := range in {
		if f.Delete {
			out = append(out, artifact.FileInput{Path: f.Path, Delete: true})
			continue
		}
		data, err := decode(f.Path, f.Content, f.Encoding)
		if err != nil {
			return nil, err
		}
		out = append(out, artifact.FileInput{Path: f.Path, Content: strings.NewReader(data), ContentType: f.ContentType})
	}
	return out, nil
}

// decode returns content as raw bytes (in a string) per encoding. Base64
// may be padded or not and may contain line breaks.
func decode(path, content, encoding string) (string, error) {
	switch strings.ToLower(encoding) {
	case "", "utf8", "utf-8", "text":
		return content, nil
	case "base64":
		s := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, content)
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
		}
		if err != nil {
			return "", artifact.Errorf(artifact.ErrInvalidArgument, "content of %q is not valid base64", path)
		}
		return string(data), nil
	}
	return "", artifact.Errorf(artifact.ErrInvalidArgument, "encoding of %q must be utf8 or base64, not %q", path, encoding)
}

// parseGrace accepts a Go duration or a whole number of days ("14d").
// Empty means the instance default.
func parseGrace(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, artifact.Errorf(artifact.ErrInvalidArgument, "grace %q must be a positive duration such as 72h or 14d", s)
	}
	return d, nil
}

// readAtMost reads up to limit bytes and reports whether r had more.
func readAtMost(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}
