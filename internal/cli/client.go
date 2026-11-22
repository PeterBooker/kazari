// Package cli is the kazari command-line client: the subcommands behind Run
// and an HTTP client for the user and admin APIs.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/buildinfo"
)

const maxErrorBody = 64 << 10

// A 429 or 503 is retried this many times, after the Retry-After the server
// sent, at most maxRetryWait.
const (
	maxRetries     = 2
	maxRetryWait   = 30 * time.Second
	defaultBackoff = time.Second
)

// Client calls one API host (api.<base> or admin.<base>) and is safe for
// concurrent use.
type Client struct {
	base      string
	token     string
	hc        *http.Client
	userAgent string
	// local marks a client on the admin Unix socket, which reaches a server
	// on this host and nothing else.
	local bool
	// host is the first label of the host this client is meant for, api or
	// admin, which an error suggests when the URL names another.
	host  string
	sleep func(context.Context, time.Duration) error
}

// NewClient returns a client for the host root baseURL, without /v1. An empty
// token sends no Authorization; a nil hc means http.DefaultClient.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		base:      strings.TrimRight(baseURL, "/"),
		token:     token,
		hc:        hc,
		userAgent: "kazari-cli/" + buildinfo.Get().Version,
		host:      "api",
		sleep:     sleepContext,
	}
}

// HTTPError is a non-2xx response. A body that is not an api.ErrorResponse
// becomes code "http_<status>" with a message that says so.
type HTTPError struct {
	Status int
	Err    api.Error
}

// Error renders "code: message".
func (e *HTTPError) Error() string { return e.Err.Code + ": " + e.Err.Message }

// Unwrap exposes the *api.Error to errors.As.
func (e *HTTPError) Unwrap() error { return &e.Err }

// IsCode reports whether err is an HTTPError carrying code.
func IsCode(err error, code string) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Err.Code == code
}

// path is already escaped and relative to /v1.
type request struct {
	method      string
	path        string
	query       url.Values
	body        io.Reader
	contentType string
	header      http.Header
	// replay gives the body again for a retry; nil when it cannot be resent.
	replay func() io.Reader
}

// retryable is a read, or a write the server refuses rather than repeats
// once it has happened: one that names the version it expects.
func (r request) retryable() bool {
	switch r.method {
	case http.MethodGet, http.MethodHead:
		return true
	}
	return r.replay != nil && r.header.Get("If-Match") != ""
}

// send returns the response only for a 2xx status, else an *HTTPError.
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	u := c.base + "/v1" + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	for attempt := 0; ; attempt++ {
		body := r.body
		if attempt > 0 && r.replay != nil {
			body = r.replay()
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u, body)
		if err != nil {
			return nil, fmt.Errorf("build %s %s: %w", r.method, r.path, err)
		}
		maps.Copy(req.Header, r.header)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", "application/json")
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", r.method, r.path, err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, nil
		}
		busy := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable
		if busy && attempt < maxRetries && r.retryable() {
			wait := retryAfter(resp.Header.Get("Retry-After"))
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
			resp.Body.Close()
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		defer resp.Body.Close()
		return nil, c.decodeError(resp)
	}
}

// retryAfter reads Retry-After in seconds or as a date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	d := defaultBackoff
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), maxRetryWait)
}

// do decodes the JSON response into out; a nil out discards it.
func (c *Client) do(ctx context.Context, r request, out any) error {
	resp, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", r.method, r.path, err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, header http.Header, in, out any) error {
	r := request{method: method, path: path, query: query, header: header}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s %s body: %w", method, path, err)
		}
		r.body = bytes.NewReader(b)
		r.replay = func() io.Reader { return bytes.NewReader(b) }
		r.contentType = "application/json"
	}
	return c.do(ctx, r, out)
}

// decodeError never passes on a body that is not the API's error: a proxy's
// or a page's HTML says nothing about what to change.
func (c *Client) decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var er api.ErrorResponse
	if json.Unmarshal(body, &er) == nil && er.Error.Code != "" {
		return &HTTPError{Status: resp.StatusCode, Err: er.Error}
	}
	status := strconv.Itoa(resp.StatusCode)
	if t := http.StatusText(resp.StatusCode); t != "" {
		status += " " + t
	}
	text := strings.TrimSpace(string(body))
	html := strings.Contains(resp.Header.Get("Content-Type"), "html") || strings.HasPrefix(text, "<")
	var msg string
	switch {
	case c.local:
		msg = "HTTP " + status + " from the admin socket"
	case html || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		msg = c.notKazari(status)
	default:
		msg = "HTTP " + status + " from " + c.base
		if line, _, _ := strings.Cut(text, "\n"); line != "" && len(line) <= 200 {
			msg += ": " + line
		}
	}
	return &HTTPError{Status: resp.StatusCode, Err: api.Error{Code: "http_" + strconv.Itoa(resp.StatusCode), Message: msg}}
}

