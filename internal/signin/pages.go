package signin

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/theme"
)

//go:embed templates/pages.html
var templateFS embed.FS

// Pages renders the pages of the MCP host, the sign-in host's and the
// authorization server's alike: one look, one set of assets at PathAssets,
// one strict policy.
type Pages struct {
	tmpl   *template.Template
	assets theme.Assets
	// foot is the MCP host's name under every page, so a person can see
	// which host they are about to type a password on.
	foot string
}

// NewPages parses the sign-in host's pages, and those of fsys that
// patterns name, into the shared theme. origin is the MCP host's. The
// pages of fsys may use the "message" and "alert" templates.
func NewPages(origin string, fsys fs.FS, patterns ...string) *Pages {
	foot := origin
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		foot = u.Host
	}
	tmpl := template.Must(theme.Templates().ParseFS(templateFS, "templates/pages.html"))
	if fsys != nil {
		tmpl = template.Must(tmpl.ParseFS(fsys, patterns...))
	}
	return &Pages{tmpl: tmpl, assets: theme.URLs(PathAssets), foot: foot}
}

// view is what every page template reads: the document head, the line under
// the card and the page's own data.
type view struct {
	Page theme.Page
	Foot string
	V    any
}

// Titled is a page's data that names the page in the browser tab.
type Titled interface{ TabTitle() string }

// Render writes the page name with its security headers. formTarget is
// where the page's form sends the browser on to, if that is another origin.
func (p *Pages) Render(w http.ResponseWriter, status int, name string, data any, formTarget string) {
	title := "Sign out"
	if t, ok := data.(Titled); ok {
		title = t.TabTitle()
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
	h.Set("Content-Security-Policy", middleware.PageCSPFor(formTarget))
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// Message writes a page that says one thing; detail, if any, is shown
// under it in small print.
func (p *Pages) Message(w http.ResponseWriter, status int, title, msg, detail string) {
	p.Render(w, status, "message", messagePage{Title: title, Message: msg, Detail: detail}, "")
}

// Failed writes the page of an error that is not the visitor's doing,
// which says nothing about it.
func (p *Pages) Failed(w http.ResponseWriter) {
	p.Message(w, http.StatusInternalServerError, "Something went wrong", "Please try again later.", "")
}

type messagePage struct {
	Title, Message, Detail string
}

func (m messagePage) TabTitle() string { return m.Title }

// signInPage asks for a password or an API key, on an instance without an
// identity provider. The sign-in continues on a host (Host, Path, Nonce) or
// with an MCP client's pending authorization (RequestID).
type signInPage struct {
	Title    string
	Lede     string
	Site     string // origin of an artifact host, in place of Lede
	Password bool   // the password form; else the API-key form
	Switch   string // the address of the other form, "" when there is none
	Recover  string // the recovery-code page, "" when there is none
	// Passkey is the ceremony of the passkey form beside the password
	// form; nil leaves the form out.
	Passkey *passkey.Ceremony
	// PasskeyError says why the last passkey was not accepted.
	PasskeyError string
	Lifetime     string // of a session, e.g. "12 hours"
	Host         string
	Path         string
	Nonce        string
	// RequestID continues an MCP client's authorization; KeyConsent then
	// offers the consent page that takes an API key in place of a sign-in.
	RequestID  string
	KeyConsent bool
	Login      string // what the person typed, shown again after a refusal
	Error      string
	// Notice is a fixed word for a line above the form: "recovered" after
	// a new password was set with a recovery code.
	Notice string

	formTarget string // where the browser goes on to, for the CSP
}

func (p signInPage) TabTitle() string { return p.Title }
