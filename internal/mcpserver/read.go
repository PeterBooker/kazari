package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	register(b, s, p, &mcp.Tool{Name: "read_file", Annotations: annotations("Read file", true, false, true)},
		auth.ScopeRead, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadFileInput) (ReadFileResult, string, error) {
			out, err := b.readFile(ctx, actor, in)
			return out, "", err
		})
}

// readFile returns the result of read_file with its content blocks.
func (b *builder) readFile(ctx context.Context, actor artifact.Actor, in ReadFileInput) (ReadFileResult, error) {
	a, _, err := b.d.Artifacts.Get(ctx, actor, in.Slug)
	if err != nil {
		return ReadFileResult{}, err
	}
	version := in.Version
	if version == 0 {
		version = a.PublishedVersion
	}
	head := func(path, what string) *mcp.TextContent {
		return &mcp.TextContent{Text: readHeader(path, version, a.PublishedVersion, a.LatestVersion, what)}
	}
	if in.StartLine > 0 || in.EndLine > 0 {
		w, note, err := b.readWindow(ctx, actor, in, version, a.LatestVersion)
		if err != nil {
			return ReadFileResult{}, err
		}
		w.blocks = []mcp.Content{head(w.Path, fmt.Sprintf("lines %d-%d of %d", w.StartLine, w.EndLine, w.TotalLines)),
			&mcp.TextContent{Text: w.Content}}
		if note != "" {
			w.blocks = append(w.blocks, &mcp.TextContent{Text: note})
		}
		return w, nil
	}
	fc, err := b.readInline(ctx, actor, in.Slug, in.Path, version, b.o.MaxInlineReadBytes, MaxInlineBlobBytes)
	f := fc.file
	out := ReadFileResult{Path: f.Path, ContentType: f.ContentType, Size: f.Size, Hash: f.Hash,
		Version: version, LatestVersion: a.LatestVersion}
	switch {
	case errors.Is(err, errTooLargeToInline):
		out.Encoding = "url"
		out.URL, err = b.downloadURL(ctx, actor, in.Slug, f.Path, version)
		if err != nil {
			return ReadFileResult{}, err
		}
		msg := fmt.Sprintf("%s is %d bytes (%s, sha256 %s), too large to return inline. Download it from %s "+
			"with the API key as a bearer token.", f.Path, f.Size, f.ContentType, f.Hash, out.URL)
		out.blocks = []mcp.Content{head(f.Path, artifact.FormatBytes(f.Size)), &mcp.TextContent{Text: msg}}
	case err != nil:
		return ReadFileResult{}, err
	case fc.text:
		out.Encoding = "utf8"
		body := string(fc.data)
		if in.Anchors {
			body, out.Anchored = artifact.AnchorLines(body), true
		}
		out.Content = body
		out.blocks = []mcp.Content{head(f.Path, lineCount(fc.data)), &mcp.TextContent{Text: body}}
	default:
		out.Encoding = "base64"
		out.blocks = []mcp.Content{head(f.Path, artifact.FormatBytes(f.Size)+" "+f.ContentType),
			&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI:      fileURI(in.Slug, f.Path),
				MIMEType: f.ContentType,
				Blob:     fc.data,
			}}}
	}
	return out, nil
}

// readHeader is the first line of every read: which version was read, and
// what an edit of that text needs, which matters most after a rollback.
func readHeader(path string, version, published, latest int, what string) string {
	var state string
	switch {
	case version != latest:
		state = fmt.Sprintf("latest %d; edit_file and update_artifact need base_version: %d to change this text", latest, version)
	case published != latest:
		state = fmt.Sprintf("the latest; version %d is published, so edit_file and update_artifact need base_version: %d, "+
			"or stage: true, to change this text", published, version)
	default:
		state = "the latest"
	}
	return fmt.Sprintf("%s, version %d (%s), %s", path, version, state, what)
}

