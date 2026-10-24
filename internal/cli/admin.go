package cli

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"kazari/internal/api"
)

// The methods in this file call the admin API; use a Client whose base URL
// is the admin host and whose token is the admin token or an admin-role key.

// ListUsers lists every user.
func (c *Client) ListUsers(ctx context.Context) ([]api.User, error) {
	return decodeSlice[api.User](c, ctx, "/users", nil)
}

// CreateUser creates a user.
func (c *Client) CreateUser(ctx context.Context, req api.CreateUserRequest) (*api.User, error) {
	var out api.User
	if err := c.doJSON(ctx, http.MethodPost, "/users", nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateUser changes a user's email, role, quota or disabled state.
func (c *Client) UpdateUser(ctx context.Context, handle string, req api.UpdateUserRequest) (*api.User, error) {
	var out api.User
	if err := c.doJSON(ctx, http.MethodPatch, "/users/"+seg(handle), nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListKeys lists a user's API keys.
func (c *Client) ListKeys(ctx context.Context, handle string) ([]api.APIKey, error) {
	return decodeSlice[api.APIKey](c, ctx, "/users/"+seg(handle)+"/keys", nil)
}

// IssueKey issues a key for a user. The secret is in the result exactly once.
func (c *Client) IssueKey(ctx context.Context, handle string, req api.IssueKeyRequest) (*api.IssuedKey, error) {
	var out api.IssuedKey
	if err := c.doJSON(ctx, http.MethodPost, "/users/"+seg(handle)+"/keys", nil, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeKey revokes the key with prefix.
func (c *Client) RevokeKey(ctx context.Context, handle, prefix string) error {
	return c.doJSON(ctx, http.MethodDelete, "/users/"+seg(handle)+"/keys/"+seg(prefix), nil, nil, nil, nil)
}

// AdminListArtifacts lists every artifact of any owner.
func (c *Client) AdminListArtifacts(ctx context.Context, p ListParams) (*api.ArtifactList, error) {
	var out api.ArtifactList
	if err := c.doJSON(ctx, http.MethodGet, "/artifacts", p.values(), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Transfer gives the artifact to another owner.
func (c *Client) Transfer(ctx context.Context, slug, owner string) (*api.Artifact, error) {
	var out api.Artifact
	err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/transfer", nil, nil, api.TransferRequest{Owner: owner}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Hide unpublishes the artifact in one call (abuse response).
func (c *Client) Hide(ctx context.Context, slug string) (*api.Artifact, error) {
	var out api.Artifact
	if err := c.doJSON(ctx, http.MethodPost, "/artifacts/"+seg(slug)+"/hide", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Settings returns the effective server configuration, secrets redacted.
func (c *Client) Settings(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/settings", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Jobs returns the scheduler status.
func (c *Client) Jobs(ctx context.Context) ([]api.JobStatus, error) {
	return decodeSlice[api.JobStatus](c, ctx, "/jobs", nil)
}

// RunJob triggers the named job now.
func (c *Client) RunJob(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/jobs/"+seg(name)+"/run", nil, nil, nil, nil)
}

// Audit queries the audit log. since is RFC 3339; empty strings and a zero
// limit are omitted.
func (c *Client) Audit(ctx context.Context, since, slug string, limit int) ([]api.AuditEvent, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if slug != "" {
		q.Set("slug", slug)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return decodeSlice[api.AuditEvent](c, ctx, "/audit", q)
}

// Health reports liveness and the server version.
func (c *Client) Health(ctx context.Context) (*api.Health, error) {
	var out api.Health
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
