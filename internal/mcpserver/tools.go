package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

// FileInput is an inline file of create_artifact.
type FileInput struct {
	Path        string `json:"path" jsonschema:"path inside the artifact, relative, e.g. index.html or assets/app.js"`
	Content     string `json:"content,omitempty" jsonschema:"file content: UTF-8 text, or base64 when encoding is base64"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default, for text) or base64 (for binary)"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type; derived from the file extension when omitted"`
	URL         string `json:"url,omitempty" jsonschema:"http or https address the server downloads as the file's content, instead of content"`
}

// UpdateFileInput is an inline file or a deletion of update_artifact.
type UpdateFileInput struct {
	Path        string `json:"path" jsonschema:"path inside the artifact, relative, e.g. index.html or assets/app.js"`
	Content     string `json:"content,omitempty" jsonschema:"file content: UTF-8 text, or base64 when encoding is base64"`
	Encoding    string `json:"encoding,omitempty" jsonschema:"utf8 (default, for text) or base64 (for binary)"`
	ContentType string `json:"content_type,omitempty" jsonschema:"MIME type; derived from the file extension when omitted"`
	URL         string `json:"url,omitempty" jsonschema:"http or https address the server downloads as the file's content, instead of content"`
	Delete      bool   `json:"delete,omitempty" jsonschema:"true removes this path from the new version; content is ignored"`
}

// CreateInput is the input of create_artifact.
type CreateInput struct {
	Slug        string            `json:"slug" jsonschema:"permanent URL name: 3-63 of a-z 0-9 and -, no leading/trailing hyphen, no --"`
	Name        string            `json:"name" jsonschema:"human-readable title"`
	Description string            `json:"description" jsonschema:"one or two sentences on what the page is"`
	Metadata    map[string]string `json:"metadata,omitempty" jsonschema:"string labels; kazari.spa, kazari.indexable and kazari.csp are interpreted by the server"`
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
	Visibility      *string            `json:"visibility,omitempty" jsonschema:"public, unlisted or members"`
	Label           string             `json:"label,omitempty" jsonschema:"optional note on the new version"`
	ExpectedVersion int                `json:"expected_version,omitempty" jsonschema:"latest_version you last saw; a newer one fails with version_conflict"`
	KeepStale       bool               `json:"keep_stale,omitempty" jsonschema:"keep a pending stale mark instead of clearing it"`
	Restore         bool               `json:"restore,omitempty" jsonschema:"bring back a soft-deleted artifact (get_artifact shows deleted_at) before applying the other changes"`
}

// EditInput is one edit of edit_file: a text edit or an anchor edit.
type EditInput struct {
	OldText    string `json:"old_text,omitempty" jsonschema:"text edit: the exact text to replace, copied from the file; it must match once unless replace_all"`
	NewText    string `json:"new_text" jsonschema:"the replacement; empty deletes the text or lines"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"text edit: replace every match of old_text"`
	Start      string `json:"start,omitempty" jsonschema:"anchor edit: anchor of the first line to replace, e.g. 12:3fa0, from read_file with anchors: true"`
	End        string `json:"end,omitempty" jsonschema:"anchor edit: anchor of the last line to replace, inclusive; default start"`
}

// EditFileInput is the input of edit_file.
type EditFileInput struct {
	Slug            string      `json:"slug" jsonschema:"artifact to change"`
	Path            string      `json:"path" jsonschema:"text file to edit, e.g. index.html"`
	Edits           []EditInput `json:"edits" jsonschema:"text edits or anchor edits, not both, applied to the latest version"`
	ExpectedVersion int         `json:"expected_version,omitempty" jsonschema:"latest_version you read; a newer one fails with version_conflict"`
	Label           string      `json:"label,omitempty" jsonschema:"optional note on the new version"`
	KeepStale       bool        `json:"keep_stale,omitempty" jsonschema:"keep a pending stale mark instead of clearing it"`
}

// editFileSchema constrains each edit to exactly one of a non-empty old_text and start.
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
	s.Properties["edits"].Items.OneOf = []*jsonschema.Schema{kind("old_text"), kind("start")}
	return s
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

