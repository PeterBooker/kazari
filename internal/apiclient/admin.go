package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/PeterBooker/kazari/internal/api"
)

// These methods need a Client for the admin host with the admin token or an
// admin-role key.

// ListUsers lists every user.
func (c *Client) ListUsers(ctx context.Context) ([]api.User, error) {
	return decodeSlice[api.User](c, ctx, "/users", nil)
}

// CreateUser creates a user.
func (c *Client) CreateUser(ctx context.Context, req api.CreateUserRequest) (*api.User, error) {
	return call[api.User](ctx, c, http.MethodPost, "/users", nil, nil, req)
}

// UpdateUser changes a user's email, role, quota or disabled state.
func (c *Client) UpdateUser(ctx context.Context, handle string, req api.UpdateUserRequest) (*api.User, error) {
	return call[api.User](ctx, c, http.MethodPatch, "/users/"+seg(handle), nil, nil, req)
}

// GetUser returns a user with their identities and usage.
func (c *Client) GetUser(ctx context.Context, handle string) (*api.User, error) {
	return call[api.User](ctx, c, http.MethodGet, "/users/"+seg(handle), nil, nil, nil)
}

// LinkIdentity links an identity provider's (issuer, subject) pair to a
// user, moving it from any other user.
func (c *Client) LinkIdentity(ctx context.Context, handle string, req api.LinkIdentityRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/users/"+seg(handle)+"/identities", nil, nil, req, nil)
}

// UnlinkIdentity removes one of a user's identities.
func (c *Client) UnlinkIdentity(ctx context.Context, handle, issuer, subject string) error {
	req := api.LinkIdentityRequest{Issuer: issuer, Subject: subject}
	return c.doJSON(ctx, http.MethodPost, "/users/"+seg(handle)+"/identities/unlink", nil, nil, req, nil)
}

// TransferAll gives every artifact handle owns to owner.
func (c *Client) TransferAll(ctx context.Context, handle, owner string) (*api.TransferredArtifacts, error) {
	return call[api.TransferredArtifacts](ctx, c, http.MethodPost, "/users/"+seg(handle)+"/transfer", nil, nil, api.TransferRequest{Owner: owner})
}

// Invite creates a user and the link that lets them set a password. The
// link's address is in the result exactly once.
func (c *Client) Invite(ctx context.Context, req api.InviteRequest) (*api.Invited, error) {
	return call[api.Invited](ctx, c, http.MethodPost, "/invites", nil, nil, req)
}

// IssueLink makes a new invite or reset link for a user, ending the one
// before it.
func (c *Client) IssueLink(ctx context.Context, handle string, req api.LinkRequest) (*api.CreatedLink, error) {
	return call[api.CreatedLink](ctx, c, http.MethodPost, "/users/"+seg(handle)+"/links", nil, nil, req)
}

// SetupLink makes the link that creates the first admin.
func (c *Client) SetupLink(ctx context.Context) (*api.CreatedLink, error) {
	return call[api.CreatedLink](ctx, c, http.MethodPost, "/setup-link", nil, nil, nil)
}

// ListLinks lists the links that have not been used, revoked or expired.
func (c *Client) ListLinks(ctx context.Context) ([]api.Link, error) {
	return decodeSlice[api.Link](c, ctx, "/invites", nil)
}

// RevokeLink ends the unused link with id.
func (c *Client) RevokeLink(ctx context.Context, id int64) error {
	return c.doJSON(ctx, http.MethodDelete, "/invites/"+strconv.FormatInt(id, 10), nil, nil, nil, nil)
}

// EndSessions ends every browser session of a user.
func (c *Client) EndSessions(ctx context.Context, handle string) error {
	return c.doJSON(ctx, http.MethodDelete, "/users/"+seg(handle)+"/sessions", nil, nil, nil, nil)
}

