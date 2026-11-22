// Package api holds the wire types and error codes shared by the REST API, the
// MCP server and the CLI. It has no logic and imports only the standard library.
package api

import "time"

const (
	CodeNotFound           = "not_found"
	CodeVersionNotFound    = "version_not_found"
	CodeFileNotFound       = "file_not_found"
	CodeUploadNotFound     = "upload_not_found"
	CodeSlugTaken          = "slug_taken"
	CodeVersionConflict    = "version_conflict"
	CodePublishedNotLatest = "published_not_latest"
	CodeTooLarge           = "too_large"
	CodeTooManyFiles       = "too_many_files"
	CodeTooManyUploads     = "too_many_uploads"
	CodeQuotaExceeded      = "quota_exceeded"
	CodeInvalidSlug        = "invalid_slug"
	CodeInvalidPath        = "invalid_path"
	CodeInvalidMetadata    = "invalid_metadata"
	CodeInvalidArgument    = "invalid_argument"
	CodeForbidden          = "forbidden"
	CodeUnauthorized       = "unauthorized"
	CodeGone               = "gone"
	CodeOffsetMismatch     = "upload_offset_mismatch"
	CodeDigestMismatch     = "upload_digest_mismatch"
	CodeRateLimited        = "rate_limited"
	CodeHandleTaken        = "handle_taken"
	CodeEmailTaken         = "email_taken"
	CodeInternal           = "internal"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeEditNotFound       = "edit_not_found"
	CodeEditAmbiguous      = "edit_ambiguous"
	CodeAnchorMismatch     = "anchor_mismatch"
	CodeLinkNotFound       = "share_link_not_found"
	CodeFetchFailed        = "fetch_failed"
	CodeJobRunning         = "job_running"
	CodeLastAdmin          = "last_admin"
	CodeTooManyKeys        = "too_many_keys"
	CodeSetupDone          = "setup_done"
	CodeBackupRunning      = "backup_running"
	CodeNoSpace            = "no_space"
	CodeBusy               = "busy"
)

// ErrorCodes is every code above, the bounded set metrics count errors by.
var ErrorCodes = []string{
	CodeNotFound, CodeVersionNotFound, CodeFileNotFound, CodeUploadNotFound, CodeSlugTaken,
	CodeVersionConflict, CodePublishedNotLatest, CodeTooLarge, CodeTooManyFiles, CodeTooManyUploads, CodeQuotaExceeded,
	CodeInvalidSlug, CodeInvalidPath, CodeInvalidMetadata, CodeInvalidArgument, CodeForbidden,
	CodeUnauthorized, CodeGone, CodeOffsetMismatch, CodeDigestMismatch, CodeRateLimited, CodeHandleTaken,
	CodeEmailTaken, CodeInternal, CodeMethodNotAllowed, CodeEditNotFound, CodeEditAmbiguous,
	CodeAnchorMismatch, CodeLinkNotFound, CodeFetchFailed, CodeJobRunning, CodeLastAdmin,
	CodeTooManyKeys, CodeSetupDone, CodeBackupRunning, CodeNoSpace, CodeBusy,
}

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

// Version is one immutable snapshot. Staged means it was added without
// being published and has not been published since.
type Version struct {
	Number    int       `json:"number"`
	Label     string    `json:"label,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	Staged    bool      `json:"staged,omitempty"`
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
	// NextFilesBefore, with file lists, is the files_before of the next page
	// of them: versions below it have no Files yet. Absent on the last page.
	NextFilesBefore int `json:"next_files_before,omitempty"`
}

// PublishResult is returned by create, update and publish. VersionURL pins
// Version. Unchanged means the files matched the latest version and no
// version was added; Staged that Version is not published, so URL still
// serves Artifact.PublishedVersion; Warnings say what will stop the version
// rendering as published.
type PublishResult struct {
	URL        string        `json:"url"`
	Version    int           `json:"version"`
	VersionURL string        `json:"version_url"`
	Unchanged  bool          `json:"unchanged,omitempty"`
	Staged     bool          `json:"staged,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
	Artifact   Artifact      `json:"artifact"`
	Import     *ImportReport `json:"import,omitempty"`
}

// ImportReport says what an import fetched, left out and found.
type ImportReport struct {
	Source         string         `json:"source"`
	Fetched        int            `json:"fetched"`
	Skipped        []SkippedFetch `json:"skipped"`
	ScriptsRemoved int            `json:"scripts_removed"`
	Notes          []string       `json:"notes"`
	Outline        []Landmark     `json:"outline"`
}