// DiscardUploadInput is the input of discard_upload.
type DiscardUploadInput struct {
	UploadID string `json:"upload_id" jsonschema:"id returned by begin_upload"`
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
	ExpiresIn string `json:"expires_in,omitempty" jsonschema:"how long the link works, e.g. 72h or 14d; default 7d"`
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

// register adds a tool limited to principals holding scope. Its text content is
// the summary line followed by the result's JSON.
func register[In, Out any](b *builder, s *mcp.Server, p auth.Principal, t *mcp.Tool, scope string, h handler[In, Out]) {
	t.Description = b.text.tools[t.Name]
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		var zero Out
		defer b.recoverTool(ctx, t.Name, func(e error) { res, out, err = nil, zero, e })
		if err := requireScope(p, scope); err != nil {
			return nil, zero, b.toolError(ctx, t.Name, err)
		}
		if scope == auth.ScopeWrite || scope == auth.ScopeDelete {
			if d := b.d.Writes.Reserve(ctx, ratelimit.CredentialKey(p)); d > 0 {
				return nil, zero, b.toolError(ctx, t.Name, &api.Error{Code: api.CodeRateLimited,
					Message: "too many writes; retry in " + ratelimit.RetryAfter(d) + "s"})
			}
		}
		out, summary, err := h(ctx, req, in)
		var ir *inputRequired
		if errors.As(err, &ir) {
			return &mcp.CallToolResult{InputRequests: ir.requests}, zero, nil
		}
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
	var wire *api.Error
	if errors.As(err, &wire) {
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

	register(b, s, p, &mcp.Tool{Name: "create_artifact", Annotations: annotations("Create artifact", false, false, false),
		Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateInput) (api.PublishResult, string, error) {
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

	if b.o.Fetch {
		b.addImport(s, p, actor)
	}

	register(b, s, p, &mcp.Tool{Name: "update_artifact", Annotations: annotations("Update artifact", false, false, false),
		Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in UpdateInput) (api.PublishResult, string, error) {
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
				Restore:         in.Restore,
				Files:           files,
			}
			// Models fill optional fields with "": that leaves the
			// visibility alone rather than changing it.
			if in.Visibility != nil && *in.Visibility != "" {
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

	register(b, s, p, &mcp.Tool{Name: "edit_file", InputSchema: b.editSchema,
		Annotations: annotations("Edit file", false, false, false), Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in EditFileInput) (api.EditResult, string, error) {
			edits := make([]artifact.Edit, len(in.Edits))
			for i, e := range in.Edits {
				edits[i] = artifact.Edit(e)
			}
			a, v, outs, err := arts.Edit(ctx, actor, artifact.EditRequest{
				Slug:            in.Slug,
				Path:            in.Path,
				Edits:           edits,
				Label:           in.Label,
				ExpectedVersion: in.ExpectedVersion,
				KeepStale:       in.KeepStale,
			})
			if err != nil {
				return api.EditResult{}, "", err
			}
			clean, _ := artifact.ValidatePath(in.Path) // valid: Edit accepted it
			res := links.Edit(a, v.Number, clean, outs)
			return res, editLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "begin_upload", Annotations: annotations("Begin chunked upload", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, _ BeginUploadInput) (api.Upload, string, error) {
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
			d, err := b.detail(ctx, actor, in.Slug, in.IncludeFiles)
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
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in PublishInput) (api.PublishResult, string, error) {
			a, err := arts.Publish(ctx, actor, in.Slug, in.Version)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, in.Version)
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
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in SlugInput) (api.PublishResult, string, error) {
			a, err := arts.UnmarkStale(ctx, actor, in.Slug)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, 0)
			return res, "No longer stale. " + publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "set_visibility", Annotations: annotations("Set visibility", false, false, true)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in VisibilityInput) (api.PublishResult, string, error) {
			a, err := arts.SetVisibility(ctx, actor, in.Slug, in.Visibility)
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := links.Result(a, 0)
			return res, publishedLine(res), nil
		})

	register(b, s, p, &mcp.Tool{Name: "create_share_link", Annotations: annotations("Create share link", false, false, false)},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in ShareLinkInput) (api.CreatedShareLink, string, error) {
			ttl, err := present.ParseDuration("expires_in", in.ExpiresIn)
			if err != nil {
				return api.CreatedShareLink{}, "", err
			}
			l, token, err := arts.CreateShareLink(ctx, actor, in.Slug, in.Label, ttl)
			if err != nil {
				return api.CreatedShareLink{}, "", err
			}
			a, _, err := arts.Get(ctx, actor, in.Slug)
			if err != nil {
				return api.CreatedShareLink{}, "", err
			}
			out := links.CreatedShareLink(a, l, token, b.o.Now())
			line := fmt.Sprintf("Share link for %s: %s (until %s). Anyone with this URL can open the page until then; "+
				"give it to the user, and revoke it with revoke_share_link prefix %q.",
				a.Slug, out.URL, l.ExpiresAt.Format(time.RFC3339), l.Prefix)
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
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in RevokeShareLinkInput) (api.OK, string, error) {
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
func editLine(r api.EditResult) string {
	parts := make([]string, len(r.Edits))
	for i, e := range r.Edits {
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
	return fmt.Sprintf("Published %s (version %d): edited %s; %s.", r.URL, r.Version, r.Path, strings.Join(parts, "; "))
}

func publishedLine(r api.PublishResult) string {
	line := fmt.Sprintf("Published %s (version %d)", r.URL, r.Version)
	switch {
	case r.Artifact.DeletedAt != nil:
		line += "; the artifact is deleted, so the URL is offline"
	case r.Artifact.UnpublishedAt != nil:
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

// files merges the inline files with uploadID's staged ones, inline winning on a
// path clash. The cleanup removes the upload; it is never nil and must be called
// even on error, so a failed call still consumes the upload.
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
	inline, closeDownloads, err := b.inlineFiles(ctx, raw)
	if err != nil {
		return nil, cleanup, err
	}
	done := cleanup
	cleanup = func() {
		closeDownloads()
		done()
	}
	return artifact.MergeFiles(inline, staged), cleanup, nil
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
		case f.URL != "" && f.Content != "":
			err = artifact.Errorf(artifact.ErrInvalidArgument, "file %q has both url and content", f.Path)
		case f.URL != "":
			fi, err = dl.Add(f.Path, f.URL, f.ContentType)
		default:
			var data string
			if data, err = decode(f.Path, f.Content, f.Encoding); err == nil {
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

// decode returns raw bytes in a string; base64 may be unpadded and contain line breaks.
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