// Backup starts a backup and returns the tar stream, which the caller
// closes. certs asks for the certificate directory too, which the server
// grants on the admin socket alone.
func (c *Client) Backup(ctx context.Context, certs bool) (io.ReadCloser, error) {
	var q url.Values
	if certs {
		q = url.Values{"certs": {"true"}}
	}
	resp, err := c.send(ctx, request{method: http.MethodPost, path: "/backup", query: q})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Ready runs the server's readiness checks, anew with fresh, else as last
// run within the server's cache time.
func (c *Client) Ready(ctx context.Context, fresh bool) (*api.Ready, error) {
	var q url.Values
	if fresh {
		q = url.Values{"fresh": {"true"}}
	}
	return call[api.Ready](ctx, c, http.MethodGet, "/ready", q, nil, nil)
}

// ListKeys lists a user's API keys.
func (c *Client) ListKeys(ctx context.Context, handle string) ([]api.APIKey, error) {
	return decodeSlice[api.APIKey](c, ctx, "/users/"+seg(handle)+"/keys", nil)
}

// IssueKey issues a key for a user. The secret is in the result exactly once.
func (c *Client) IssueKey(ctx context.Context, handle string, req api.IssueKeyRequest) (*api.IssuedKey, error) {
	return call[api.IssuedKey](ctx, c, http.MethodPost, "/users/"+seg(handle)+"/keys", nil, nil, req)
}

// RevokeKey revokes the key with prefix.
func (c *Client) RevokeKey(ctx context.Context, handle, prefix string) error {
	return c.doJSON(ctx, http.MethodDelete, "/users/"+seg(handle)+"/keys/"+seg(prefix), nil, nil, nil, nil)
}

// RotateKey replaces a user's key; the old one works until overlap has
// passed ("" is the server's default).
func (c *Client) RotateKey(ctx context.Context, handle, prefix, overlap string) (*api.IssuedKey, error) {
	path := "/users/" + seg(handle) + "/keys/" + seg(prefix) + "/rotate"
	return call[api.IssuedKey](ctx, c, http.MethodPost, path, nil, nil, api.RotateKeyRequest{Overlap: overlap})
}

// ListGrants lists a user's OAuth grants.
func (c *Client) ListGrants(ctx context.Context, handle string) ([]api.OAuthGrant, error) {
	return decodeSlice[api.OAuthGrant](c, ctx, "/users/"+seg(handle)+"/grants", nil)
}

// RevokeGrant revokes the OAuth grant with id and every token of it.
func (c *Client) RevokeGrant(ctx context.Context, id int64) error {
	return c.doJSON(ctx, http.MethodDelete, "/grants/"+strconv.FormatInt(id, 10), nil, nil, nil, nil)
}

// AdminListArtifacts lists every artifact of any owner.
func (c *Client) AdminListArtifacts(ctx context.Context, p ListParams) (*api.ArtifactList, error) {
	return call[api.ArtifactList](ctx, c, http.MethodGet, "/artifacts", p.values(), nil, nil)
}

// Transfer gives the artifact to another owner.
func (c *Client) Transfer(ctx context.Context, slug, owner string) (*api.Artifact, error) {
	return call[api.Artifact](ctx, c, http.MethodPost, "/artifacts/"+seg(slug)+"/transfer", nil, nil, api.TransferRequest{Owner: owner})
}

// Hide unpublishes the artifact in one call (abuse response).
func (c *Client) Hide(ctx context.Context, slug string) (*api.Artifact, error) {
	return call[api.Artifact](ctx, c, http.MethodPost, "/artifacts/"+seg(slug)+"/hide", nil, nil, nil)
}

// Settings returns the effective server configuration, secrets redacted.
func (c *Client) Settings(ctx context.Context) (map[string]any, error) {
	out, err := call[map[string]any](ctx, c, http.MethodGet, "/settings", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return *out, nil
}

// SettingSources names where each setting not at its default came from,
// such as "KAZARI_LISTEN_HTTP in the environment".
func (c *Client) SettingSources(ctx context.Context) (map[string]string, error) {
	out, err := call[map[string]string](ctx, c, http.MethodGet, "/settings/sources", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return *out, nil
}

// Jobs returns the scheduler status.
func (c *Client) Jobs(ctx context.Context) ([]api.JobStatus, error) {
	return decodeSlice[api.JobStatus](c, ctx, "/jobs", nil)
}

// RunJob triggers the named job now.
func (c *Client) RunJob(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/jobs/"+seg(name)+"/run", nil, nil, nil, nil)
}

// ViewSummary returns the instance's page views over the last days days,
// today included.
func (c *Client) ViewSummary(ctx context.Context, days int) (*api.ViewSummary, error) {
	return call[api.ViewSummary](ctx, c, http.MethodGet, "/views", viewQuery(days), nil, nil)
}

// ArtifactViews returns one artifact's page views over the last days days.
func (c *Client) ArtifactViews(ctx context.Context, slug string, days int) (*api.ArtifactViews, error) {
	return call[api.ArtifactViews](ctx, c, http.MethodGet, "/artifacts/"+seg(slug)+"/views", viewQuery(days), nil, nil)
}

func viewQuery(days int) url.Values {
	q := url.Values{}
	if days > 0 {
		q.Set("days", strconv.Itoa(days))
	}
	return q
}

// AuditParams filters GET /audit. Zero fields are omitted.
type AuditParams struct {
	Since, Until string // RFC 3339; Since is inclusive, Until is not
	Slug         string
	Actor        string
	Subject      string
	// Actions keeps the events with one of these actions; one ending in "."
	// is a prefix, such as "artifact.".
	Actions []string
	Cursor  string
	Limit   int
}

func (p AuditParams) values() url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("since", p.Since)
	set("until", p.Until)
	set("slug", p.Slug)
	set("actor", p.Actor)
	set("subject", p.Subject)
	for _, a := range p.Actions {
		q.Add("action", a)
	}
	set("cursor", p.Cursor)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// Audit returns one page of the audit log, newest first.
func (c *Client) Audit(ctx context.Context, p AuditParams) (*api.AuditList, error) {
	return call[api.AuditList](ctx, c, http.MethodGet, "/audit", p.values(), nil, nil)
}

// Health reports liveness and the server version.
func (c *Client) Health(ctx context.Context) (*api.Health, error) {
	return call[api.Health](ctx, c, http.MethodGet, "/health", nil, nil, nil)
}