// SkippedFetch is a resource an import left remote, with the reason.
type SkippedFetch struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// Landmark is a section of an imported page: the line its opening tag is
// on, that tag as text, and the first heading inside it.
type Landmark struct {
	Line    int    `json:"line"`
	Tag     string `json:"tag"`
	Heading string `json:"heading,omitempty"`
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
	// URL names content the server downloads instead of Content.
	URL    string `json:"url,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// CreateArtifactRequest is the body of POST /artifacts and of create_artifact.
type CreateArtifactRequest struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Visibility  string            `json:"visibility,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Label       string            `json:"label,omitempty"`
	Files       []FileInput       `json:"files,omitempty"`
	UploadID    string            `json:"upload_id,omitempty"`
	// SourceURL imports a web page as the artifact; Files are added to it.
	SourceURL   string `json:"source_url,omitempty"`
	KeepScripts bool   `json:"keep_scripts,omitempty"`
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
	BaseVersion     int                `json:"base_version,omitempty"`
	KeepStale       bool               `json:"keep_stale,omitempty"`
	Restore         bool               `json:"restore,omitempty"`
	// Stage adds the version without publishing it.
	Stage    bool        `json:"stage,omitempty"`
	Files    []FileInput `json:"files,omitempty"`
	UploadID string      `json:"upload_id,omitempty"`
	// Mirror deletes the base version's files that the request does not
	// carry, so the new version holds exactly what was sent. REST only.
	Mirror bool `json:"mirror,omitempty"`
}

// EditRequest is the JSON body of POST /artifacts/{slug}/edit: edits to one
// text file of the latest version, published as a new version unless Stage.
type EditRequest struct {
	Path            string `json:"path"`
	Edits           []Edit `json:"edits"`
	Label           string `json:"label,omitempty"`
	ExpectedVersion int    `json:"expected_version,omitempty"`
	BaseVersion     int    `json:"base_version,omitempty"`
	KeepStale       bool   `json:"keep_stale,omitempty"`
	Stage           bool   `json:"stage,omitempty"`
}

