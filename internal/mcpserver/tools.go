package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	pathpkg "path"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
)

// FileInput is an inline file of create_artifact. Content is a pointer so an
// entry that leaves it out is told apart from an empty file.
type FileInput struct {
	Path        string  `json:"path" jsonschema:"path inside the artifact, e.g. index.html or assets/app.js"`
	Content     *string `json:"content,omitempty" jsonschema:"UTF-8 text, or base64 with encoding base64; \"\" for an empty file"`
	Encoding    string  `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	ContentType string  `json:"content_type,omitempty" jsonschema:"MIME type; default from the extension"`
	URL         string  `json:"url,omitempty" jsonschema:"address the server downloads as the content"`
}

// UpdateFileInput is an inline file, an edit or a deletion of update_artifact.
type UpdateFileInput struct {
	Path        string      `json:"path" jsonschema:"path inside the artifact, e.g. index.html or assets/app.js"`
	Content     *string     `json:"content,omitempty" jsonschema:"UTF-8 text, or base64 with encoding base64; \"\" for an empty file"`
	Encoding    string      `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	ContentType string      `json:"content_type,omitempty" jsonschema:"MIME type; default from the extension"`
	URL         string      `json:"url,omitempty" jsonschema:"address the server downloads as the content"`
	Edits       []EditInput `json:"edits,omitempty" jsonschema:"edits to this text file, as in edit_file, instead of content"`
	Delete      bool        `json:"delete,omitempty" jsonschema:"true removes this path from the new version"`
}

// CreateInput is the input of create_artifact.
type CreateInput struct {
	Slug        string            `json:"slug" jsonschema:"permanent URL name: 3-63 of a-z 0-9 and -, no leading/trailing hyphen, no --"`
	Name        string            `json:"name" jsonschema:"human-readable title"`
	Description string            `json:"description" jsonschema:"one or two sentences on what the page is"`
	Metadata    map[string]string `json:"metadata,omitempty" jsonschema:"string labels; kazari.spa, kazari.indexable, kazari.csp and kazari.keep_versions are interpreted by the server"`
	Visibility  string            `json:"visibility,omitempty" jsonschema:"public, unlisted or members (only people signed in to this instance, or with a share link); omitted means the instance default"`
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
	Visibility      *string            `json:"visibility,omitempty" jsonschema:"public, unlisted, members or unpublished (offline)"`
	Label           string             `json:"label,omitempty" jsonschema:"optional note on the new version"`
	ExpectedVersion int                `json:"expected_version,omitempty" jsonschema:"latest_version you last saw; a newer one fails with version_conflict"`
	BaseVersion     int                `json:"base_version,omitempty" jsonschema:"version the new one builds on; default the latest, required while an earlier version is published"`
	KeepStale       bool               `json:"keep_stale,omitempty" jsonschema:"keep a pending stale mark instead of clearing it"`
	Restore         bool               `json:"restore,omitempty" jsonschema:"bring back a soft-deleted artifact (get_artifact shows deleted_at) before applying the other changes"`
	Stage           bool               `json:"stage,omitempty" jsonschema:"add the version without publishing it; publish_version makes it live"`
}

// EditInput is one edit of edit_file: a text edit or an anchor edit.
type EditInput struct {
	OldText    string `json:"old_text,omitempty" jsonschema:"text edit: exact text to replace, copied from the file"`
	NewText    string `json:"new_text,omitempty" jsonschema:"replacement; omitted or empty deletes"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every match of old_text"`
	Start      string `json:"start,omitempty" jsonschema:"anchor edit: first line to replace, e.g. 12:3fa0"`
	End        string `json:"end,omitempty" jsonschema:"anchor edit: last line to replace; default start"`
	After      string `json:"after,omitempty" jsonschema:"insert new_text after this anchored line"`
	Before     string `json:"before,omitempty" jsonschema:"insert new_text before this anchored line"`
}

// EditFileInput is the input of edit_file.
type EditFileInput struct {
	Slug            string      `json:"slug" jsonschema:"artifact to change"`
	Path            string      `json:"path" jsonschema:"text file to edit, e.g. index.html"`
	Edits           []EditInput `json:"edits" jsonschema:"text edits or anchor edits, not both"`
	ExpectedVersion int         `json:"expected_version,omitempty" jsonschema:"latest_version you last saw; a newer one fails with version_conflict"`
	BaseVersion     int         `json:"base_version,omitempty" jsonschema:"version whose file to edit; default the latest, required while an earlier version is published"`
	Label           string      `json:"label,omitempty" jsonschema:"optional note on the new version"`
	KeepStale       bool        `json:"keep_stale,omitempty" jsonschema:"keep a pending stale mark instead of clearing it"`
	Stage           bool        `json:"stage,omitempty" jsonschema:"add the version without publishing it; publish_version makes it live"`
}

