package oauth

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"regexp"

	"kazari/internal/auth"
)

//go:embed templates/pages.html
var templateFS embed.FS

type pages struct {
	tmpl *template.Template
}

func newPages() *pages {
	return &pages{tmpl: template.Must(template.ParseFS(templateFS, "templates/pages.html"))}
}

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
	AskKey       bool
	Error        string
}

// signInPage asks for an API key to open a gated artifact, on an instance
// without an identity provider.
type signInPage struct {
	Site     string // origin of the artifact host
	Lifetime string // of a session, e.g. "12 hours"
	Host     string
	Path     string
	Nonce    string
	Error    string
}

var scopeDescriptions = map[string]string{
	auth.ScopeRead:   "List and read your artifacts",
	auth.ScopeWrite:  "Create and update artifacts",
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

const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'"

// safeOrigin matches an origin that can be put in a CSP source list as is.
var safeOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$|^http://\[::1\](:[0-9]+)?$`)

// render writes an HTML page with its security headers. formTarget's origin,
// when set, joins form-action because browsers apply it to the redirect
// after a form submission.
func (p *pages) render(w http.ResponseWriter, status int, name string, data any, formTarget string) {
	var buf bytes.Buffer
	if err := p.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	csp := pageCSP
	if u, err := url.Parse(formTarget); err == nil && formTarget != "" {
		origin := u.Scheme + "://" + u.Host
		if safeOrigin.MatchString(origin) {
			csp += " " + origin
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", csp)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Service) message(w http.ResponseWriter, status int, title, msg, detail string) {
	s.pages.render(w, status, "message", messagePage{Title: title, Message: msg, Detail: detail}, "")
}