// Edit replaces old_text, or the lines from anchor start to end, with
// new_text, or inserts new_text as lines after or before an anchored line.
type Edit struct {
	OldText    string `json:"old_text,omitempty"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
	Start      string `json:"start,omitempty"`
	End        string `json:"end,omitempty"`
	After      string `json:"after,omitempty"`
	Before     string `json:"before,omitempty"`
}

// EditResult is the response of an edit.
type EditResult struct {
	URL        string        `json:"url"`
	Version    int           `json:"version"`
	VersionURL string        `json:"version_url"`
	Staged     bool          `json:"staged,omitempty"`
	Path       string        `json:"path"`
	Edits      []EditOutcome `json:"edits"`
	Warnings   []string      `json:"warnings,omitempty"`
	Artifact   Artifact      `json:"artifact"`
}

// EditOutcome reports one edit: Match is exact, trailing_whitespace, indentation
// or anchor; StartLine and EndLine are where the new text sits in the new version.
// ExcerptOf names the edit whose excerpt also shows this one's lines.
type EditOutcome struct {
	Match        string `json:"match"`
	Replacements int    `json:"replacements"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	Excerpt      string `json:"excerpt"`
	ExcerptOf    int    `json:"excerpt_of,omitempty"`
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

// PruneRequest is the body of POST /artifacts/{slug}/versions/prune: keep
// the Keep newest versions, the published one and the staged ones newer
// than it, and delete the rest.
type PruneRequest struct {
	Keep   int  `json:"keep"`
	DryRun bool `json:"dry_run,omitempty"`
}

// DeletedVersions answers a version deletion or a prune: the numbers
// deleted, oldest first, or with DryRun the ones a prune would delete.
type DeletedVersions struct {
	Deleted  []int    `json:"deleted"`
	DryRun   bool     `json:"dry_run,omitempty"`
	Artifact Artifact `json:"artifact"`
}

// VisibilityRequest sets visibility (public, unlisted, members, unpublished).
type VisibilityRequest struct {
	Visibility string `json:"visibility"`
}

// ShareLink is a share link of a members artifact, without its token.
// Prefix names it for revoking.
type ShareLink struct {
	Prefix     string     `json:"prefix"`
	Label      string     `json:"label,omitempty"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Uses       int        `json:"uses"`
	Active     bool       `json:"active"`
}

// ShareLinkRequest is POST /artifacts/{slug}/share-links. ExpiresIn is a
// duration such as 72h or 7d; empty means 7 days.
type ShareLinkRequest struct {
	Label     string `json:"label,omitempty"`
	ExpiresIn string `json:"expires_in,omitempty"`
}

// CreatedShareLink carries the link's URL, which holds its token, exactly
// once.
type CreatedShareLink struct {
	URL       string    `json:"url"`
	ShareLink ShareLink `json:"share_link"`
	Artifact  Artifact  `json:"artifact"`
}

// ShareLinkList is GET /artifacts/{slug}/share-links, newest first.
type ShareLinkList struct {
	Items []ShareLink `json:"items"`
}

// OK is an empty success body.
type OK struct {
	OK bool `json:"ok"`
}

// BeginUploadRequest is the optional body of POST /uploads. Both fields
// together ask for an archive URL; the response then carries archive_url.
type BeginUploadRequest struct {
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	ArchiveBytes  int64  `json:"archive_bytes,omitempty"`
	// ArchivePath sends one file instead of an archive, staged at this path.
	ArchivePath string `json:"archive_path,omitempty"`
}

// Upload describes an upload. ArchiveURL is set when the upload was begun
// with a digest: a PUT of the archive to it needs no credential.
type Upload struct {
	UploadID      string    `json:"upload_id"`
	ExpiresAt     time.Time `json:"expires_at"`
	MaxChunkBytes int64     `json:"max_chunk_bytes"`
	MaxTotalBytes int64     `json:"max_total_bytes"`
	ArchiveURL    string    `json:"archive_url,omitempty"`
}

// ArchiveResult acknowledges an archive received at an upload's archive URL.
type ArchiveResult struct {
	UploadID string `json:"upload_id"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
}

// ChunkResult acknowledges one chunk.
type ChunkResult struct {
	Path          string `json:"path"`
	BytesReceived int64  `json:"bytes_received"`
}

// Me is GET /me. Slugs is the credential's restriction: the slug patterns it
// may see and change, absent when it has none. ExpiresAt is when the
// credential ends whatever is done with it: an API key's expiry, or that of
// the key an OAuth connection was approved with.
type Me struct {
	Handle     string     `json:"handle"`
	Admin      bool       `json:"admin"`
	Role       string     `json:"role,omitempty"`
	Scopes     []string   `json:"scopes"`
	Source     string     `json:"source"`
	Slugs      []string   `json:"slugs,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	QuotaBytes int64      `json:"quota_bytes"`
	UsedBytes  int64      `json:"used_bytes"`
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
	// How the person signs in, on the admin API: whether they have set a
	// password, whether an invite is waiting for them to, their unused
	// recovery codes, their passkeys, their live keys and the latest of their sign-ins and
	// key uses.
	Password      bool       `json:"password,omitempty"`
	Invited       bool       `json:"invited,omitempty"`
	RecoveryCodes int        `json:"recovery_codes,omitempty"`
	Passkeys      int        `json:"passkeys,omitempty"`
	Keys          int        `json:"keys,omitempty"`
	LastActiveAt  *time.Time `json:"last_active_at,omitempty"`
	// Sessions counts the person's browser sessions, on GET /users/{handle}.
	Sessions *int `json:"sessions,omitempty"`
}

// Identity is an upstream identity linked to a user.
type Identity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	// LastLoginAt is absent for an identity an admin linked that has not
	// signed in yet.
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// CreateUserRequest is POST /users on the admin API.
type CreateUserRequest struct {
	Handle      string `json:"handle"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"` // member (default) | admin
	QuotaBytes  *int64 `json:"quota_bytes,omitempty"`
}

// InviteRequest is POST /invites on the admin API: a new user, and the
// link that lets them set a password.
type InviteRequest struct {
	Handle      string `json:"handle"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"` // member (default) | admin
	QuotaBytes  *int64 `json:"quota_bytes,omitempty"`
}

// LinkRequest is POST /users/{handle}/links: a new invite or reset link for
// an existing user, ending the one before it.
type LinkRequest struct {
	Purpose string `json:"purpose"` // invite | reset
	// RevokeAll makes using a reset link also revoke the user's API keys and
	// OAuth grants.
	RevokeAll bool `json:"revoke_all,omitempty"`
}

// Link is a single-use link that sets a password. User is empty for the
// setup link, which creates the first admin.
type Link struct {
	ID        int64     `json:"id"`
	User      string    `json:"user,omitempty"`
	Purpose   string    `json:"purpose"` // invite | reset | setup
	RevokeAll bool      `json:"revoke_all,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreatedLink is a new link with its address, which holds the token and is
// never available again.
type CreatedLink struct {
	Link
	URL string `json:"url"`
}

// Invited is the result of POST /invites.
type Invited struct {
	User User        `json:"user"`
	Link CreatedLink `json:"link"`
}

// Ready is GET /ready on the admin API: the checks of a running server.
type Ready struct {
	Ready     bool      `json:"ready"` // no check failed
	CheckedAt time.Time `json:"checked_at"`
	Checks    []Check   `json:"checks"`
	// Update is absent until the update check has got an answer, and on an
	// instance that does not run it.
	Update *Update `json:"update,omitempty"`
}

// Update is the last answer of the daily update check.
type Update struct {
	Latest    string    `json:"latest"`
	Newer     bool      `json:"newer"` // than the running version
	CheckedAt time.Time `json:"checked_at"`
}

// Check is one readiness finding. Detail ends with what to do next when
// Status is warn or fail.
type Check struct {
	Check  string `json:"check"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
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

// APIKey is a key as listed (never the secret). Slugs is its restriction,
// absent when it has none; IssuedBy the prefix of the key it was issued
// with, absent when an admin issued it.
type APIKey struct {
	Prefix     string     `json:"prefix"`
	Label      string     `json:"label,omitempty"`
	Scopes     []string   `json:"scopes"`
	Slugs      []string   `json:"slugs,omitempty"`
	IssuedBy   string     `json:"issued_by,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IssueKeyRequest is POST /users/{handle}/keys and POST /me/keys. Slugs are
// slug patterns, an exact slug or a prefix ending in "*"; ExpiresIn is a
// duration such as 720h or 90d.
type IssueKeyRequest struct {
	Label     string   `json:"label,omitempty"`
	Scopes    []string `json:"scopes,omitempty"` // default: read, write, share, delete
	Slugs     []string `json:"slugs,omitempty"`
	ExpiresIn string   `json:"expires_in,omitempty"`
}

// IssuedKey carries the secret exactly once. Replaces is set by a rotation:
// the key the new one replaces, with the expiry the rotation gave it.
type IssuedKey struct {
	Key      string  `json:"key"`
	APIKey   APIKey  `json:"api_key"`
	Replaces *APIKey `json:"replaces,omitempty"`
}

// RotateKeyRequest is the optional body of the two rotate routes. Overlap is
// how long the old key keeps working, such as 24h (the default), 7d (the
// most) or 0.
type RotateKeyRequest struct {
	Overlap string `json:"overlap,omitempty"`
}

// TransferRequest is POST /artifacts/{slug}/transfer.
type TransferRequest struct {
	Owner string `json:"owner"`
}

// TransferredArtifacts answers POST /users/{handle}/transfer: the slugs that
// changed owner.
type TransferredArtifacts struct {
	Transferred []string `json:"transferred"`
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

// ViewTotals are page views over a range of days. Visitors is the sum of
// each day's visitors: one person on three days counts three times.
type ViewTotals struct {
	Views       int64 `json:"views"`
	Visitors    int64 `json:"visitors"`
	MemberViews int64 `json:"member_views"`
	LinkViews   int64 `json:"link_views"`
}

// ViewDay is one UTC day's views; Day is YYYY-MM-DD.
type ViewDay struct {
	Day string `json:"day"`
	ViewTotals
}

// ArtifactViewTotals are one artifact's views over a range.
type ArtifactViewTotals struct {
	Slug string `json:"slug"`
	ViewTotals
}

// ViewSummary is GET /views: the instance's views from From to To, both
// YYYY-MM-DD and included, a day each in Days, the most viewed artifacts in
// Top.
type ViewSummary struct {
	From   string               `json:"from"`
	To     string               `json:"to"`
	Days   []ViewDay            `json:"days"`
	Totals ViewTotals           `json:"totals"`
	Top    []ArtifactViewTotals `json:"top"`
}

// ViewFile is the views of one file of an artifact.
type ViewFile struct {
	Path  string `json:"path"`
	Views int64  `json:"views"`
}

// ViewReader is a member who opened a members page.
type ViewReader struct {
	Handle  string    `json:"handle"`
	Name    string    `json:"name,omitempty"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
	Views   int64     `json:"views"`
}

// ArtifactViews is GET /artifacts/{slug}/views. Readers are the latest
// first and not bound to the range.
type ArtifactViews struct {
	Slug    string       `json:"slug"`
	From    string       `json:"from"`
	To      string       `json:"to"`
	Days    []ViewDay    `json:"days"`
	Totals  ViewTotals   `json:"totals"`
	Files   []ViewFile   `json:"files"`
	Readers []ViewReader `json:"readers"`
}

// AuditList is one page of GET /audit, newest first.
type AuditList struct {
	Events     []AuditEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// AuditEvent is one audit row.
type AuditEvent struct {
	At      time.Time         `json:"at"`
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Slug    string            `json:"slug,omitempty"`
	Subject string            `json:"subject,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// OAuthGrant is a client's connection for a user. ID is the number in the
// "oauth:<id>" credential of the audit log and stays the same across
// refreshes. CreatedAt is the last refresh, ExpiresAt when the grant lapses
// unless it is refreshed again, and KeyPrefix the API key it was approved
// with, whose scopes, restriction and expiry it shares.
type OAuthGrant struct {
	ID        int64      `json:"id"`
	ClientID  string     `json:"client_id"`
	Client    string     `json:"client,omitempty"`
	User      string     `json:"user"`
	Scopes    []string   `json:"scopes"`
	KeyPrefix string     `json:"key_prefix,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Health is GET /health.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"` // admin host only
}
