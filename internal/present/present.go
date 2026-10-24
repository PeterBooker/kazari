// Package present converts domain types to the wire types in package api
// and maps domain errors to HTTP statuses. Both the REST API and the MCP
// server use it, so an artifact looks the same on every surface.
package present

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/audit"
	"kazari/internal/auth"
	"kazari/internal/scheduler"
	"kazari/internal/upload"
	"kazari/internal/user"
)

// Links builds public URLs. URL takes a host label ("demo" or "demo--v3")
// and returns the root URL of that host, e.g. https://demo.art.example.com/.
type Links struct {
	URL func(sub string) string
}

// ArtifactURL is the latest-version URL of a slug.
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

// Version converts a version; Files are included when present.
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

// Result converts the outcome of a create, update or publish. version is
// the version the call produced or moved to (0 falls back to the published
// version).
func (l Links) Result(a *artifact.Artifact, version int) api.PublishResult {
	if version == 0 {
		version = a.PublishedVersion
	}
	return api.PublishResult{URL: l.ArtifactURL(a.Slug), Version: version, Artifact: l.Artifact(a)}
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
	return api.Me{Handle: p.Handle, Admin: p.Admin, Scopes: scopes, Source: p.Source, QuotaBytes: p.QuotaBytes, UsedBytes: used}
}

// Actor converts a principal to the artifact service's caller.
func Actor(p auth.Principal) artifact.Actor {
	return artifact.Actor{Handle: p.Handle, Admin: p.Admin, QuotaBytes: p.QuotaBytes, Credential: credential(p)}
}

// credential names the credential behind p for the audit trail: the source
// and the key id, "api_key:<prefix>", "oauth:<id>", or the source alone
// ("admin_token") when there is no key id. A key id that already starts
// with the source is used as it is.
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
	case errors.Is(err, user.ErrInvalidHandle), errors.Is(err, user.ErrInvalidRole), errors.Is(err, user.ErrInvalidScope):
		return http.StatusUnprocessableEntity, api.Error{Code: api.CodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, user.ErrDisabled):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: err.Error()}
	case errors.Is(err, user.ErrNotProvisioned):
		return http.StatusForbidden, api.Error{Code: api.CodeForbidden, Message: err.Error()}
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, api.Error{Code: api.CodeTooLarge, Message: fmt.Sprintf("request body exceeds %d bytes", mbe.Limit)}
	}
	return http.StatusInternalServerError, api.Error{Code: api.CodeInternal, Message: "internal error"}
}

func statusFor(code string) int {
	switch code {
	case api.CodeNotFound, api.CodeVersionNotFound, api.CodeFileNotFound, api.CodeUploadNotFound:
		return http.StatusNotFound
	case api.CodeSlugTaken, api.CodeOffsetMismatch:
		return http.StatusConflict
	case api.CodeVersionConflict:
		return http.StatusPreconditionFailed
	case api.CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case api.CodeTooManyFiles, api.CodeInvalidSlug, api.CodeInvalidPath, api.CodeInvalidMetadata, api.CodeInvalidArgument:
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
	}
	return http.StatusInternalServerError
}
