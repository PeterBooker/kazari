// Package api holds the wire types shared by the REST API, the MCP server
// and the CLI: request and response structs and the error codes. It has no
// logic and imports nothing outside the standard library.
package api

import "time"

// Error codes. The HTTP layer maps them to statuses in one place.
const (
	CodeNotFound         = "not_found"
	CodeVersionNotFound  = "version_not_found"
	CodeFileNotFound     = "file_not_found"
	CodeUploadNotFound   = "upload_not_found"
	CodeSlugTaken        = "slug_taken"
	CodeVersionConflict  = "version_conflict"
	CodeTooLarge         = "too_large"
	CodeTooManyFiles     = "too_many_files"
	CodeTooManyUploads   = "too_many_uploads"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeInvalidSlug      = "invalid_slug"
	CodeInvalidPath      = "invalid_path"
	CodeInvalidMetadata  = "invalid_metadata"
	CodeInvalidArgument  = "invalid_argument"
	CodeForbidden        = "forbidden"
	CodeUnauthorized     = "unauthorized"
	CodeGone             = "gone"
	CodeOffsetMismatch   = "upload_offset_mismatch"
	CodeRateLimited      = "rate_limited"
	CodeHandleTaken      = "handle_taken"
	CodeEmailTaken       = "email_taken"
	CodeInternal         = "internal"
	CodeMethodNotAllowed = "method_not_allowed"
)

// Error is the body of every error response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Current is set on upload_offset_mismatch: the staged size to resume from.
	Current *int64 `json:"current,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrorResponse wraps Error as {"error": {...}}.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// Artifact is the public record.
type Artifact struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Owner            string            `json:"owner"`
	Visibility       string            `json:"visibility"`
	Metadata         map[string]string `json:"metadata"`
	URL              string            `json:"url"`
	PublishedVersion int               `json:"published_version"`
	LatestVersion    int               `json:"latest_version"`
	SizeBytes        int64             `json:"size_bytes"`
	StaleAt          *time.Time        `json:"stale_at,omitempty"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
	UnpublishedAt    *time.Time        `json:"unpublished_at,omitempty"`
	DeletedAt        *time.Time        `json:"deleted_at,omitempty"`
	PurgedAt         *time.Time        `json:"purged_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// Version is one immutable snapshot.
type Version struct {
	Number    int       `json:"number"`
	Label     string    `json:"label,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	URL       string    `json:"url"`
	Files     []File    `json:"files,omitempty"`
}