// lineCount counts as artifact.AnchorLines does: "a\nb\n" is two lines.
func lineCount(data []byte) string {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
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
		Description: "An artifact's record and versions with the first page of file lists, as JSON (the get_artifact result with include_files).",
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
		d, err := b.detail(ctx, actor, slug, true, 0)
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
				"%s is %s, more than the %s resource limit; use read_file for a download URL",
				fc.file.Path, size(fc.file.Size), size(b.o.MaxInlineReadBytes))}, cause: err}
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
// says what the window covers and where to continue when it was cut short. It
// holds the window, not the file.
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
	w := lineWindow{start: start, end: end, limit: b.o.MaxInlineReadBytes, anchors: in.Anchors}
	valid, n, err := w.scan(io.LimitReader(rc, b.o.MaxFileBytes+1))
	if err != nil {
		return ReadFileResult{}, "", fmt.Errorf("mcpserver: read %s/%s: %w", in.Slug, f.Path, err)
	}
	if n > b.o.MaxFileBytes {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrTooLarge,
			"%s is larger than the %s file limit", f.Path, size(b.o.MaxFileBytes))
	}
	if !valid {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument, "%s is not UTF-8 text", f.Path)
	}
	total, last := w.total, w.last
	if start > total {
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrInvalidArgument,
			"start_line %d is past the end of %s, which has %d lines", start, f.Path, total)
	}
	if w.last == 0 { // line start exists, so it did not fit
		return ReadFileResult{}, "", artifact.Errorf(artifact.ErrTooLarge,
			"line %d of %s is %d bytes, more than the %s inline limit", start, f.Path, w.tooLong, size(b.o.MaxInlineReadBytes))
	}
	if end == 0 || end > total {
		end = total
	}
	out := ReadFileResult{Path: f.Path, ContentType: f.ContentType, Size: f.Size, Hash: f.Hash, Encoding: "utf8",
		Content: w.sb.String(), Anchored: in.Anchors, Version: version, LatestVersion: latest,
		StartLine: start, EndLine: last, TotalLines: total}
	note := ""
	if last < end {
		note = fmt.Sprintf("The window was cut at the %s inline limit: continue from start_line %d.",
			size(b.o.MaxInlineReadBytes), last+1)
	}
	return out, note, nil
}

// windowChunkBytes is the read size of lineWindow.scan.
const windowChunkBytes = 32 << 10

// lineWindow keeps lines start to end (0 for the last) of a file read once, in
// at most limit bytes, however large the file. It counts lines as
// artifact.AnchorLines does: "a\nb\n" is two lines and "" none.
type lineWindow struct {
	start, end int
	limit      int64
	anchors    bool

	sb      strings.Builder
	line    []byte // the line being read, while it can still fit
	lineLen int64  // that line's length so far, kept or not
	total   int    // lines finished
	last    int    // last line in sb
	cut     bool   // a line did not fit, so sb is complete
	tooLong int64  // length of line start, if it did not fit
}

// scan reads r to its end, so the line count and the UTF-8 check cover the whole
// file. It stops early only at invalid UTF-8, returning valid false.
func (w *lineWindow) scan(r io.Reader) (valid bool, n int64, err error) {
	buf := make([]byte, windowChunkBytes)
	have := 0
	for {
		m, err := r.Read(buf[have:])
		have += m
		n += int64(m)
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return false, n, err
		}
		whole := have
		if !eof {
			whole -= partialRune(buf[:have])
		}
		if !utf8.Valid(buf[:whole]) {
			return false, n, nil
		}
		w.write(buf[:whole])
		have = copy(buf, buf[whole:have])
		if eof {
			break
		}
	}
	if w.lineLen > 0 {
		w.endLine()
	}
	return true, n, nil
}

// partialRune returns how many bytes at the end of p start a character that p cuts off.
func partialRune(p []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(p); i++ {
		if tail := p[len(p)-i:]; utf8.RuneStart(tail[0]) {
			if utf8.FullRune(tail) {
				return 0
			}
			return i
		}
	}
	return 0
}

func (w *lineWindow) write(p []byte) {
	for {
		i := bytes.IndexByte(p, '\n')
		seg := p
		if i >= 0 {
			seg = p[:i]
		}
		w.lineLen += int64(len(seg))
		if w.inWindow(w.total+1) && int64(w.sb.Len())+w.lineLen <= w.limit {
			w.line = append(w.line, seg...)
		}
		if i < 0 {
			return
		}
		w.endLine()
		p = p[i+1:]
	}
}

func (w *lineWindow) inWindow(n int) bool {
	return !w.cut && n >= w.start && (w.end == 0 || n <= w.end)
}

// endLine adds the finished line to the window if it fits; a line that does not
// ends the window, as an error when it is the first.
func (w *lineWindow) endLine() {
	w.total++
	n := w.total
	if w.inWindow(n) && !w.keep(n) {
		w.cut = true
		if n == w.start {
			w.tooLong = w.lineLen
		}
	}
	w.line, w.lineLen = w.line[:0], 0
}

func (w *lineWindow) keep(n int) bool {
	if int64(w.sb.Len())+w.lineLen > w.limit {
		return false
	}
	piece := string(w.line)
	if w.anchors {
		piece = artifact.Anchor(n, piece) + "|" + piece
	}
	add := len(piece)
	if n > w.start {
		add++ // the line break before it
	}
	if int64(w.sb.Len()+add) > w.limit {
		return false
	}
	if n > w.start {
		w.sb.WriteByte('\n')
	}
	w.sb.WriteString(piece)
	w.last = n
	return true
}
