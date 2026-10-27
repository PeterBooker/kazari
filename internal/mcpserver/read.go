package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/present"
)

// {+path} is reserved expansion, so a path may contain slashes.
const (
	resourcePrefix       = "kazari://artifacts/"
	artifactTemplate     = resourcePrefix + "{slug}"
	fileTemplate         = resourcePrefix + "{slug}/files/{+path}"
	filesSegment         = "/files/"
	artifactResourceMIME = "application/json"
)

var errTooLargeToInline = errors.New("too large to return inline")

func fileURI(slug, path string) string {
	return resourcePrefix + slug + filesSegment + path
}

func isText(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(contentType))
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/ecmascript", "application/xml",
		"application/xhtml+xml", "application/ld+json", "application/manifest+json", "image/svg+xml",
		"application/x-javascript", "application/toml", "application/yaml", "application/x-yaml":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

type fileContent struct {
	file artifact.File
	data []byte
	text bool // data is valid UTF-8 of a textual type
}

// readInline reads a file if it fits: text up to textLimit, anything else up to
// blobLimit. Otherwise it returns the file entry with errTooLargeToInline.
func (b *builder) readInline(ctx context.Context, actor artifact.Actor, slug, path string, version int, textLimit, blobLimit int64) (fileContent, error) {
	f, rc, err := b.d.Artifacts.OpenFile(ctx, actor, slug, path, version)
	if err != nil {
		return fileContent{}, err
	}
	defer rc.Close()
	text := isText(f.ContentType)
	limit := blobLimit
	if text {
		limit = max(textLimit, blobLimit)
	}
	if f.Size > limit {
		return fileContent{file: f}, errTooLargeToInline
	}
	data, more, err := readAtMost(rc, limit)
	if err != nil {
		return fileContent{}, fmt.Errorf("mcpserver: read %s/%s: %w", slug, f.Path, err)
	}
	if more {
		return fileContent{file: f}, errTooLargeToInline
	}
	text = text && int64(len(data)) <= textLimit && utf8.Valid(data)
	if !text && int64(len(data)) > blobLimit {
		return fileContent{file: f}, errTooLargeToInline
	}
	return fileContent{file: f, data: data, text: text}, nil
}

func (b *builder) addReadFile(s *mcp.Server, p auth.Principal, actor artifact.Actor) {
	t := &mcp.Tool{
		Name:        "read_file",
		Description: b.text.tools["read_file"],
		Annotations: annotations("Read file", true, false, true),
	}
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (res *mcp.CallToolResult, out ReadFileResult, err error) {
		defer b.recoverTool(ctx, t.Name, func(e error) { res, out, err = nil, ReadFileResult{}, e })
		if err := requireScope(p, auth.ScopeRead); err != nil {
			return nil, ReadFileResult{}, b.toolError(ctx, t.Name, err)
		}
		a, _, err := b.d.Artifacts.Get(ctx, actor, in.Slug)
		if err != nil {
			return nil, ReadFileResult{}, b.toolError(ctx, t.Name, err)
		}
		version := in.Version
		if version == 0 {
			version = a.PublishedVersion
		}
		if in.StartLine > 0 || in.EndLine > 0 {
			w, note, err := b.readWindow(ctx, actor, in, version, a.LatestVersion)
			if err != nil {
				return nil, ReadFileResult{}, b.toolError(ctx, t.Name, err)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: w.Content}, &mcp.TextContent{Text: note}}}, w, nil
		}
		fc, err := b.readInline(ctx, actor, in.Slug, in.Path, version, b.o.MaxInlineReadBytes, MaxInlineBlobBytes)
		f := fc.file
		out = ReadFileResult{Path: f.Path, ContentType: f.ContentType, Size: f.Size, Hash: f.Hash,
			Version: version, LatestVersion: a.LatestVersion}
		switch {
		case errors.Is(err, errTooLargeToInline):
			out.Encoding = "url"
			out.URL, err = b.downloadURL(ctx, actor, in.Slug, f.Path, version)
			if err != nil {
				return nil, ReadFileResult{}, b.toolError(ctx, t.Name, err)
			}
			msg := fmt.Sprintf("%s is %d bytes (%s, sha256 %s), too large to return inline. Download it from %s "+
				"with the API key as a bearer token.", f.Path, f.Size, f.ContentType, f.Hash, out.URL)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, out, nil
		case err != nil:
			return nil, ReadFileResult{}, b.toolError(ctx, t.Name, err)
		case fc.text:
			out.Encoding = "utf8"
			body := string(fc.data)
			if in.Anchors {
				body, out.Anchored = artifact.AnchorLines(body), true
			}
			out.Content = body
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, out, nil
		default:
			out.Encoding = "base64"
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI:      fileURI(in.Slug, f.Path),
				MIMEType: f.ContentType,
				Blob:     fc.data,
			}}}}, out, nil
		}
	})
}

