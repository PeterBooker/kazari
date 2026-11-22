package dashboard

import "net/http"

// confirmation is the page shown before a change that ends something: what
// it ends, what stays, and the one button that does it. People and Pages
// share it, so every such page reads the same way.
type confirmation struct {
	// Slug or Handle is what the change is about, for the way back: a page
	// or a person. Both empty is a change under People about nobody yet.
	Slug, Handle string
	Title        string
	Lede         string
	EndsTitle    string
	Ends         []string
	StaysTitle   string
	Stays        []string
	Select       *confirmSelect
	Check        *confirmCheck
	// Hidden are fields the form carries without showing them.
	Hidden map[string]string
	Note   string // why there is no option, when there is none
	Action string // where the form posts to
	Button string
	Danger bool
	Cancel string
}

type confirmSelect struct {
	Name, Label, Help string
	Required          bool
	Options           []confirmOption
}

type confirmOption struct {
	Value, Label string
	Selected     bool
}

type confirmCheck struct {
	Name, Label, Help string
	// Legend, when set, heads the option as a group of its own.
	Legend  string
	Checked bool
}

func (c confirmation) nav() string {
	if c.Slug != "" {
		return "pages"
	}
	return "people"
}

func (s *server) confirm(w http.ResponseWriter, r *http.Request, c *request, d confirmation) error {
	return s.render(w, r, c, http.StatusOK, "confirm", page{Title: d.Title, Nav: d.nav()}, d)
}

// shownOnce is the page that shows a new secret, a link or a key, in the
// response to the POST that made it and never again.
type shownOnce struct {
	// Slug or Handle is what the secret belongs to, as in a confirmation.
	Slug, Handle string
	Title        string
	Lede         string
	Label        string // of the field that holds the secret
	Value        string
	Button       string
	Notes        []string
	Snippets     []snippet
	Back         string // what the way back is called
}

// snippet is a block of text to copy, with what it is for.
type snippet struct {
	ID, Title, Text, Button string
}

// showOnce renders the page that shows a secret. The response is never
// stored, and the secret goes nowhere else: not into a redirect, a cookie
// or a flash.
func (s *server) showOnce(w http.ResponseWriter, r *http.Request, c *request, d shownOnce) error {
	nav := "people"
	if d.Slug != "" {
		nav = "pages"
	}
	w.Header().Set("Cache-Control", "no-store")
	return s.render(w, r, c, http.StatusOK, "once", page{Title: d.Title, Nav: nav}, d)
}
