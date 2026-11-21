// Package present converts domain types to the api wire types and domain errors
// to HTTP statuses, so the REST API and the MCP server agree.
package present

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

// Links builds public URLs. URL takes a host label ("demo" or "demo--v3")
// and returns the root URL of that host, e.g. https://demo.art.example.com/.
type Links struct {
	URL func(sub string) string
}

// ArtifactURL is the URL of a slug's published version.
func (l Links) ArtifactURL(slug string) string {
	if l.URL == nil {
		return ""
	}
	return l.URL(slug)
}

// VersionURL is the pinned URL of one version.
func (l Links) VersionURL(slug string, n int) string {
	if l.URL == nil {
		return ""
	}
	return l.URL(slug + artifact.VersionSeparator + strconv.Itoa(n))
}

// Artifact converts an artifact record.
func (l Links) Artifact(a *artifact.Artifact) api.Artifact {
	md := a.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return api.Artifact{
		Slug:             a.Slug,
		Name:             a.Name,
		Description:      a.Description,
		Owner:            a.OwnerHandle,
		Visibility:       string(a.Visibility),
		Metadata:         md,
		URL:              l.ArtifactURL(a.Slug),
		PublishedVersion: a.PublishedVersion,
		LatestVersion:    a.LatestVersion,
		SizeBytes:        a.SizeBytes,
		StaleAt:          a.StaleAt,
		ExpiresAt:        a.ExpiresAt,
		UnpublishedAt:    a.UnpublishedAt,
		DeletedAt:        a.DeletedAt,
		PurgedAt:         a.PurgedAt,
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}

// Artifacts converts a list page.
func (l Links) Artifacts(p artifact.ListPage) api.ArtifactList {
	out := api.ArtifactList{Items: make([]api.Artifact, 0, len(p.Items)), NextCursor: p.NextCursor}
	for i := range p.Items {
		out.Items = append(out.Items, l.Artifact(&p.Items[i]))
	}
	return out
}

// Version converts a version.
func (l Links) Version(slug string, v artifact.Version) api.Version {
	out := api.Version{
		Number:    v.Number,
		Label:     v.Label,
		CreatedBy: v.CreatedBy,
		CreatedAt: v.CreatedAt,
		SizeBytes: v.SizeBytes,
		URL:       l.VersionURL(slug, v.Number),
	}
	for _, f := range v.Files {
		out.Files = append(out.Files, File(f))
	}
	return out
}

// File converts a file entry.
func File(f artifact.File) api.File {
	return api.File{Path: f.Path, Hash: f.Hash, Size: f.Size, ContentType: f.ContentType}
}

// Detail converts an artifact with its versions.
func (l Links) Detail(a *artifact.Artifact, vs []artifact.Version) api.ArtifactDetail {
	d := api.ArtifactDetail{Artifact: l.Artifact(a), Versions: make([]api.Version, 0, len(vs))}
	for _, v := range vs {
		d.Versions = append(d.Versions, l.Version(a.Slug, v))
	}
	return d
}

// Result converts the outcome of a create, update or publish; version 0 means the
// published version.
func (l Links) Result(a *artifact.Artifact, version int) api.PublishResult {
	if version == 0 {
		version = a.PublishedVersion
	}
	return api.PublishResult{URL: l.ArtifactURL(a.Slug), Version: version, Artifact: l.Artifact(a)}
}

// Imported is Result with the import report attached.
func (l Links) Imported(a *artifact.Artifact, version int, r *artifact.ImportReport) api.PublishResult {
	res := l.Result(a, version)
	res.Import = Import(r)
	return res
}

// Import converts an import report, with empty lists rather than null.
func Import(r *artifact.ImportReport) *api.ImportReport {
	out := &api.ImportReport{
		Source:         r.Source,
		Fetched:        r.Fetched,
		Skipped:        make([]api.SkippedFetch, 0, len(r.Skipped)),
		ScriptsRemoved: r.ScriptsRemoved,
		Notes:          append([]string{}, r.Notes...),
		Outline:        make([]api.Landmark, 0, len(r.Outline)),
	}
	for _, s := range r.Skipped {
		out.Skipped = append(out.Skipped, api.SkippedFetch(s))
	}
	for _, l := range r.Outline {
		out.Outline = append(out.Outline, api.Landmark(l))
	}
	return out
}

// ShareURL is the URL that opens a share link: the gate's share path on
// the artifact's latest host, with the token.
func (l Links) ShareURL(slug, token string) string {
	base := strings.TrimSuffix(l.ArtifactURL(slug), "/")
	return base + gate.SharePath + "?" + url.Values{"token": {token}}.Encode()
}

// ShareLink converts a share link; Active is its state at now.
func ShareLink(sl *artifact.ShareLink, now time.Time) api.ShareLink {
	return api.ShareLink{
		Prefix:     sl.Prefix,
		Label:      sl.Label,
		CreatedBy:  sl.CreatedBy,
		CreatedAt:  sl.CreatedAt,
		ExpiresAt:  sl.ExpiresAt,
		RevokedAt:  sl.RevokedAt,
		LastUsedAt: sl.LastUsedAt,
		Uses:       sl.Uses,
		Active:     sl.Active(now),
	}
}

// ShareLinks converts a listing of share links.
func ShareLinks(ls []artifact.ShareLink, now time.Time) api.ShareLinkList {
	out := api.ShareLinkList{Items: make([]api.ShareLink, 0, len(ls))}
	for i := range ls {
		out.Items = append(out.Items, ShareLink(&ls[i], now))
	}
	return out
}

// CreatedShareLink converts a new link and its token for the artifact a.
func (l Links) CreatedShareLink(a *artifact.Artifact, sl *artifact.ShareLink, token string, now time.Time) api.CreatedShareLink {
	return api.CreatedShareLink{URL: l.ShareURL(a.Slug, token), ShareLink: ShareLink(sl, now), Artifact: l.Artifact(a)}
}

// ParseDuration reads a duration given on the wire: a Go duration such as
// 72h, or whole days such as 14d. Empty is 0. field names the input in the
// error, which is ErrInvalidArgument.
func ParseDuration(field, s string) (time.Duration, error) {
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
		return 0, artifact.Errorf(artifact.ErrInvalidArgument, "%s %q must be a positive duration such as 72h or 14d", field, s)
	}
	return d, nil
}