// editFileSchema constrains each edit to exactly one of a non-empty old_text, start, after and before.
func editFileSchema() *jsonschema.Schema {
	s, err := jsonschema.For[EditFileInput](nil)
	if err != nil {
		panic(err) // a fixed struct always infers
	}
	one := 1
	kind := func(field string) *jsonschema.Schema {
		return &jsonschema.Schema{
			Required:   []string{field},
			Properties: map[string]*jsonschema.Schema{field: {Type: "string", MinLength: &one}},
		}
	}
	s.Properties["edits"].Items.OneOf = []*jsonschema.Schema{kind("old_text"), kind("start"), kind("after"), kind("before")}
	return s
}

func updateSchema() *jsonschema.Schema {
	s, err := jsonschema.For[UpdateInput](nil)
	if err != nil {
		panic(err) // a fixed struct always infers
	}
	for _, p := range s.Properties["files"].Items.Properties["edits"].Items.Properties {
		p.Description = ""
	}
	return s
}

// BeginUploadInput is the input of begin_upload. Without the archive fields
// the upload takes chunks; with both it also takes one archive at archive_url.
type BeginUploadInput struct {
	ArchiveSHA256 string `json:"archive_sha256,omitempty" jsonschema:"hex SHA-256 of the zip or tar.gz you will PUT to archive_url; with archive_bytes, asks for that URL"`
	ArchiveBytes  int64  `json:"archive_bytes,omitempty" jsonschema:"size of that archive in bytes"`
}

// ChunkInput is the input of upload_chunk.
type ChunkInput struct {
	UploadID    string `json:"upload_id" jsonschema:"id returned by begin_upload"`
	Path        string `json:"path" jsonschema:"path of the file in the artifact"`
	Offset      int64  `json:"offset" jsonschema:"0 for the first chunk of a path, then the bytes_received of the previous chunk"`
	Content     string `json:"content" jsonschema:"chunk content in the given encoding"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default) or base64"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type of the file, read from the first chunk"`
}

// DiscardUploadInput is the input of discard_upload.
type DiscardUploadInput struct {
	UploadID string `json:"upload_id" jsonschema:"id returned by begin_upload"`
}

// GetInput is the input of get_artifact.
type GetInput struct {
	Slug         string `json:"slug" jsonschema:"artifact to fetch"`
	IncludeFiles bool   `json:"include_files,omitempty" jsonschema:"include file lists, newest version first, one page at a time"`
	FilesBefore  int    `json:"files_before,omitempty" jsonschema:"with include_files, the next_files_before of the previous page"`
}

// ReadFileInput is the input of read_file.
type ReadFileInput struct {
	Slug    string `json:"slug" jsonschema:"artifact to read from"`
	Path    string `json:"path" jsonschema:"file path, e.g. index.html"`
	Version int    `json:"version,omitempty" jsonschema:"version number; default the published version"`
	Anchors bool   `json:"anchors,omitempty" jsonschema:"prefix each line of a text file with its anchor (12:3fa0|) for edit_file"`
	// StartLine and EndLine select a window of a text file of any size.
	StartLine int `json:"start_line,omitempty" jsonschema:"first line of a window of a text file (1-based); the file may be larger than the inline limit"`
	EndLine   int `json:"end_line,omitempty" jsonschema:"last line of the window, inclusive; default the end of the file"`
}