// downloadURL pins version 0 to the published version.
func (b *builder) downloadURL(ctx context.Context, actor artifact.Actor, slug, path string, version int) (string, error) {
	if version == 0 {
		a, _, err := b.d.Artifacts.Get(ctx, actor, slug)
		if err != nil {
			return "", err
		}
		version = a.PublishedVersion
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	base := b.o.APIBaseURL
	if base == "" {
		base = "/v1"
	}
	return base + "/artifacts/" + url.PathEscape(slug) + "/files/" + strings.Join(segs, "/") + "?version=" + strconv.Itoa(version), nil
}

func (b *builder) addResources(s *mcp.Server, p auth.Principal) {
	actor := present.Actor(p)
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "artifact",
		Title:       "Artifact record",
		Description: "An artifact's record and versions with their file lists, as JSON (the get_artifact result with include_files).",
		MIMEType:    artifactResourceMIME,
		URITemplate: artifactTemplate,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (res *mcp.ReadResourceResult, err error) {
		uri := req.Params.URI
		defer b.recoverTool(ctx, uri, func(e error) { res, err = nil, e })
		if err := requireScope(p, auth.ScopeRead); err != nil {
			return nil, b.toolError(ctx, "resource", err)
		}
		slug, ok := strings.CutPrefix(uri, resourcePrefix)
		if !ok || slug == "" || strings.Contains(slug, "/") {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		d, err := b.detail(ctx, actor, slug, true)
		if err != nil {
			return nil, b.resourceError(ctx, uri, err)
		}
		js, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("mcpserver: encode %s: %w", uri, err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: artifactResourceMIME, Text: string(js)}}}, nil
	})

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:  "artifact-file",
		Title: "Artifact file",
		Description: fmt.Sprintf("One file of an artifact's published version, with its own content type: text inline, "+
			"other files as a base64 blob, up to %s; larger files are an error (use read_file for a download URL).",
			size(b.o.MaxInlineReadBytes)),
		URITemplate: fileTemplate,
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (res *mcp.ReadResourceResult, err error) {
		uri := req.Params.URI
		defer b.recoverTool(ctx, uri, func(e error) { res, err = nil, e })
		if err := requireScope(p, auth.ScopeRead); err != nil {
			return nil, b.toolError(ctx, "resource", err)
		}
		slug, path, ok := parseFileURI(uri)
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		fc, err := b.readInline(ctx, actor, slug, path, 0, b.o.MaxInlineReadBytes, b.o.MaxInlineReadBytes)
		if errors.Is(err, errTooLargeToInline) {
			return nil, &toolError{e: api.Error{Code: api.CodeTooLarge, Message: fmt.Sprintf(
				"%s is %d bytes, more than the %s resource limit; use read_file for a download URL",
				fc.file.Path, fc.file.Size, size(b.o.MaxInlineReadBytes))}, cause: err}
		}
		if err != nil {
			return nil, b.resourceError(ctx, uri, err)
		}
		c := &mcp.ResourceContents{URI: uri, MIMEType: fc.file.ContentType}
		if fc.text {
			c.Text = string(fc.data)
		} else {
			c.Blob = fc.data
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{c}}, nil
	})
}

func (b *builder) resourceError(ctx context.Context, uri string, err error) error {
	switch artifact.CodeOf(err) {
	case api.CodeNotFound, api.CodeFileNotFound, api.CodeVersionNotFound, api.CodeGone:
		return mcp.ResourceNotFoundError(uri)
	}
	return b.toolError(ctx, "resource", err)
}

