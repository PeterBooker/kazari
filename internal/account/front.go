package account

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/theme"
)

// APIAssetPrefix is where the API's host serves the theme for its front
// page; the API itself lives under /v1/.
const APIAssetPrefix = "/assets/"

// FrontOptions configures the front pages.
type FrontOptions struct {
	// URL is the apex origin, such as https://art.example.com; MCPURL the
	// connector address and APIURL the API's base, with or without /v1.
	URL, MCPURL, APIURL string
	// AdminURL is the dashboard, "" when it is off.
	AdminURL string
}

// Front is the page a browser finds at the root of each public host that is
// not a page: the apex, the connector address and the API address. None
// needs a session, and none says more than the addresses.
type Front struct {
	Home, Connector, API http.Handler
}

type frontPage struct {
	base
	Kind                    string // home, connector or api
	MCPURL, CLIURL, Account string
	AdminURL                string
}

// NewFront returns the front pages.
func NewFront(o FrontOptions) Front {
	tmpl := template.Must(theme.Templates().Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	host := o.URL
	if u, err := url.Parse(o.URL); err == nil {
		host = u.Host
	}
	page := func(kind, assets, origin string) http.Handler {
		d := frontPage{
			Assets: theme.URLs(assets), Host: host, Kind: kind, MCPURL: o.MCPURL,
			CLIURL:  strings.TrimSuffix(strings.TrimSuffix(o.APIURL, "/"), "/v1"),
			Account: strings.TrimSuffix(o.URL, "/") + Prefix, AdminURL: o.AdminURL,
		}
		return noStore(middleware.Pages(strings.TrimSuffix(origin, "/"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := theme.Render(w, r, tmpl, "front", http.StatusOK, d); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		})))
	}
	return Front{
		Home:      page("home", assetPrefix, o.URL),
		Connector: page("connector", oauth.PathAssets, o.MCPURL),
		API:       page("api", APIAssetPrefix, strings.TrimSuffix(strings.TrimSuffix(o.APIURL, "/"), "/v1")),
	}
}