// notKazari explains an answer from something other than the API, and
// suggests the host the API usually is on.
func (c *Client) notKazari(status string) string {
	msg := fmt.Sprintf("%s answered HTTP %s, which is not the Kazari API; it is usually https://%s.<base>", c.base, status, c.host)
	u, err := url.Parse(c.base)
	if err != nil || u.Host == "" {
		return msg
	}
	first, rest, _ := strings.Cut(u.Host, ".")
	switch {
	case first == c.host:
		return msg
	case (first == "api" || first == "mcp" || first == "admin") && rest != "":
		u.Host = c.host + "." + rest
	default:
		u.Host = c.host + "." + u.Host
	}
	u.Scheme = "https"
	return msg + "; try --url " + u.String()
}

// decodeSlice accepts a bare array or an object with exactly one array field,
// so the client does not depend on the server's envelope for simple lists.
func decodeSlice[T any](c *Client, ctx context.Context, path string, query url.Values) ([]T, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, query, nil, nil, &raw); err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	var out []T
	if len(raw) > 0 && raw[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("decode GET %s response: %w", path, err)
		}
		var found []json.RawMessage
		for _, v := range obj {
			if v = bytes.TrimSpace(v); len(v) > 0 && v[0] == '[' {
				found = append(found, v)
			}
		}
		if len(found) != 1 {
			return nil, fmt.Errorf("decode GET %s response: expected an array", path)
		}
		raw = found[0]
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode GET %s response: %w", path, err)
	}
	return out, nil
}

func seg(s string) string { return url.PathEscape(s) }

func filePath(p string) string {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func ifMatch(version int) http.Header {
	if version <= 0 {
		return nil
	}
	return http.Header{"If-Match": {strconv.Quote(strconv.Itoa(version))}}
}

// multipartBody streams through a pipe so the bundle is never buffered. The
// transport closes the returned reader, which stops the writer.
func multipartBody(manifest any, bundle io.Reader) (io.ReadCloser, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeMultipart(mw, manifest, bundle))
	}()
	return pr, mw.FormDataContentType()
}

func writeMultipart(mw *multipart.Writer, manifest any, bundle io.Reader) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="manifest"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("manifest part: %w", err)
	}
	if err := json.NewEncoder(part).Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="bundle"; filename="bundle.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	part, err = mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("bundle part: %w", err)
	}
	if _, err := io.Copy(part, bundle); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	return mw.Close()
}

// ListParams filters GET /artifacts. Zero fields are omitted.
type ListParams struct {
	Mine       string // created | updated | any
	Metadata   map[string]string
	Stale      string // include | exclude | only
	Deleted    string
	Search     string // text in the slug or the name
	Visibility string // public | unlisted | members | unpublished
	Owner      string // a handle; the admin list alone reads it
	Cursor     string
	Limit      int
}

func (p ListParams) values() url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("mine", p.Mine)
	for k, v := range p.Metadata {
		q.Set("metadata."+k, v)
	}
	set("stale", p.Stale)
	set("deleted", p.Deleted)
	set("q", p.Search)
	set("visibility", p.Visibility)
	set("owner", p.Owner)
	set("cursor", p.Cursor)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// Create publishes a new artifact from a JSON body.
