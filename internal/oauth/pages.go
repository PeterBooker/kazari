package oauth

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/theme"
)

//go:embed templates/pages.html
var templateFS embed.FS

// PathAssets is where the sign-in host serves the stylesheet, the script
// and the fonts its pages load: the pages allow their own origin and
// nothing else.
const PathAssets = "/oauth/assets/"

type pages struct {
	tmpl   *template.Template
	assets theme.Assets
	// foot is the sign-in host's name under every page, so a person can see
	// which host they are about to type a password on.
	foot string
}

// newPages parses the pages into the shared theme. origin is the issuer's.
func newPages(origin string) *pages {
	foot := origin
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		foot = u.Host
	}
	return &pages{
		tmpl:   template.Must(theme.Templates().ParseFS(templateFS, "templates/pages.html")),
		assets: theme.URLs(PathAssets),
		foot:   foot,
	}
}

// view is what every page template reads: the document head, the line under
// the card and the page's own data.
type view struct {
	Page theme.Page
	Foot string
	V    any
}

// titled is a page that names itself in the browser tab.
type titled interface{ tabTitle() string }

func (m messagePage) tabTitle() string { return m.Title }
func (consentPage) tabTitle() string   { return "Authorize access" }
func (p signInPage) tabTitle() string  { return p.Title }

type messagePage struct {
	Title, Message, Detail string
}

type scopeView struct {
	Name, Description string
}

type consentPage struct {
	ClientName   string
	ClientID     string
	RedirectHost string
	Handle       string // "" when signing in with an API key
	Scopes       []scopeView
	RequestID    string
	AskKey       bool // no identity provider: the key is how the person signs in
	OfferKey     bool // signed in: a key of theirs may bound the grant instead
	Error        string
}

// signInPage asks for a password or an API key, on an instance without an
// identity provider. The sign-in continues on a host (Host, Path, Nonce) or
// with an MCP client's pending authorization (RequestID).
type signInPage struct {
	Title    string
	Lede     string
	Site     string // origin of an artifact host, in place of Lede
	Password bool   // the password form; else the API-key form
	Switch   string // the address of the other form, "" when there is none
	Lifetime string // of a session, e.g. "12 hours"
	Host     string
	Path     string
	Nonce    string
	// RequestID continues an MCP client's authorization; KeyConsent then
	// offers the consent page that takes an API key in place of a sign-in.
	RequestID  string
	KeyConsent bool
	Login      string // what the person typed, shown again after a refusal
	Error      string

	formTarget string // where the browser goes on to, for the CSP
}

var scopeDescriptions = map[string]string{
	auth.ScopeRead:   "List and read your artifacts",
	auth.ScopeWrite:  "Create and update artifacts",
	auth.ScopeShare:  "Change who can open artifacts, and create share links",
	auth.ScopeDelete: "Delete artifacts",
}

func scopeViews(scopes []string) []scopeView {
	out := make([]scopeView, 0, len(scopes))
	for _, s := range scopes {
		d := scopeDescriptions[s]
		if d == "" {
			d = s
		}
		out = append(out, scopeView{Name: s, Description: d})
	}
	return out
}

// safeOrigin matches an origin that can be put in a CSP source list as is.
var safeOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$|^http://\[::1\](:[0-9]+)?$`)

const formActionSelf = "form-action 'self'"

// pagePolicy is middleware.PageCSP for a page whose form ends at
// formTarget: browsers apply form-action to the redirect after a
// submission, so that one origin joins it. Nothing else is loosened.
func pagePolicy(formTarget string) string {
	if formTarget == "" {
		return middleware.PageCSP
	}
	u, err := url.Parse(formTarget)
	if err != nil {
		return middleware.PageCSP
	}
	origin := u.Scheme + "://" + u.Host
	if !safeOrigin.MatchString(origin) {
		return middleware.PageCSP
	}
	return strings.Replace(middleware.PageCSP, formActionSelf, formActionSelf+" "+origin, 1)
}

// render writes an HTML page with its security headers. formTarget is where
// the page's form sends the browser on to, if that is another origin.
func (p *pages) render(w http.ResponseWriter, status int, name string, data any, formTarget string) {
	title := "Sign out"
	if t, ok := data.(titled); ok {
		title = t.tabTitle()
	}
	var buf bytes.Buffer
	v := view{Page: theme.Page{Title: title, Assets: p.assets}, Foot: p.foot, V: data}
	if err := p.tmpl.ExecuteTemplate(&buf, name, v); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", pagePolicy(formTarget))
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Service) message(w http.ResponseWriter, status int, title, msg, detail string) {
	s.pages.render(w, status, "message", messagePage{Title: title, Message: msg, Detail: detail}, "")
}