// ParseOverlap is ParseDuration for a value that may be zero ("0", "0s"):
// the overlap of a key rotation. Empty is def.
func ParseOverlap(field, s string, def time.Duration) (time.Duration, error) {
	switch s = strings.TrimSpace(s); s {
	case "":
		return def, nil
	case "0", "0s", "0h", "0d":
		return 0, nil
	}
	return ParseDuration(field, s)
}

// RequireShare refuses a principal without artifacts:share that names a
// visibility: the scope covers who can open an artifact, and without it a
// new page gets the instance's default and no call changes one.
func RequireShare(p auth.Principal, visibility string) error {
	if visibility == "" || p.HasScope(auth.ScopeShare) {
		return nil
	}
	return artifact.Errorf(artifact.ErrForbidden,
		"scope %q required to set visibility; leave visibility out to keep the artifact's, or the instance default on a new one",
		auth.ScopeShare)
}

// Edit converts the outcome of an edit of path that produced version.
func (l Links) Edit(a *artifact.Artifact, version int, path string, outs []artifact.EditOutcome) api.EditResult {
	r := api.EditResult{URL: l.ArtifactURL(a.Slug), Version: version, Path: path,
		Edits: make([]api.EditOutcome, 0, len(outs)), Artifact: l.Artifact(a)}
	for _, o := range outs {
		r.Edits = append(r.Edits, api.EditOutcome(o))
	}
	return r
}

// Stale converts the outcome of MarkStale.
func (l Links) Stale(a *artifact.Artifact) api.StaleResult {
	r := api.StaleResult{Artifact: l.Artifact(a)}
	if a.StaleAt != nil {
		r.StaleAt = *a.StaleAt
	}
	if a.ExpiresAt != nil {
		r.ExpiresAt = *a.ExpiresAt
	}
	return r
}

// Upload converts an upload record.
func Upload(u *upload.Upload, maxChunk, maxTotal int64) api.Upload {
	return api.Upload{UploadID: u.ID, ExpiresAt: u.ExpiresAt, MaxChunkBytes: maxChunk, MaxTotalBytes: maxTotal}
}

// ArchiveURL is where an upload's archive is PUT; base is the REST root,
// e.g. https://api.art.example.com/v1. The token travels in the query, as
// presigned URLs do everywhere, so one plain URL is all a shell needs.
func ArchiveURL(base, id, token string) string {
	return strings.TrimSuffix(base, "/") + "/uploads/" + url.PathEscape(id) + "/archive?token=" + url.QueryEscape(token)
}

// Chunk converts a staged file after a chunk was appended.
func Chunk(f upload.StagedFile) api.ChunkResult {
	return api.ChunkResult{Path: f.Path, BytesReceived: f.Bytes}
}

// User converts a user.
func User(u *user.User) api.User {
	return api.User{
		Handle:      u.Handle,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		QuotaBytes:  u.QuotaBytes,
		DisabledAt:  u.DisabledAt,
		CreatedAt:   u.CreatedAt,
	}
}