// File is one entry of a version.
type File struct {
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// ArtifactDetail is GET /artifacts/{slug}.
type ArtifactDetail struct {
	Artifact Artifact  `json:"artifact"`
	Versions []Version `json:"versions"`
}

// PublishResult is returned by create, update and publish.
type PublishResult struct {
	URL      string   `json:"url"`
	Version  int      `json:"version"`
	Artifact Artifact `json:"artifact"`
}

// ArtifactList is one page of GET /artifacts.
type ArtifactList struct {
	Items      []Artifact `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// FileInput is an inline file in a JSON create or update body.
type FileInput struct {
	Path        string `json:"path"`
	Content     string `json:"content,omitempty"`
	Encoding    string `json:"encoding,omitempty"` // "utf8" (default) | "base64"
	ContentType string `json:"content_type,omitempty"`
	Delete      bool   `json:"delete,omitempty"`
}

// CreateArtifactRequest is the JSON body of POST /artifacts and the input of
// the create_artifact tool.
type CreateArtifactRequest struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Label       string            `json:"label,omitempty"`
	Files       []FileInput       `json:"files,omitempty"`
	UploadID    string            `json:"upload_id,omitempty"`
}

// UpdateArtifactRequest is the JSON body of PUT /artifacts/{slug}, the
// manifest part of a multipart update, and the input of update_artifact.
type UpdateArtifactRequest struct {
	Name            *string            `json:"name,omitempty"`
	Description     *string            `json:"description,omitempty"`
	Metadata        map[string]*string `json:"metadata,omitempty"` // null deletes
	Visibility      *string            `json:"visibility,omitempty"`
	Label           string             `json:"label,omitempty"`
	ExpectedVersion int                `json:"expected_version,omitempty"`
	KeepStale       bool               `json:"keep_stale,omitempty"`
	Restore         bool               `json:"restore,omitempty"`
	Files           []FileInput        `json:"files,omitempty"`
	UploadID        string             `json:"upload_id,omitempty"`
}

// CreateManifest is the manifest part of a multipart create.
type CreateManifest struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Label       string            `json:"label,omitempty"`
}

// PublishRequest moves the published pointer.
type PublishRequest struct {
	Version int `json:"version"`
}

// StaleRequest marks an artifact stale; Grace is a Go duration string.
type StaleRequest struct {
	Grace string `json:"grace,omitempty"`
}

// StaleResult is the response of mark_stale.
type StaleResult struct {
	StaleAt   time.Time `json:"stale_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Artifact  Artifact  `json:"artifact"`
}

// VisibilityRequest sets visibility (public, unlisted, unpublished).
type VisibilityRequest struct {
	Visibility string `json:"visibility"`
}

// OK is an empty success body.
type OK struct {
	OK bool `json:"ok"`
}

// Upload describes a chunked upload.
type Upload struct {
	UploadID      string    `json:"upload_id"`
	ExpiresAt     time.Time `json:"expires_at"`
	MaxChunkBytes int64     `json:"max_chunk_bytes"`
	MaxTotalBytes int64     `json:"max_total_bytes"`
}

// ChunkResult acknowledges one chunk.
type ChunkResult struct {
	Path          string `json:"path"`
	BytesReceived int64  `json:"bytes_received"`
}

// Me is GET /me.
type Me struct {
	Handle     string   `json:"handle"`
	Admin      bool     `json:"admin"`
	Scopes     []string `json:"scopes"`
	Source     string   `json:"source"`
	QuotaBytes int64    `json:"quota_bytes"`
	UsedBytes  int64    `json:"used_bytes"`
}

// User is an instance member.
type User struct {
	Handle      string     `json:"handle"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	Role        string     `json:"role"`
	QuotaBytes  int64      `json:"quota_bytes"`
	UsedBytes   int64      `json:"used_bytes,omitempty"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Identities  []Identity `json:"identities,omitempty"`
}

// Identity is an upstream identity linked to a user.
type Identity struct {
	Issuer      string    `json:"issuer"`
	Subject     string    `json:"subject"`
	Email       string    `json:"email,omitempty"`
	LastLoginAt time.Time `json:"last_login_at"`
}

// CreateUserRequest is POST /users on the admin API.
type CreateUserRequest struct {
	Handle      string `json:"handle"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"` // member (default) | admin
	QuotaBytes  *int64 `json:"quota_bytes,omitempty"`
}

// UpdateUserRequest is PATCH /users/{handle}.
type UpdateUserRequest struct {
	Email       *string `json:"email,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Role        *string `json:"role,omitempty"`
	QuotaBytes  *int64  `json:"quota_bytes,omitempty"`
	Disabled    *bool   `json:"disabled,omitempty"`
}

// LinkIdentityRequest is POST /users/{handle}/identities.
type LinkIdentityRequest struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
}

// APIKey is a key as listed (never the secret).
type APIKey struct {
	Prefix     string     `json:"prefix"`
	Label      string     `json:"label,omitempty"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IssueKeyRequest is POST /users/{handle}/keys.
type IssueKeyRequest struct {
	Label     string   `json:"label,omitempty"`
	Scopes    []string `json:"scopes,omitempty"` // default: read, write, delete
	ExpiresIn string   `json:"expires_in,omitempty"`
}

// IssuedKey carries the secret exactly once.
type IssuedKey struct {
	Key    string `json:"key"`
	APIKey APIKey `json:"api_key"`
}

// TransferRequest is POST /artifacts/{slug}/transfer.
type TransferRequest struct {
	Owner string `json:"owner"`
}

// JobStatus is one scheduler job.
type JobStatus struct {
	Name      string     `json:"name"`
	Every     string     `json:"every"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	NextRun   *time.Time `json:"next_run,omitempty"`
	Running   bool       `json:"running"`
}

// AuditEvent is one audit row.
type AuditEvent struct {
	At      time.Time         `json:"at"`
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Slug    string            `json:"slug,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// OAuthGrant is a refresh token issued to a client for a user.
type OAuthGrant struct {
	ID        int64      `json:"id"`
	ClientID  string     `json:"client_id"`
	Client    string     `json:"client,omitempty"`
	User      string     `json:"user"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Health is GET /health.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"` // admin host only
}