// parseFileURI splits a file URI; the path may be percent-encoded.
func parseFileURI(uri string) (slug, path string, ok bool) {
	rest, ok := strings.CutPrefix(uri, resourcePrefix)
	if !ok {
		return "", "", false
	}
	slug, raw, ok := strings.Cut(rest, filesSegment)
	if !ok || slug == "" || raw == "" || strings.Contains(slug, "/") {
		return "", "", false
	}
	path, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", false
	}
	return slug, path, true
}

func (b *builder) addPrompt(s *mcp.Server) {
	s.AddPrompt(&mcp.Prompt{
		Name:        "publish-page",
		Title:       "Publish a page",
		Description: "Write a static web page and publish it to Kazari, following the hosting conventions (single index.html, relative links, allowed CDNs, size limits).",
		Arguments: []*mcp.PromptArgument{{
			Name:        "topic",
			Description: "what the page is about (optional)",
		}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := strings.TrimSpace(req.Params.Arguments["topic"])
		return &mcp.GetPromptResult{
			Description: "Kazari publishing conventions",
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: b.text.promptText(b.o, topic)},
			}},
		}, nil
	})
}

// readWindow returns lines start_line to end_line of a text file of any size up
// to the file limit: as many whole lines as fit the inline limit, with anchors
// that carry their absolute line numbers so edit_file can use them. The note
// says what the window covers and where to continue when it was cut short.
func (b *builder) readWindow(ctx context.Context, actor artifact.Actor, in ReadFileInput, version, latest int) (ReadFileResult, string, error) {
	start, end := in.StartLine, in.EndLine
	if start <= 0 {
		start = 1
	}
	if end != 0 && end < start {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument, "end_line %d is before start_line %d", end, start)
	}
	f, rc, err := b.d.Artifacts.OpenFile(ctx, actor, in.Slug, in.Path, version)
	if err != nil {
		return ReadFileResult{}, "", err
	}
	defer rc.Close()
	if !isText(f.ContentType) {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument,
			"%s is %s, not a text file; start_line and end_line read text files only", f.Path, f.ContentType)
	}
	if f.Size > b.o.MaxFileBytes {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrTooLarge,
			"%s is %d bytes, more than the %s file limit", f.Path, f.Size, size(b.o.MaxFileBytes))
	}
	data, more, err := readAtMost(rc, b.o.MaxFileBytes)
	if err != nil {
		return ReadFileResult{}, "", fmt.Errorf("mcpserver: read %s/%s: %w", in.Slug, f.Path, err)
	}
	if more {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrTooLarge,
			"%s is larger than the %s file limit", f.Path, size(b.o.MaxFileBytes))
	}
	if !utf8.Valid(data) {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument, "%s is not UTF-8 text", f.Path)
	}
	lines := splitLines(string(data))
	total := len(lines)
	if start > total {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument,
			"start_line %d is past the end of %s, which has %d lines", start, f.Path, total)
	}
	if end == 0 || end > total {
		end = total
	}
	var sb strings.Builder
	last := start - 1
	for n := start; n <= end; n++ {
		piece := lines[n-1]
		if in.Anchors {
			piece = artifact.Anchor(n, piece) + "|" + piece
		}
		add := len(piece)
		if n > start {
			add++ // the line break before it
		}
		if int64(sb.Len()+add) > b.o.MaxInlineReadBytes {
			if n == start {
				return ReadFileResult{}, "", artifact.Errorf(artifact.ErrTooLarge,
					"line %d of %s is %d bytes, more than the %s inline limit", n, f.Path, len(lines[n-1]), size(b.o.MaxInlineReadBytes))
			}
			break
		}
		if n > start {
			sb.WriteByte('\n')
		}
		sb.WriteString(piece)
		last = n
	}
	out := ReadFileResult{Path: f.Path, ContentType: f.ContentType, Size: f.Size, Hash: f.Hash, Encoding: "utf8",
		Content: sb.String(), Anchored: in.Anchors, Version: version, LatestVersion: latest,
		StartLine: start, EndLine: last, TotalLines: total}
	note := fmt.Sprintf("Lines %d-%d of %d in %s.", start, last, total, f.Path)
	if last < end {
		note += fmt.Sprintf(" The window was cut at the %s inline limit: continue from start_line %d.",
			size(b.o.MaxInlineReadBytes), last+1)
	}
	return out, note, nil
}

// splitLines counts "a\nb\n" as two lines and "" as none, as artifact.AnchorLines does.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