// WithAccount adds how the person signs in to their wire type.
func WithAccount(u api.User, a user.Account) api.User {
	u.Password, u.Invited, u.Keys, u.LastActiveAt = a.HasPassword, a.Invited, a.LiveKeys, a.LastActive()
	return u
}

// AccountLinkURL is the address on the apex that a link's token opens. The
// token travels in the query string, which the access log and the spans
// leave out, never in the path.
func (l Links) AccountLinkURL(purpose user.Purpose, token string) string {
	return l.URL("") + "account/" + string(purpose) + "?token=" + url.QueryEscape(token)
}

// Link converts a link without its token.
func Link(l *user.Link) api.Link {
	return api.Link{
		ID:        l.ID,
		User:      l.UserHandle,
		Purpose:   string(l.Purpose),
		RevokeAll: l.RevokeAll,
		CreatedBy: l.CreatedBy,
		CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt,
	}
}

// CreatedLink converts a link that was just made, with its address.
func (l Links) CreatedLink(link *user.Link, token string) api.CreatedLink {
	return api.CreatedLink{Link: Link(link), URL: l.AccountLinkURL(link.Purpose, token)}
}

// Ready converts a readiness report.
func Ready(r preflight.Report) api.Ready {
	out := api.Ready{Ready: !r.Failed(), CheckedAt: r.At, Checks: make([]api.Check, 0, len(r.Findings))}
	for _, f := range r.Findings {
		out.Checks = append(out.Checks, api.Check{Check: f.Check, Status: string(f.Status), Detail: f.Detail})
	}
	return out
}

// Identity converts a linked identity.
func Identity(id user.Identity) api.Identity {
	return api.Identity{Issuer: id.Issuer, Subject: id.Subject, Email: id.EmailAtLogin, LastLoginAt: id.LastLoginAt}
}

// Key converts an API key (never the secret).
func Key(k *user.APIKey) api.APIKey {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return api.APIKey{
		Prefix:     k.Prefix,
		Label:      k.Label,
		Scopes:     scopes,
		Slugs:      k.Slugs,
		IssuedBy:   k.IssuedBy,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		RevokedAt:  k.RevokedAt,
		CreatedAt:  k.CreatedAt,
	}
}

// Me converts a principal.
func Me(p auth.Principal, used int64) api.Me {
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return api.Me{Handle: p.Handle, Admin: p.Admin, Scopes: scopes, Source: p.Source, Slugs: p.Slugs, ExpiresAt: p.ExpiresAt,
		QuotaBytes: p.QuotaBytes, UsedBytes: used}
}

// Actor converts a principal to the artifact service's caller.
func Actor(p auth.Principal) artifact.Actor {
	return artifact.Actor{Handle: p.Handle, Admin: p.Admin, QuotaBytes: p.QuotaBytes, Credential: credential(p),
		CanDelete: p.HasScope(auth.ScopeDelete), Slugs: p.Slugs,
		Operator: p.Source == auth.SourceAdminToken || p.Source == auth.SourceLocal}
}

// credential names p's credential in the audit trail: "api_key:<prefix>",
// "oauth:<id>", or the source alone ("admin_token") without a key id.
func credential(p auth.Principal) string {
	switch {
	case p.Source == "":
		return p.KeyID
	case p.KeyID == "":
		return p.Source
	case strings.HasPrefix(p.KeyID, p.Source+":"):
		return p.KeyID
	}
	return p.Source + ":" + p.KeyID
}

// Job converts a scheduler status.
func Job(s scheduler.JobStatus) api.JobStatus {
	return api.JobStatus{Name: s.Name, Every: s.Every.String(), LastRun: s.LastRun, LastError: s.LastError, NextRun: s.NextRun, Running: s.Running}
}

// AuditEvent converts an audit row.
func AuditEvent(e audit.Event) api.AuditEvent {
	return api.AuditEvent{At: e.At, Actor: e.Actor, Action: e.Action, Slug: e.Slug, Details: e.Details}
}

