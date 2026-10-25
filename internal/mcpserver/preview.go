package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The preview is an MCP App (the MCP Apps extension, SEP-1865): hosts that
// support it render the results of the publishing tools as a card with the
// page's links and, when the CSP lets the page be framed, the page itself.
// Other hosts ignore it and show the text result.
const (
	previewURI    = "ui://kazari/preview.html"
	appMIME       = "text/html;profile=mcp-app"
	appsExtension = "io.modelcontextprotocol/ui"
)

//go:embed preview.html
var previewHTML string

// previewConfig is handed to the preview page as JSON.
type previewConfig struct {
	// Embed is true when the instance CSP lets other sites frame pages.
	Embed bool `json:"embed"`
	// VersionURL is a pinned version's URL with {slug} and {version} to
	// fill in.
	VersionURL string `json:"versionURL"`
}

// previewToolMeta links a tool's results to the preview.
func previewToolMeta() mcp.Meta {
	return mcp.Meta{"ui": map[string]any{"resourceUri": previewURI}}
}

// newPreview renders the preview page and the _meta.ui of its resource.
func newPreview(t texts, csp string) (string, map[string]any) {
	cfg := previewConfig{
		Embed:      framingAllowed(csp),
		VersionURL: strings.ReplaceAll(strings.ReplaceAll(t.versionURL, "<slug>", "{slug}"), "<N>", "{version}"),
	}
	js, err := json.Marshal(cfg) // escapes <, > and &, so it is safe inside <script>
	if err != nil {
		panic(err) // two plain fields always encode
	}
	ui := map[string]any{"prefersBorder": true}
	if cfg.Embed {
		// The page loads from the artifact hosts, e.g. https://*.art.example.com.
		origin := strings.TrimSuffix(strings.Replace(t.pageURL, "<slug>", "*", 1), "/")
		ui["csp"] = map[string]any{"frameDomains": []string{origin}}
	}
	return strings.Replace(previewHTML, "/*KAZARI_CONFIG*/null", string(js), 1), ui
}

// framingAllowed reports whether a page served with csp may be framed by
// another site: no policy, no frame-ancestors directive, or one naming a
// source other than 'none' and 'self'. Per-artifact overrides are not
// considered.
func framingAllowed(csp string) bool {
	fa, ok := parseCSP(csp)["frame-ancestors"]
	if !ok {
		return true
	}
	for _, src := range fa {
		switch strings.ToLower(src) {
		case "'none'", "'self'":
		default:
			return true
		}
	}
	return false
}

func (b *builder) addPreview(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         previewURI,
		Name:        "preview",
		Title:       "Artifact preview",
		Description: "Shows a published artifact: its name, links and, where the instance allows framing, the page itself.",
		MIMEType:    appMIME,
		Meta:        mcp.Meta{"ui": b.previewUI},
	}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: previewURI, MIMEType: appMIME, Text: b.preview, Meta: mcp.Meta{"ui": b.previewUI},
		}}}, nil
	})
}
