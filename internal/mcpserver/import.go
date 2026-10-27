package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
)

// ImportInput is the input of import_page.
type ImportInput struct {
	URL         string            `json:"url" jsonschema:"http or https address of the page to copy"`
	Slug        string            `json:"slug" jsonschema:"permanent URL name: 3-63 of a-z 0-9 and -, no leading/trailing hyphen, no --"`
	Name        string            `json:"name,omitempty" jsonschema:"human-readable title; default the page's title"`
	Description string            `json:"description,omitempty" jsonschema:"one or two sentences on what the page is; default the page's meta description"`
	Visibility  string            `json:"visibility,omitempty" jsonschema:"public, unlisted or members (only people signed in to this instance, or with a share link); omitted means the instance default"`
	Metadata    map[string]string `json:"metadata,omitempty" jsonschema:"string labels; kazari.spa, kazari.indexable and kazari.csp are interpreted by the server"`
	Label       string            `json:"label,omitempty" jsonschema:"optional note on this version"`
	KeepScripts bool              `json:"keep_scripts,omitempty" jsonschema:"keep the page's scripts and event handlers; by default they are removed"`
}

func (b *builder) addImport(s *mcp.Server, p auth.Principal, actor artifact.Actor) {
	a := annotations("Import page", false, false, false)
	a.OpenWorldHint = boolPtr(true)
	register(b, s, p, &mcp.Tool{Name: "import_page", Annotations: a, Meta: previewToolMeta()},
		auth.ScopeWrite, func(ctx context.Context, _ *mcp.CallToolRequest, in ImportInput) (api.PublishResult, string, error) {
			art, v, rep, err := b.d.Artifacts.Import(ctx, actor, artifact.ImportRequest{
				URL:         in.URL,
				Slug:        in.Slug,
				Name:        in.Name,
				Description: in.Description,
				Visibility:  artifact.Visibility(in.Visibility),
				Metadata:    in.Metadata,
				Label:       in.Label,
				KeepScripts: in.KeepScripts,
			})
			if err != nil {
				return api.PublishResult{}, "", err
			}
			res := b.d.Links.Imported(art, v.Number, rep)
			return res, importedLine(res), nil
		})
}

func importedLine(r api.PublishResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s from %s: %d files", publishedLine(r), r.Import.Source, r.Import.Fetched+1)
	if n := len(r.Import.Skipped); n > 0 {
		fmt.Fprintf(&b, ", %d resources left remote", n)
	}
	if r.Import.ScriptsRemoved > 0 {
		fmt.Fprintf(&b, ", %d scripts removed", r.Import.ScriptsRemoved)
	}
	b.WriteString(".")
	for _, n := range r.Import.Notes {
		b.WriteString(" Note: " + n + ".")
	}
	return b.String()
}
