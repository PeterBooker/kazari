package mcpserver

import (
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
)

// WriteResult is the structured result of the tools that publish or change an
// artifact. It is the REST API's PublishResult without the full record, whose
// description, metadata and timestamps the model sent or rarely needs:
// get_artifact returns them.
type WriteResult struct {
	URL              string     `json:"url"`
	Version          int        `json:"version"`
	Slug             string     `json:"slug"`
	Name             string     `json:"name"`
	Visibility       string     `json:"visibility"`
	PublishedVersion int        `json:"published_version"`
	LatestVersion    int        `json:"latest_version"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	UnpublishedAt    *time.Time `json:"unpublished_at,omitempty"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	// Note repeats what the summary line warns of, for clients that show the
	// model only the structured result.
	Note   string            `json:"note,omitempty"`
	Import *api.ImportReport `json:"import,omitempty"`

	description string
}

// resultMeta is a result with _meta for the preview, which hosts show the
// user but not the model.
type resultMeta interface{ meta() mcp.Meta }

func (r WriteResult) meta() mcp.Meta {
	if r.description == "" {
		return nil
	}
	return mcp.Meta{"kazari": map[string]any{"description": r.description}}
}

// EditFileResult is the structured result of edit_file.
type EditFileResult struct {
	WriteResult
	Path  string            `json:"path"`
	Edits []api.EditOutcome `json:"edits"`
}

func writeResult(r api.PublishResult) WriteResult {
	a := r.Artifact
	out := WriteResult{
		URL:              r.URL,
		Version:          r.Version,
		Slug:             a.Slug,
		Name:             a.Name,
		Visibility:       a.Visibility,
		PublishedVersion: a.PublishedVersion,
		LatestVersion:    a.LatestVersion,
		ExpiresAt:        a.ExpiresAt,
		UnpublishedAt:    a.UnpublishedAt,
		DeletedAt:        a.DeletedAt,
		Import:           r.Import,
		description:      a.Description,
	}
	switch {
	case a.DeletedAt != nil:
		out.Note = "The artifact is deleted, so the URL is offline; update_artifact with restore: true brings it back."
	case a.UnpublishedAt != nil:
		out.Note = "Unpublished: the URL is offline until set_visibility public, unlisted or members."
	}
	return out
}

func editFileResult(r api.EditResult) EditFileResult {
	return EditFileResult{
		WriteResult: writeResult(api.PublishResult{URL: r.URL, Version: r.Version, Artifact: r.Artifact}),
		Path:        r.Path,
		Edits:       r.Edits,
	}
}