// ListInput is the input of list_artifacts.
type ListInput struct {
	Mine     string            `json:"mine,omitempty" jsonschema:"created, updated or any: only artifacts you created/updated"`
	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"metadata pairs that must all match"`
	Stale    string            `json:"stale,omitempty" jsonschema:"include (default), exclude or only"`
	Deleted  string            `json:"deleted,omitempty" jsonschema:"exclude (default), include or only: soft-deleted artifacts you may still see"`
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
	Visibility string `json:"visibility" jsonschema:"public, unlisted, members or unpublished (offline)"`
}

// ShareLinkInput is the input of create_share_link.
type ShareLinkInput struct {
	Slug      string `json:"slug" jsonschema:"artifact to share"`
	Label     string `json:"label,omitempty" jsonschema:"note on who the link is for, e.g. Acme review"`
	ExpiresIn string `json:"expires_in,omitempty" jsonschema:"how long the link works, e.g. 72h, 14d or 2w; default 7d"`
	Version   int    `json:"version,omitempty" jsonschema:"also return version_url, the link opening this version"`
	Password  string `json:"password,omitempty" jsonschema:"a password the page asks for before it opens"`
}

// ListShareLinksInput is the input of list_share_links.
type ListShareLinksInput struct {
	Slug string `json:"slug" jsonschema:"artifact whose share links to list"`
}

// RevokeShareLinkInput is the input of revoke_share_link.
type RevokeShareLinkInput struct {
	Slug   string `json:"slug" jsonschema:"artifact the link opens"`
	Prefix string `json:"prefix" jsonschema:"prefix of the link, from list_share_links or create_share_link"`
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
	// Encoding is utf8 (inline text), base64 (embedded blob) or url (download from URL).
	Encoding string `json:"encoding"`
	// Content repeats the text content for clients that show only the structured result.
	Content  string `json:"content,omitempty"`
	URL      string `json:"url,omitempty"`
	Anchored bool   `json:"anchored,omitempty"`
	// Version is the version read; LatestVersion the one edit_file edits.
	Version       int `json:"version"`
	LatestVersion int `json:"latest_version"`
	// StartLine, EndLine and TotalLines describe a window read with start_line
	// and end_line; EndLine is the last line returned.
	StartLine  int `json:"start_line,omitempty"`
	EndLine    int `json:"end_line,omitempty"`
	TotalLines int `json:"total_lines,omitempty"`
}

// handler returns a tool's structured result and a one-line summary for the text content.
type handler[In, Out any] func(ctx context.Context, req *mcp.CallToolRequest, in In) (Out, string, error)

// inputRequired asks for the user's answers first: register turns it into an
// input_required result (SEP-2322) and the client calls again with them.
type inputRequired struct{ requests mcp.InputRequestMap }

func (*inputRequired) Error() string { return "mcpserver: input required" }

// register adds a tool when p holds scope: a tool the credential cannot use
// would only cost the model tokens and a failed call. Its text content is
// the summary line followed by the result's JSON.
func register[In, Out any](b *builder, s *mcp.Server, p auth.Principal, t *mcp.Tool, scope string, h handler[In, Out]) {
	if !p.HasScope(scope) {
		return
	}
	t.Description = b.text.tools[t.Name]
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		var zero Out
		defer b.recoverTool(ctx, t.Name, func(e error) { res, out, err = nil, zero, e })
		if err := requireScope(p, scope); err != nil {
			return nil, zero, b.toolError(ctx, t.Name, err)
		}
		if scope != auth.ScopeRead {
			if d := b.d.Writes.Reserve(ctx, ratelimit.CredentialKey(p)); d > 0 {
				return nil, zero, b.toolError(ctx, t.Name, &api.Error{Code: api.CodeRateLimited,
					Message: "too many writes; retry in " + ratelimit.RetryAfter(d) + "s"})
			}
		}
		out, summary, err := h(ctx, req, in)
		if ir, ok := errors.AsType[*inputRequired](err); ok {
			return &mcp.CallToolResult{InputRequests: ir.requests}, zero, nil
		}
		if err != nil {
			return nil, zero, b.toolError(ctx, t.Name, b.notFound(ctx, present.Actor(p), req, err))
		}
		if w, ok := any(out).(warned); ok {
			for _, line := range w.warningLines() {
				summary += "\nWarning: " + line
			}
		}
		// Unescaped, "<" costs one byte of the model's context instead of six.
		var js strings.Builder
		enc := json.NewEncoder(&js)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			return nil, zero, fmt.Errorf("mcpserver: %s: encode result: %w", t.Name, err)
		}
		res = &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
			&mcp.TextContent{Text: strings.TrimSuffix(js.String(), "\n")},
		}}
		if m, ok := any(out).(resultMeta); ok {
			res.Meta = m.meta()
		}
		return res, out, nil
	})
}

// recoverTool, deferred, hands a panic to fail as an internal error: the SDK runs
// handlers outside net/http's and middleware.Recover's reach, so it would end the process.
func (b *builder) recoverTool(ctx context.Context, name string, fail func(error)) {
	v := recover()
	if v == nil {
		return
	}
	b.log.ErrorContext(ctx, "mcp handler panicked", "tool", name, "panic", v, "stack", string(debug.Stack()))
	fail(&toolError{e: api.Error{Code: api.CodeInternal, Message: "internal error"}, next: b.text.next[api.CodeInternal],
		cause: fmt.Errorf("panic: %v", v)})
}

func requireScope(p auth.Principal, scope string) error {
	if !p.HasScope(scope) {
		return artifact.Errorf(artifact.ErrForbidden, "scope %q required", scope)
	}
	return nil
}

// toolError is the error a tool returns to the model.
type toolError struct {
	e     api.Error
	next  string
	cause error
}

func (e *toolError) Error() string {
	s := e.e.Code + ": " + e.e.Message
	if e.e.Current != nil {
		s += fmt.Sprintf(" (current=%d: resend this path from offset %d)", *e.e.Current, *e.e.Current)
	}
	if e.next != "" {
		s += "\nNext step: " + e.next
	}
	return s
}

func (e *toolError) Unwrap() error { return e.cause }

// toolError adds the next step for err's code and logs internal errors, since
// present.Error hides their message.
func (b *builder) toolError(ctx context.Context, tool string, err error) error {
	var e api.Error
	if wire, ok := errors.AsType[*api.Error](err); ok {
		e = *wire
	} else {
		_, e = present.Error(err)
	}
	if e.Code == api.CodeInternal {
		b.log.ErrorContext(ctx, "mcp tool failed", "tool", tool, "err", err)
	}
	e.Message = strings.TrimPrefix(e.Message, e.Code+": ")
	return &toolError{e: e, next: b.text.next[e.Code], cause: err}
}

// annotations builds tool hints; every tool works on a closed world.
func annotations(title string, readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	a := &mcp.ToolAnnotations{Title: title, ReadOnlyHint: readOnly, IdempotentHint: idempotent, OpenWorldHint: new(false)}
	if !readOnly {
		a.DestructiveHint = new(destructive)
	}
	return a
}

func (b *builder) addTools(s *mcp.Server, p auth.Principal) {
	actor := present.Actor(p)
	arts := b.d.Artifacts
	links := b.d.Links

	register(b, s, p, &mcp.Tool{Name: "create_artifact", Annotations: annotations("Create artifact", false, false, false),
		Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateInput) (WriteResult, string, error) {
			if err := present.RequireShare(p, in.Visibility); err != nil {
				return WriteResult{}, "", err
			}
			files, done, err := b.files(ctx, p, in.UploadID, createFiles(in.Files))
			if err != nil {
				return WriteResult{}, "", err
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
			done(err == nil)
			if err != nil {
				return WriteResult{}, "", err
			}
			res := writeResult(links.Published(a, v))
			return res, publishedLine(res), nil
		})

	if b.o.Fetch {
		b.addImport(s, p, actor)
	}

	register(b, s, p, &mcp.Tool{Name: "update_artifact", InputSchema: b.updateSchema, Annotations: annotations("Update artifact", false, false, false),
		Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in UpdateInput) (WriteResult, string, error) {
			// Models fill optional fields with "": that leaves the
			// visibility alone rather than changing it.
			vis := ""
			if in.Visibility != nil {
				vis = *in.Visibility
			}
			if err := present.RequireShare(p, vis); err != nil {
				return WriteResult{}, "", err
			}
			if vis != "" {
				if err := checkVisibility(vis); err != nil {
					return WriteResult{}, "", err
				}
			}
			files, done, err := b.files(ctx, p, in.UploadID, in.Files)
			if err != nil {
				return WriteResult{}, "", err
			}
			a, v, err := b.update(ctx, actor, artifact.UpdateRequest{
				Slug:            in.Slug,
				Name:            in.Name,
				Description:     in.Description,
				Metadata:        in.Metadata,
				Label:           in.Label,
				ExpectedVersion: in.ExpectedVersion,
				BaseVersion:     in.BaseVersion,
				KeepStale:       in.KeepStale,
				Restore:         in.Restore,
				Stage:           in.Stage,
				Files:           files,
			}, vis)
			done(err == nil)
			if err != nil {
				return WriteResult{}, "", err
			}
			if v == nil {
				res := writeResult(links.Result(a, 0))
				line := "Updated fields, no new version. " + publishedLine(res)
				if vis != "" {
					line = visibilityLine(res)
				}
				return res, line, nil
			}
			res := writeResult(links.Published(a, v))
			res.Edited = editedFiles(v.Edited)
			line := publishedLine(res)
			if len(res.Edited) > 0 {
				parts := make([]string, len(res.Edited))
				for i, f := range res.Edited {
					parts[i] = f.Path + " (" + editParts(f.Edits) + ")"
				}
				line += "; edited " + strings.Join(parts, ", ")
			}
			return res, line, nil
		})

	register(b, s, p, &mcp.Tool{Name: "edit_file", InputSchema: b.editSchema,
		Annotations: annotations("Edit file", false, false, false), Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in EditFileInput) (EditFileResult, string, error) {
			a, v, outs, err := arts.Edit(ctx, actor, artifact.EditRequest{
				Slug:            in.Slug,
				Path:            in.Path,
				Edits:           edits(in.Edits),
				Label:           in.Label,
				ExpectedVersion: in.ExpectedVersion,
				BaseVersion:     in.BaseVersion,
				KeepStale:       in.KeepStale,
				Stage:           in.Stage,
			})
			if err != nil {
				return EditFileResult{}, "", err
			}
			clean, _ := artifact.ValidatePath(in.Path) // valid: Edit accepted it
			res := editFileResult(links.Edit(a, v, clean, outs))
			return res, editLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "begin_upload", Annotations: annotations("Begin upload", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in BeginUploadInput) (api.Upload, string, error) {
			if in.ArchiveSHA256 != "" || in.ArchiveBytes != 0 {
				u, token, err := b.d.Uploads.BeginArchive(ctx, p.Handle, in.ArchiveSHA256, in.ArchiveBytes)
				if err != nil {
					return api.Upload{}, "", err
				}
				out := present.Upload(u, b.o.MaxChunkBytes, b.o.MaxTotalBytes)
				out.ArchiveURL = present.ArchiveURL(b.o.APIBaseURL, u.ID, token)
				return out, fmt.Sprintf("Upload %s open until %s. Send the archive with: curl --fail-with-body -T <archive> '%s' "+
					"(no credential needed), then call create_artifact or update_artifact with upload_id.",
					out.UploadID, out.ExpiresAt.Format(time.RFC3339), out.ArchiveURL), nil
			}
			u, err := b.d.Uploads.Begin(ctx, p.Handle)
			if err != nil {
				return api.Upload{}, "", err
			}
			out := present.Upload(u, b.o.MaxChunkBytes, b.o.MaxTotalBytes)
			return out, fmt.Sprintf("Upload %s open until %s; send chunks of at most %d bytes, starting at offset 0.",
				out.UploadID, out.ExpiresAt.Format(time.RFC3339), out.MaxChunkBytes), nil
		})

	register(b, s, p, &mcp.Tool{Name: "upload_chunk", Annotations: annotations("Upload chunk", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in ChunkInput) (api.ChunkResult, string, error) {
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

	register(b, s, p, &mcp.Tool{Name: "discard_upload", Annotations: annotations("Discard upload", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in DiscardUploadInput) (api.OK, string, error) {
			if err := b.d.Uploads.Abort(ctx, p.Handle, in.UploadID); err != nil {
				return api.OK{}, "", err
			}
			return api.OK{OK: true}, fmt.Sprintf("Discarded upload %s; its staged files are removed.", in.UploadID), nil
		})

	register(b, s, p, &mcp.Tool{Name: "get_artifact", Annotations: annotations("Get artifact", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, _ *mcp.CallToolRequest, in GetInput) (api.ArtifactDetail, string, error) {
			d, err := b.detail(ctx, actor, in.Slug, in.IncludeFiles, in.FilesBefore)
			if err != nil {
				return api.ArtifactDetail{}, "", err
			}
			return d, artifactLine(d.Artifact), nil
		})

	b.addReadFile(s, p, actor)

	register(b, s, p, &mcp.Tool{Name: "list_artifacts", Annotations: annotations("List artifacts", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (api.ArtifactList, string, error) {
			page, err := arts.List(ctx, actor, artifact.ListQuery{
				Mine:     in.Mine,
				Metadata: in.Metadata,
				Stale:    in.Stale,
				Deleted:  in.Deleted,
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

	register(b, s, p, &mcp.Tool{Name: "publish_version", Annotations: annotations("Publish version", false, false, true),
		Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in PublishInput) (WriteResult, string, error) {
			a, err := arts.Publish(ctx, actor, in.Slug, in.Version)
			if err != nil {
				return WriteResult{}, "", err
			}
			res := writeResult(links.Result(a, in.Version))
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "mark_stale", Annotations: annotations("Mark stale", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in StaleInput) (api.StaleResult, string, error) {
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
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in SlugInput) (WriteResult, string, error) {
			a, err := arts.UnmarkStale(ctx, actor, in.Slug)
			if err != nil {
				return WriteResult{}, "", err
			}
			res := writeResult(links.Result(a, 0))
			return res, "No longer stale. " + publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "set_visibility", Annotations: annotations("Set visibility", false, false, true)},
		auth.ScopeShare, func(ctx context.Context, _ *mcp.CallToolRequest, in VisibilityInput) (WriteResult, string, error) {
			a, err := arts.SetVisibility(ctx, actor, in.Slug, in.Visibility)
			if err != nil {
				return WriteResult{}, "", err
			}
			res := writeResult(links.Result(a, 0))
			return res, visibilityLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "create_share_link", Annotations: annotations("Create share link", false, false, false)},
		auth.ScopeShare, func(ctx context.Context, _ *mcp.CallToolRequest, in ShareLinkInput) (ShareLinkResult, string, error) {
			ttl, err := present.ParseDuration("expires_in", in.ExpiresIn)
			if err != nil {
				return ShareLinkResult{}, "", err
			}
			a, vs, err := arts.Get(ctx, actor, in.Slug)
			if err != nil {
				return ShareLinkResult{}, "", err
			}
			if in.Version < 0 || in.Version > a.LatestVersion {
				return ShareLinkResult{}, "", artifact.Errorf(artifact.ErrVersionNotFound, "%s has no version %d; its latest is %d",
					a.Slug, in.Version, a.LatestVersion)
			}
			if in.Version > 0 && !slices.ContainsFunc(vs, func(v artifact.Version) bool { return v.Number == in.Version }) {
				return ShareLinkResult{}, "", artifact.Errorf(artifact.ErrVersionNotFound, "version %d of %s was deleted",
					in.Version, a.Slug)
			}
			if slices.ContainsFunc(vs, func(v artifact.Version) bool { return v.Number == in.Version && v.Staged }) {
				return ShareLinkResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument,
					"version %d of %s is staged, and a share link does not open a staged version; publish it with "+
						"publish_version first, or leave out version", in.Version, a.Slug)
			}
			l, token, err := arts.CreateShareLink(ctx, actor, in.Slug, in.Label, ttl, in.Password)
			if err != nil {
				return ShareLinkResult{}, "", err
			}
			out := ShareLinkResult{URL: links.ShareURL(a.Slug, token), Slug: a.Slug, Visibility: string(a.Visibility),
				Prefix: l.Prefix, Label: l.Label, ExpiresAt: l.ExpiresAt, Password: l.HasPassword}
			who := "Anyone with this URL"
			if l.HasPassword {
				who = "Anyone with this URL and the password"
			}
			line := fmt.Sprintf("Share link for %s: %s (until %s). %s can open the page until then; "+
				"give it to the user, and revoke it with revoke_share_link prefix %q.",
				a.Slug, out.URL, l.ExpiresAt.Format(time.RFC3339), who, l.Prefix)
			if l.HasPassword {
				line += " Tell the user to send the password apart from the link."
			}
			if in.Version > 0 {
				latest := strings.TrimSuffix(links.ArtifactURL(a.Slug), "/")
				out.VersionURL = strings.TrimSuffix(links.VersionURL(a.Slug, in.Version), "/") + strings.TrimPrefix(out.URL, latest)
				line += fmt.Sprintf(" Version %d: %s.", in.Version, out.VersionURL)
			}
			if !a.Visibility.Gated() {
				line += fmt.Sprintf(" Note: %s is %s, so anyone can open it without the link; set_visibility \"members\" "+
					"limits it to members and link holders.", a.Slug, a.Visibility)
			}
			return out, line, nil
		})

	register(b, s, p, &mcp.Tool{Name: "list_share_links", Annotations: annotations("List share links", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, _ *mcp.CallToolRequest, in ListShareLinksInput) (api.ShareLinkList, string, error) {
			ls, err := arts.ListShareLinks(ctx, actor, in.Slug)
			if err != nil {
				return api.ShareLinkList{}, "", err
			}
			out := present.ShareLinks(ls, b.o.Now())
			active := 0
			for _, l := range out.Items {
				if l.Active {
					active++
				}
			}
			return out, fmt.Sprintf("%d share links of %s, %d active.", len(out.Items), in.Slug, active), nil
		})

	register(b, s, p, &mcp.Tool{Name: "revoke_share_link", Annotations: annotations("Revoke share link", false, true, true)},
		auth.ScopeShare, func(ctx context.Context, _ *mcp.CallToolRequest, in RevokeShareLinkInput) (api.OK, string, error) {
			if err := arts.RevokeShareLink(ctx, actor, in.Slug, in.Prefix); err != nil {
				return api.OK{}, "", err
			}
			return api.OK{OK: true}, fmt.Sprintf("Revoked share link %s of %s; browsers that opened it lose access on "+
				"their next request.", in.Prefix, in.Slug), nil
		})

	register(b, s, p, &mcp.Tool{Name: "delete_artifact", Annotations: annotations("Delete artifact", false, true, true)},
		auth.ScopeDelete, func(ctx context.Context, req *mcp.CallToolRequest, in DeleteInput) (api.OK, string, error) {
			if in.Hard && canConfirm(req) {
				answer, answered := req.Params.InputResponses[confirmPurge]
				if !answered {
					return api.OK{}, "", &inputRequired{requests: mcp.InputRequestMap{confirmPurge: purgeQuestion(in.Slug)}}
				}
				if !confirmed(answer) {
					return api.OK{OK: false}, fmt.Sprintf("Not purged: the user did not confirm, so %s is unchanged.", in.Slug), nil
				}
			}
			if err := arts.Delete(ctx, actor, in.Slug, in.Hard); err != nil {
				return api.OK{}, "", err
			}
			if in.Hard {
				return api.OK{OK: true}, fmt.Sprintf("Purged %s permanently.", in.Slug), nil
			}
			return api.OK{OK: true}, fmt.Sprintf("Deleted %s; it is offline and will be purged after the retention period. "+
				"update_artifact with restore: true brings it back until then.", in.Slug), nil
		})
}

const confirmPurge = "confirm_purge"

// canConfirm reports whether the client can answer an input_required form. Older
// protocols need an elicitation request, which a stateless server cannot send;
// those clients are left to the tool's destructive hint.
func canConfirm(req *mcp.CallToolRequest) bool {
	if req == nil || req.ProtocolVersion() < "2026-07-28" {
		return false
	}
	caps := req.ClientCapabilities()
	// An empty elicitation capability means form elicitation.
	return caps != nil && caps.Elicitation != nil && (caps.Elicitation.Form != nil || caps.Elicitation.URL == nil)
}

func purgeQuestion(slug string) *mcp.ElicitParams {
	return &mcp.ElicitParams{
		Mode: "form",
		Message: fmt.Sprintf("Permanently delete the artifact %q? Every version is deleted now and cannot be restored, "+
			"and the slug stays blocked for a while.", slug),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{
					"type":        "boolean",
					"title":       "Delete " + slug + " permanently",
					"description": "Tick to delete every version now.",
					"default":     false,
				},
			},
			"required": []string{"confirm"},
		},
	}
}

func confirmed(answer mcp.InputResponse) bool {
	r, ok := answer.(*mcp.ElicitResult)
	if !ok || r == nil || r.Action != "accept" {
		return false
	}
	v, _ := r.Content["confirm"].(bool)
	return v
}

// editLine names each edit's match so a fuzzy match is never silent.
func editLine(r EditFileResult) string {
	if r.Staged {
		return fmt.Sprintf("%s: edited %s; %s.", stagedLine(r.WriteResult), r.Path, editParts(r.Edits))
	}
	return fmt.Sprintf("Published %s (version %d): edited %s; %s.", r.URL, r.Version, r.Path, editParts(r.Edits))
}

// stagedLine says where a staged version opens, and who can open it.
func stagedLine(r WriteResult) string {
	return fmt.Sprintf("Staged version %d at %s, which only people signed in to this instance can open; %s still "+
		"serves version %d until publish_version %d", r.Version, r.VersionURL, r.URL, r.PublishedVersion, r.Version)
}

func editParts(edits []api.EditOutcome) string {
	parts := make([]string, len(edits))
	for i, e := range edits {
		where := fmt.Sprintf("lines %d-%d", e.StartLine, e.EndLine)
		if e.EndLine < e.StartLine {
			where = fmt.Sprintf("removed at line %d", e.StartLine)
		}
		parts[i] = fmt.Sprintf("edit %d %s (%s", i+1, where, e.Match)
		if e.Replacements > 1 {
			parts[i] += fmt.Sprintf(", %d places", e.Replacements)
		}
		parts[i] += ")"
	}
	return strings.Join(parts, "; ")
}

// visibilityLine says who can open the page now.
func visibilityLine(r WriteResult) string {
	if r.UnpublishedAt != nil || r.DeletedAt != nil {
		return publishedLine(r)
	}
	who := map[string]string{
		string(artifact.Public):   "public: anyone can open it, and it is listed",
		string(artifact.Unlisted): "unlisted: anyone with the URL can open it, and it is not listed",
		string(artifact.Members):  "members-only: people who sign in to this instance, or who hold a share link, can open it",
	}[r.Visibility]
	if who == "" {
		who = r.Visibility
	}
	return fmt.Sprintf("%s is now %s at %s (version %d).", r.Slug, who, r.URL, r.Version)
}

func publishedLine(r WriteResult) string {
	if r.Unchanged {
		return fmt.Sprintf("No change: the files match version %d; no version was added. %s serves version %d.",
			r.Version, r.URL, r.PublishedVersion)
	}
	line := fmt.Sprintf("Published %s (version %d)", r.URL, r.Version)
	if r.Staged {
		line = stagedLine(r)
	}
	switch {
	case r.DeletedAt != nil:
		line += "; the artifact is deleted, so the URL is offline"
	case r.UnpublishedAt != nil:
		line = fmt.Sprintf("Unpublished %s: offline until set_visibility public, unlisted or members (version %d kept)", r.URL, r.Version)
	}
	return line
}

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

func (b *builder) detail(ctx context.Context, actor artifact.Actor, slug string, withFiles bool, filesBefore int) (api.ArtifactDetail, error) {
	a, vs, err := b.d.Artifacts.Get(ctx, actor, slug)
	if err != nil {
		return api.ArtifactDetail{}, err
	}
	next := 0
	if withFiles {
		if next, err = b.d.Artifacts.ListFiles(ctx, vs, filesBefore); err != nil {
			return api.ArtifactDetail{}, err
		}
	}
	d := b.d.Links.Detail(a, vs)
	d.NextFilesBefore = next
	return d, nil
}

// files merges the inline files with uploadID's staged ones, inline winning on a
// path clash. Call done once the publish has succeeded or failed: it removes
// the downloads, and the upload only when consumed, so a refused call can be
// corrected and sent again with the same upload_id.
func (b *builder) files(ctx context.Context, p auth.Principal, uploadID string, raw []UpdateFileInput) ([]artifact.FileInput, func(consumed bool), error) {
	release := func(bool) error { return nil }
	var staged []artifact.FileInput
	if uploadID != "" {
		files, r, err := b.d.Uploads.Files(ctx, p.Handle, uploadID)
		if err != nil {
			return nil, nil, err
		}
		staged, release = files, r
	}
	inline, closeDownloads, err := b.inlineFiles(ctx, raw)
	if err != nil {
		b.release(ctx, uploadID, release, false)
		return nil, nil, err
	}
	done := func(consumed bool) {
		closeDownloads()
		b.release(ctx, uploadID, release, consumed)
	}
	return artifact.MergeFiles(inline, staged), done, nil
}

func (b *builder) release(ctx context.Context, uploadID string, release func(bool) error, consumed bool) {
	if err := release(consumed); err != nil {
		b.log.ErrorContext(ctx, "mcp upload cleanup failed", "upload", uploadID, "err", err)
	}
}

func edits(in []EditInput) []artifact.Edit {
	out := make([]artifact.Edit, len(in))
	for i, e := range in {
		out[i] = artifact.Edit(e)
	}
	return out
}

// checkVisibility refuses a visibility before any upload or download is
// opened for the call.
func checkVisibility(v string) error {
	if v == artifact.Unpublished {
		return nil
	}
	_, err := artifact.ParseVisibility(v)
	return err
}

// update applies req, then a visibility through SetVisibility, so
// "unpublished" works as in set_visibility; a call that only sets the
// visibility changes nothing else.
func (b *builder) update(ctx context.Context, actor artifact.Actor, req artifact.UpdateRequest, vis string) (*artifact.Artifact, *artifact.Version, error) {
	only := vis != "" && len(req.Files) == 0 && req.Name == nil && req.Description == nil && len(req.Metadata) == 0 &&
		!req.Restore && req.ExpectedVersion == 0
	var (
		a   *artifact.Artifact
		v   *artifact.Version
		err error
	)
	if !only {
		if a, v, err = b.d.Artifacts.Update(ctx, actor, req); err != nil {
			return nil, nil, err
		}
	}
	if vis != "" {
		if a, err = b.d.Artifacts.SetVisibility(ctx, actor, req.Slug, vis); err != nil {
			return nil, nil, err
		}
	}
	return a, v, nil
}

func createFiles(in []FileInput) []UpdateFileInput {
	out := make([]UpdateFileInput, len(in))
	for i, f := range in {
		out[i] = UpdateFileInput{Path: f.Path, Content: f.Content, Encoding: f.Encoding, ContentType: f.ContentType, URL: f.URL}
	}
	return out
}

// inlineFiles returns the files of a call, with those named by url
// downloaded; the func it returns removes the downloads.
func (b *builder) inlineFiles(ctx context.Context, in []UpdateFileInput) ([]artifact.FileInput, func(), error) {
	dl := b.d.Fetch.Downloads(ctx, b.o.MaxFileBytes, b.o.MaxVersionBytes)
	out := make([]artifact.FileInput, 0, len(in))
	for _, f := range in {
		var (
			fi  artifact.FileInput
			err error
		)
		switch {
		case f.Delete:
			fi = artifact.FileInput{Path: f.Path, Delete: true}
		case len(f.Edits) > 0 && (f.URL != "" || f.Content != nil):
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has edits and also content or url; give one", f.Path)
		case len(f.Edits) > 0:
			fi = artifact.FileInput{Path: f.Path, Edits: edits(f.Edits), ContentType: f.ContentType}
		case f.URL != "" && f.Content != nil:
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has both url and content", f.Path)
		case f.URL != "":
			fi, err = dl.Add(f.Path, f.URL, f.ContentType)
		case f.Content == nil:
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has no content: give content (\"\" for an empty "+
				"file), url, edits or delete: true", f.Path)
		default:
			var data string
			if data, err = decode(f.Path, *f.Content, f.Encoding); err == nil {
				fi = artifact.FileInput{Path: f.Path, Content: strings.NewReader(data), ContentType: f.ContentType}
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

// binaryExts are the extensions whose content is never text, so base64
// sent without encoding is a slip, not the file.
var binaryExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true,
	".ico": true, ".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".pdf": true, ".zip": true, ".mp4": true,
	".webm": true, ".mp3": true}

// decode returns raw bytes in a string; base64 may be unpadded, contain line
// breaks and start with a data: URL's "data:<type>;base64,".
func decode(path, content, encoding string) (string, error) {
	switch strings.ToLower(encoding) {
	case "", "utf8", "utf-8", "text":
		if encoding == "" && binaryExts[strings.ToLower(pathpkg.Ext(path))] && looksBase64(content) {
			return "", artifact.Errorf(artifact.ErrInvalidArgument, "content of %q looks like base64, but encoding is "+
				"not set, so it would be stored as text: add encoding: \"base64\"", path)
		}
		return content, nil
	case "base64":
		if rest, ok := strings.CutPrefix(content, "data:"); ok {
			if _, data, ok := strings.Cut(rest, ";base64,"); ok {
				content = data
			}
		}
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

// looksBase64 reports whether s is base64 or a base64 data: URL.
func looksBase64(s string) bool {
	if rest, ok := strings.CutPrefix(s, "data:"); ok {
		_, _, ok = strings.Cut(rest, ";base64,")
		return ok
	}
	s = strings.TrimRight(strings.NewReplacer("\r", "", "\n", "").Replace(s), "=")
	if len(s) < 8 {
		return false
	}
	_, err := base64.RawStdEncoding.DecodeString(s)
	return err == nil
}

// parseGrace returns 0, the instance default, for "".
func parseGrace(s string) (time.Duration, error) { return present.ParseDuration("grace", s) }

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