func (c *Client) Create(ctx context.Context, req api.CreateArtifactRequest) (*api.PublishResult, error) {
	var out api.PublishResult
	if err := c.doJSON(ctx, http.MethodPost, "/artifacts", nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateBundle publishes a new artifact from a streamed tar.gz bundle.
func (c *Client) CreateBundle(ctx context.Context, manifest api.CreateManifest, bundle io.Reader) (*api.PublishResult, error) {
	body, ct := multipartBody(manifest, bundle)
	var out api.PublishResult
	err := c.do(ctx, request{method: http.MethodPost, path: "/artifacts", body: body, contentType: ct}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Update adds a version and/or changes fields from a JSON body.
func (c *Client) Update(ctx context.Context, slug string, req api.UpdateArtifactRequest) (*api.PublishResult, error) {
	var out api.PublishResult
	err := c.doJSON(ctx, http.MethodPut, "/artifacts/"+seg(slug), nil, ifMatch(req.ExpectedVersion), req, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateBundle adds a version from a streamed tar.gz bundle. A positive
// manifest.ExpectedVersion is also sent as If-Match.
func (c *Client) UpdateBundle(ctx context.Context, slug string, manifest api.UpdateArtifactRequest, bundle io.Reader) (*api.PublishResult, error) {
	body, ct := multipartBody(manifest, bundle)
	var out api.PublishResult
	err := c.do(ctx, request{
		method:      http.MethodPut,
		path:        "/artifacts/" + seg(slug),
		body:        body,
		contentType: ct,
		header:      ifMatch(manifest.ExpectedVersion),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Patch changes fields only: name, description, metadata, visibility.
func (c *Client) Patch(ctx context.Context, slug string, req api.UpdateArtifactRequest) (*api.PublishResult, error) {
	var out api.PublishResult
	if err := c.doJSON(ctx, http.MethodPatch, "/artifacts/"+seg(slug), nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns the artifact and its versions; files includes each version's
// file list.
func (c *Client) Get(ctx context.Context, slug string, files bool) (*api.ArtifactDetail, error) {
	var q url.Values
	if files {
		q = url.Values{"files": {"true"}}
	}
	var out api.ArtifactDetail
	if err := c.doJSON(ctx, http.MethodGet, "/artifacts/"+seg(slug), q, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LatestVersion asks VersionFiles for the latest version.
const LatestVersion = -1

// VersionFiles returns the artifact with version's files: the published
// version's when version is 0, the latest's when it is LatestVersion.
func (c *Client) VersionFiles(ctx context.Context, slug string, version int) (*api.ArtifactDetail, *api.Version, error) {
	get := func(before int) (*api.ArtifactDetail, error) {
		q := url.Values{"files": {"true"}}
		if before > 0 {
			q.Set("files_before", strconv.Itoa(before))
		}
		var out api.ArtifactDetail
		if err := c.doJSON(ctx, http.MethodGet, "/artifacts/"+seg(slug), q, nil, nil, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	before := 0
	if version > 0 {
		before = version + 1
	}
	d, err := get(before)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case version == LatestVersion:
		version = d.Artifact.LatestVersion
	case version == 0:
		version = d.Artifact.PublishedVersion
		if d.NextFilesBefore != 0 && version < d.NextFilesBefore {
			if d, err = get(version + 1); err != nil {
				return nil, nil, err
			}
		}
	}
	for i := range d.Versions {
		if d.Versions[i].Number == version {
			return d, &d.Versions[i], nil
		}
	}
	return nil, nil, &HTTPError{Status: http.StatusNotFound, Err: api.Error{
		Code:    api.CodeVersionNotFound,
		Message: fmt.Sprintf("%s has no version %d; its latest is %d", slug, version, d.Artifact.LatestVersion),
	}}
}

// List returns one page of the artifacts the caller can see.
func (c *Client) List(ctx context.Context, p ListParams) (*api.ArtifactList, error) {
	var out api.ArtifactList
	if err := c.doJSON(ctx, http.MethodGet, "/artifacts", p.values(), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Publish moves the published pointer to version (rollback).
func (c *Client) Publish(ctx context.Context, slug string, version int) (*api.PublishResult, error) {
	var out api.PublishResult
	err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/publish", nil, nil, api.PublishRequest{Version: version}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkStale marks the artifact stale; grace is a Go duration, "" for the
// server default.
func (c *Client) MarkStale(ctx context.Context, slug, grace string) (*api.StaleResult, error) {
	var out api.StaleResult
	err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/stale", nil, nil, api.StaleRequest{Grace: grace}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UnmarkStale clears the stale mark.
func (c *Client) UnmarkStale(ctx context.Context, slug string) (*api.PublishResult, error) {
	var out api.PublishResult
	if err := c.doJSON(ctx, http.MethodDelete, "/artifacts/"+seg(slug)+"/stale", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateShareLink creates a share link expiring after expiresIn, such as 72h
// or 7d ("" for a week). The result's URL carries the token.
func (c *Client) CreateShareLink(ctx context.Context, slug, label, expiresIn string) (*api.CreatedShareLink, error) {
	var out api.CreatedShareLink
	err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/share-links", nil, nil,
		api.ShareLinkRequest{Label: label, ExpiresIn: expiresIn}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListShareLinks lists an artifact's share links, newest first.
func (c *Client) ListShareLinks(ctx context.Context, slug string) (*api.ShareLinkList, error) {
	var out api.ShareLinkList
	if err := c.doJSON(ctx, http.MethodGet, "/artifacts/"+seg(slug)+"/share-links", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeShareLink revokes the share link with prefix.
func (c *Client) RevokeShareLink(ctx context.Context, slug, prefix string) error {
	return c.doJSON(ctx, http.MethodDelete, "/artifacts/"+seg(slug)+"/share-links/"+seg(prefix), nil, nil, nil, nil)
}

// SetVisibility sets public, unlisted, members or unpublished.
func (c *Client) SetVisibility(ctx context.Context, slug, visibility string) (*api.PublishResult, error) {
	return c.Patch(ctx, slug, api.UpdateArtifactRequest{Visibility: &visibility})
}

// Delete soft-deletes the artifact, or purges it when hard is set.
func (c *Client) Delete(ctx context.Context, slug string, hard bool) error {
	var q url.Values
	if hard {
		q = url.Values{"hard": {"true"}}
	}
	return c.doJSON(ctx, http.MethodDelete, "/artifacts/"+seg(slug), q, nil, nil, nil)
}

// ReadFile opens one file of the published version, or of version when
// positive, and returns its Content-Type; the caller closes the body. With
// anchors, each text line is prefixed by its anchor for Edit's Start and End.
func (c *Client) ReadFile(ctx context.Context, slug, path string, version int, anchors bool) (io.ReadCloser, string, error) {
	q := url.Values{}
	if version > 0 {
		q.Set("version", strconv.Itoa(version))
	}
	if anchors {
		q.Set("anchors", "true")
	}
	resp, err := c.send(ctx, request{
		method: http.MethodGet,
		path:   "/artifacts/" + seg(slug) + "/files/" + filePath(path),
		query:  q,
	})
	if err != nil {
		return nil, "", err
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// Edit applies edits to one text file of the latest version and publishes
// it; a positive ExpectedVersion fails with version_conflict if stale.
func (c *Client) Edit(ctx context.Context, slug string, req api.EditRequest) (*api.EditResult, error) {
	var out api.EditResult
	if err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/edit", nil, ifMatch(req.ExpectedVersion), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Skill returns the instance's Agent Skill as SKILL.md text.
func (c *Client) Skill(ctx context.Context) (string, error) {
	resp, err := c.send(ctx, request{method: http.MethodGet, path: "/skill"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read GET /skill response: %w", err)
	}
	return string(b), nil
}

// Me returns the caller's identity and scopes.
func (c *Client) Me(ctx context.Context) (*api.Me, error) {
	var out api.Me
	if err := c.doJSON(ctx, http.MethodGet, "/me", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MyKeys lists the caller's own API keys (never the secrets).
func (c *Client) MyKeys(ctx context.Context) ([]api.APIKey, error) {
	return decodeSlice[api.APIKey](c, ctx, "/me/keys", nil)
}

// IssueMyKey issues a key for the caller, bounded by the key the client
// holds. The secret is in the result exactly once.
func (c *Client) IssueMyKey(ctx context.Context, req api.IssueKeyRequest) (*api.IssuedKey, error) {
	var out api.IssuedKey
	if err := c.doJSON(ctx, http.MethodPost, "/me/keys", nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeMyKey revokes one of the caller's keys and the keys issued with it.
func (c *Client) RevokeMyKey(ctx context.Context, prefix string) error {
	return c.doJSON(ctx, http.MethodDelete, "/me/keys/"+seg(prefix), nil, nil, nil, nil)
}

// RotateMyKey replaces one of the caller's keys; the old one works until
// overlap has passed ("" is the server's default).
func (c *Client) RotateMyKey(ctx context.Context, prefix, overlap string) (*api.IssuedKey, error) {
	var out api.IssuedKey
	err := c.doJSON(ctx, http.MethodPost, "/me/keys/"+seg(prefix)+"/rotate", nil, nil, api.RotateKeyRequest{Overlap: overlap}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// BeginUpload starts a chunked upload.
func (c *Client) BeginUpload(ctx context.Context) (*api.Upload, error) {
	var out api.Upload
	if err := c.doJSON(ctx, http.MethodPost, "/uploads", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadChunk appends r to path at offset. contentType sets the file's type
// on the first chunk; "" sends application/octet-stream.
func (c *Client) UploadChunk(ctx context.Context, id, path string, offset int64, r io.Reader, contentType string) (*api.ChunkResult, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var out api.ChunkResult
	err := c.do(ctx, request{
		method:      http.MethodPatch,
		path:        "/uploads/" + seg(id) + "/files/" + filePath(path),
		query:       url.Values{"offset": {strconv.FormatInt(offset, 10)}},
		body:        r,
		contentType: contentType,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AbortUpload discards a chunked upload.
func (c *Client) AbortUpload(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodDelete, "/uploads/"+seg(id), nil, nil, nil, nil)
}