// Error maps a domain error to an HTTP status and wire error. Unknown
// errors become 500 internal with a generic message so nothing leaks.
func Error(err error) (int, api.Error) {
	var om *upload.OffsetMismatchError
	if errors.As(err, &om) {
		cur := om.Current
		return http.StatusConflict, api.Error{Code: api.CodeOffsetMismatch, Message: om.Error(), Current: &cur}
	}
	var ae *artifact.Error
	if errors.As(err, &ae) {
		return statusFor(ae.Code), api.Error{Code: ae.Code, Message: ae.Message}
	}
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		return http.StatusUnauthorized, api.Error{Code: api.CodeUnauthorized, Message: "invalid or expired token"}
	case errors.Is(err, user.ErrNotFound):
		return http.StatusNotFound, api.Error{Code: api.CodeNotFound, Message: "user not found"}
	case errors.Is(err, user.ErrKeyNotFound):
		return http.StatusNotFound, api.Error{Code: api.CodeNotFound, Message: "key not found"}
	case errors.Is(err, user.ErrHandleTaken):
		return http.StatusConflict, api.Error{Code: api.CodeHandleTaken, Message: err.Error()}
	case errors.Is(err, user.ErrEmailTaken):
		return http.StatusConflict, api.Error{Code: api.CodeEmailTaken, Message: err.Error()}
	case errors.Is(err, user.ErrInvalidHandle), errors.Is(err, user.ErrInvalidRole), errors.Is(err, user.ErrInvalidScope),
		errors.Is(err, user.ErrInvalidKey):
		return http.StatusUnprocessableEntity, api.Error{Code: api.CodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, user.ErrKeyTooWide):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: err.Error()}
	case errors.Is(err, user.ErrDisabled):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: err.Error()}
	case errors.Is(err, user.ErrNotProvisioned):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: err.Error()}
	case errors.Is(err, user.ErrLastAdmin):
		return http.StatusConflict, api.Error{Code: api.CodeLastAdmin, Message: "the last active admin cannot be disabled or demoted; make another person an admin first"}
	case errors.Is(err, user.ErrSelf):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: "nobody disables or demotes their own account; ask another admin"}
	case errors.Is(err, user.ErrTooManyKeys):
		return http.StatusForbidden, api.Error{Code: api.CodeTooManyKeys, Message: err.Error()}
	case errors.Is(err, user.ErrSetupDone):
		return http.StatusConflict, api.Error{Code: api.CodeSetupDone, Message: "the instance already has users; an admin invites the others"}
	case errors.Is(err, user.ErrLocalOff):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: "built-in accounts are off on this instance; people sign in at the identity provider"}
	case errors.Is(err, user.ErrLinkNotFound):
		return http.StatusNotFound, api.Error{Code: api.CodeNotFound, Message: "link not found, or it was used, replaced or has expired"}
	case errors.Is(err, user.ErrWeakPassword):
		return http.StatusUnprocessableEntity, api.Error{Code: api.CodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, user.ErrBadPassword):
		return http.StatusUnauthorized, api.Error{Code: api.CodeUnauthorized, Message: "wrong name or password"}
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, api.Error{Code: api.CodeTooLarge, Message: fmt.Sprintf("request body exceeds %d bytes", mbe.Limit)}
	}
	return http.StatusInternalServerError, api.Error{Code: api.CodeInternal, Message: "internal error"}
}

// PageCode is the error code an HTML page's status stands for on its
// request's span, in the API's vocabulary, so a trace reads the same
// whichever surface failed. A 2xx or 3xx has none.
func PageCode(status int) string {
	switch {
	case status < http.StatusBadRequest:
		return ""
	case status == http.StatusUnauthorized:
		return api.CodeUnauthorized
	case status == http.StatusForbidden:
		return api.CodeForbidden
	case status == http.StatusNotFound:
		return api.CodeNotFound
	case status == http.StatusTooManyRequests:
		return api.CodeRateLimited
	case status >= http.StatusInternalServerError:
		return api.CodeInternal
	}
	return api.CodeInvalidArgument
}

func statusFor(code string) int {
	switch code {
	case api.CodeNotFound, api.CodeVersionNotFound, api.CodeFileNotFound, api.CodeUploadNotFound, api.CodeLinkNotFound:
		return http.StatusNotFound
	case api.CodeSlugTaken, api.CodeOffsetMismatch, api.CodeAnchorMismatch:
		return http.StatusConflict
	case api.CodeVersionConflict:
		return http.StatusPreconditionFailed
	case api.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case api.CodeTooManyFiles, api.CodeInvalidSlug, api.CodeInvalidPath, api.CodeInvalidMetadata, api.CodeInvalidArgument,
		api.CodeEditNotFound, api.CodeEditAmbiguous, api.CodeDigestMismatch:
		return http.StatusUnprocessableEntity
	case api.CodeQuotaExceeded, api.CodeTooManyUploads:
		return http.StatusForbidden
	case api.CodeForbidden:
		return http.StatusForbidden
	case api.CodeUnauthorized:
		return http.StatusUnauthorized
	case api.CodeGone:
		return http.StatusGone
	case api.CodeRateLimited:
		return http.StatusTooManyRequests
	case api.CodeFetchFailed:
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}
