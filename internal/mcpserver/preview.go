package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The preview is an MCP App (SEP-1865): supporting hosts render publishing results as
// a card with the page's links and, when the CSP allows framing, the page; others ignore it.
const (
	previewURI    = "ui://kazari/preview.html"
	appMIME       = "text/html;profile=mcp-app"
	appsExtension = "io.modelcontextprotocol/ui"
)

//go:embed preview.html
var previewHTML string

type previewConfig struct {
	Embed bool `json:"embed"`
	// VersionURL has {slug} and {version} placeholders.
	VersionURL string `json:"versionURL"`
}

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
		// The artifact hosts, e.g. https://*.art.example.com.
		origin := strings.TrimSuffix(strings.Replace(t.pageURL, "<slug>", "*", 1), "/")
		ui["csp"] = map[string]any{"frameDomains": []string{origin}}
	}
	return strings.Replace(previewHTML, "/*KAZARI_CONFIG*/null", string(js), 1), ui
}

// framingAllowed reports whether another site may frame a page served with csp.
// Per-artifact overrides are not considered.
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
