package oauth

import (
	"embed"
	"net/http"

	"github.com/PeterBooker/kazari/internal/auth"
)

//go:embed templates/consent.html
var templateFS embed.FS

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

func (consentPage) TabTitle() string { return "Authorise access" }

var scopeDescriptions = map[string]string{
	auth.ScopeRead:   "List and read your pages",
	auth.ScopeWrite:  "Create and update pages",
	auth.ScopeShare:  "Change who can open pages, and create share links",
	auth.ScopeDelete: "Delete pages",
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

func (s *Service) message(w http.ResponseWriter, status int, title, msg, detail string) {
	s.pages.Message(w, status, title, msg, detail)
}

// fail logs an error that is not the visitor's doing and answers with a
// page that says nothing about it.
func (s *Service) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.ErrorContext(r.Context(), "oauth: "+what, "err", err)
	s.pages.Failed(w)
}
