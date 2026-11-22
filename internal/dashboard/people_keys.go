package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

// States of a key.
const (
	peopleKeyLive    = "live"
	peopleKeyRevoked = "revoked"
	peopleKeyExpired = "expired"
)

// peopleKey is an API key as a person's page lists it.
type peopleKey struct {
	user.APIKey
	State string
}

func newPeopleKey(k *user.APIKey, now time.Time) peopleKey {
	state := peopleKeyLive
	switch {
	case k.RevokedAt != nil:
		state = peopleKeyRevoked
	case k.ExpiresAt != nil && !now.Before(*k.ExpiresAt):
		state = peopleKeyExpired
	}
	return peopleKey{APIKey: *k, State: state}
}

// Tone is the theme's tone of the state.
func (k peopleKey) Tone() string {
	switch k.State {
	case peopleKeyRevoked:
		return "tone-danger"
	case peopleKeyExpired:
		return "tone-neutral"
	}
	return "tone-ok"
}

// StateWord is the state as a person reads it.
func (k peopleKey) StateWord() string { return strings.ToUpper(k.State[:1]) + k.State[1:] }

// Name is the key as lists and the activity log name it.
func (k peopleKey) Name() string { return user.KeyPrefix + k.Prefix }

// peopleScope is one checkbox of the form that issues a key.
type peopleScope struct {
	Name, Description string
	Checked           bool
}

// peopleKeyForm is the form that issues a key, as it was sent.
type peopleKeyForm struct {
	Label, Slugs, Days string
	Scopes             []peopleScope
	// Errors names what was refused, by field: scopes, slugs, days, or
	// form for what belongs to no field.
	Errors map[string]string
}

// maxKeyLifeDays bounds the lifetime a form may ask for, the same on the
// account page: ten years, past which "no expiry" is the honest choice.
const maxKeyLifeDays = 3650

// maxKeyDays is the longest a key may live on this instance in whole days,
// 0 when the instance sets no limit.
func (s *server) maxKeyDays() int { return int(s.o.MaxKeyTTL / (24 * time.Hour)) }

// newPeopleKeyForm offers the scopes u's role allows, the member ones
// ticked: an admin key is asked for by name.
func newPeopleKeyForm(u *user.User) peopleKeyForm {
	var f peopleKeyForm
	for _, sc := range user.ScopesFor(u) {
		f.Scopes = append(f.Scopes, peopleScope{Name: sc, Description: peopleScopeNames[sc], Checked: sc != auth.ScopeAdmin})
	}
	return f
}

func (s *server) peopleKeyIssue(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := newPeopleKeyForm(u)
	f.Label = strings.TrimSpace(r.PostForm.Get("label"))
	f.Slugs = strings.TrimSpace(r.PostForm.Get("slugs"))
	f.Days = strings.TrimSpace(r.PostForm.Get("days"))
	req := user.IssueKeyRequest{Handle: u.Handle, Label: f.Label,
		Slugs: strings.FieldsFunc(f.Slugs, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' || c == '\t' })}
	for i := range f.Scopes {
		f.Scopes[i].Checked = slices.Contains(r.PostForm["scopes"], f.Scopes[i].Name)
		if f.Scopes[i].Checked {
			req.Scopes = append(req.Scopes, f.Scopes[i].Name)
		}
	}
	// Each refusal is worded here and put beside its field: what the
	// service would say names its own types, not the form's.
	f.Errors = map[string]string{}
	if len(req.Scopes) == 0 {
		f.Errors["scopes"] = "Tick at least one thing the key may do."
	}
	if artifact.ValidateSlugPatterns(req.Slugs) != nil {
		f.Errors["slugs"] = "Write each as the first part of a page's address, or its start followed by *, such as docs-*."
	} else if len(req.Slugs) > 0 && slices.Contains(req.Scopes, auth.ScopeAdmin) {
		f.Errors["slugs"] = "A key that administers the instance cannot be restricted to certain pages. Untick that, or leave this empty."
	}
	if f.Days != "" {
		limit := maxKeyLifeDays
		if n := s.maxKeyDays(); n > 0 {
			limit = n
		}
		days, err := strconv.Atoi(f.Days)
		if err != nil || days < 1 || days > limit {
			f.Errors["days"] = "Enter a number of days from 1 to " + strconv.Itoa(limit) + ", or leave it empty."
		}
		req.ExpiresIn = time.Duration(days) * 24 * time.Hour
	}
	var (
		k      *user.APIKey
		secret string
	)
	if len(f.Errors) == 0 {
		k, secret, err = s.d.Users.IssueKey(ctx, req)
		switch {
		case errors.Is(err, user.ErrTooManyKeys):
			f.Errors["form"] = "They hold as many live keys as a person may. Revoke one first."
		case errors.Is(err, user.ErrDisabled):
			f.Errors["form"] = "They are disabled, and a disabled person gets no key."
		case errors.Is(err, user.ErrInvalidScope):
			f.Errors["scopes"] = "Their role does not allow that combination. Tick only what is offered."
		case errors.Is(err, user.ErrInvalidKey):
			f.Errors["slugs"] = "Those were not accepted. Write each as the first part of a page's address, or its start followed by *."
		case err != nil:
			return err
		}
	}
	if len(f.Errors) > 0 {
		d, err := s.peoplePersonData(ctx, c, u)
		if err != nil {
			return err
		}
		d.KeyForm = f
		return s.render(w, r, c, http.StatusUnprocessableEntity, "people-person", peoplePage(d.Row.Name), d)
	}
	return s.peopleShowKey(w, r, c, u, k, secret, "New key for "+u.Handle, nil)
}

func (s *server) peopleShowKey(w http.ResponseWriter, r *http.Request, c *request, u *user.User, k *user.APIKey, secret, title string, notes []string) error {
	notes = append(notes,
		"Kazari keeps only a hash of it. After you leave this page it cannot be shown again, only rotated or revoked.",
		"It is named "+user.KeyPrefix+k.Prefix+" in lists and in the activity log. It may: "+strings.ToLower(scopeWords(k.Scopes))+".")
	if len(k.Slugs) > 0 {
		notes = append(notes, "It reaches only pages whose address matches "+strings.Join(k.Slugs, ", ")+".")
	}
	if k.ExpiresAt != nil {
		notes = append(notes, "It stops working on "+k.ExpiresAt.UTC().Format(timeLayout)+".")
	} else {
		notes = append(notes, "It works until it is revoked.")
	}
	name := u.DisplayName
	if name == "" {
		name = u.Handle
	}
	return s.showOnce(w, r, c, shownOnce{
		Handle: u.Handle, Back: "Done, back to " + name, Title: title,
		Lede:  "Copy the key now and hand it to " + name + " over a channel you trust. It acts as them.",
		Label: "API key", Value: secret, Button: "Copy the key",
		Notes: notes,
		Snippets: []snippet{
			{ID: "key-connect", Title: "Connect an agent with it", Button: "Copy the command",
				Text: "claude mcp add --transport http kazari " + s.o.MCPURL + " \\\n  --header \"Authorization: Bearer " + secret + "\""},
			{ID: "key-header", Title: "Or send it as a header", Button: "Copy the header",
				Text: "Authorization: Bearer " + secret},
			{ID: "key-cli", Title: "Or use it with the kazari command", Button: "Copy the lines",
				Text: "export KAZARI_URL=" + strings.TrimSuffix(s.o.APIURL, "/v1") + "\nexport KAZARI_TOKEN=" + secret},
		},
	})
}

// peopleKeyOf loads the key a path names if it is the person's, or the 404
// page: another person's prefix names nothing here.
func (s *server) peopleKeyOf(r *http.Request) (*user.User, *user.APIKey, []user.APIKey, error) {
	u, err := s.peopleUser(r)
	if err != nil {
		return nil, nil, nil, err
	}
	keys, err := s.d.Users.ListKeys(r.Context(), u.Handle)
	if err != nil {
		return nil, nil, nil, err
	}
	prefix := r.PathValue("prefix")
	for i := range keys {
		if keys[i].Prefix == prefix {
			return u, &keys[i], keys, nil
		}
	}
	return nil, nil, nil, notFound(u.Handle + " has no key with that prefix.")
}

// peopleOverlaps are how long a rotated key may keep working.
var peopleOverlaps = []struct {
	value string
	d     time.Duration
	label string
}{
	{"0", 0, "Stop the old key at once"},
	{"1h", time.Hour, "Keep the old key working for an hour"},
	{"24h", 24 * time.Hour, "Keep the old key working for a day"},
	{"168h", 7 * 24 * time.Hour, "Keep the old key working for a week"},
}

func (s *server) peopleKeyRotateConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, k, _, err := s.peopleKeyOf(r)
	if err != nil {
		return err
	}
	name := user.KeyPrefix + k.Prefix
	sel := &confirmSelect{Name: "overlap", Label: "The old key", Help: "Time to put the new key where the old one is used."}
	for _, o := range peopleOverlaps {
		sel.Options = append(sel.Options, confirmOption{Value: o.value, Label: o.label, Selected: o.d == user.DefaultRotateOverlap})
	}
	return s.confirm(w, r, c, confirmation{
		Handle:     u.Handle,
		Title:      "Rotate " + name,
		Lede:       "Rotating gives the key a new secret. The new key has the same label, scopes and slug patterns, and is shown once on the next page.",
		EndsTitle:  "What ends",
		Ends:       []string{"The old key, at once or after the time you choose."},
		StaysTitle: "What stays",
		Stays: []string{
			"Every key issued with the old key, and every app approved with it, until the old key ends.",
			"Nothing moves to the new key by itself: put it wherever the old one is used.",
		},
		Select: sel,
		Action: peoplePath(u.Handle) + "/keys/" + k.Prefix + "/rotate", Button: "Rotate the key", Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleKeyRotate(w http.ResponseWriter, r *http.Request, c *request) error {
	u, k, _, err := s.peopleKeyOf(r)
	if err != nil {
		return err
	}
	overlap := user.DefaultRotateOverlap
	for _, o := range peopleOverlaps {
		if o.value == r.PostForm.Get("overlap") {
			overlap = o.d
		}
	}
	next, secret, old, err := s.d.Users.RotateKey(r.Context(), user.KeyRef{Handle: u.Handle, Prefix: k.Prefix}, overlap)
	switch {
	case errors.Is(err, user.ErrKeyNotFound):
		return refused("%s%s is revoked or expired, and a key that has ended is not rotated. Issue a new one.", user.KeyPrefix, k.Prefix)
	case errors.Is(err, user.ErrTooManyKeys):
		return refused("They hold as many live keys as a person may. Revoke one first.")
	case err != nil:
		return err
	}
	note := "The old key, " + user.KeyPrefix + old.Prefix + ", has stopped working."
	if old.ExpiresAt != nil && old.ExpiresAt.After(s.now()) {
		note = "The old key, " + user.KeyPrefix + old.Prefix + ", works until " + old.ExpiresAt.UTC().Format(timeLayout) + "."
	}
	return s.peopleShowKey(w, r, c, u, next, secret, "New key for "+u.Handle, []string{note})
}

func (s *server) peopleKeyRevokeConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, k, keys, err := s.peopleKeyOf(r)
	if err != nil {
		return err
	}
	if k.RevokedAt != nil {
		return refused("%s%s is revoked already.", user.KeyPrefix, k.Prefix)
	}
	name := user.KeyPrefix + k.Prefix
	ends := []string{
		"The key " + name + " stops working at once.",
		"Every key issued with it ends too, and every key issued with one of those.",
		"Every app approved with one of them is disconnected, and every browser signed in with one is signed out.",
	}
	var issued []string
	for _, other := range keys {
		if other.IssuedBy == k.Prefix && other.RevokedAt == nil {
			issued = append(issued, user.KeyPrefix+other.Prefix)
		}
	}
	if len(issued) > 0 {
		ends = append(ends, "Issued with it now: "+strings.Join(issued, ", ")+".")
	}
	return s.confirm(w, r, c, confirmation{
		Handle:    u.Handle,
		Title:     "Revoke " + name,
		Lede:      "A revoked key cannot be brought back. To replace a key that is in use, rotate it instead.",
		EndsTitle: "What ends for good", Ends: ends,
		Action: peoplePath(u.Handle) + "/keys/" + k.Prefix + "/revoke", Button: "Revoke the key", Danger: true, Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleKeyRevoke(w http.ResponseWriter, r *http.Request, _ *request) error {
	u, k, _, err := s.peopleKeyOf(r)
	if err != nil {
		return err
	}
	if err := s.d.Users.RevokeKey(r.Context(), user.KeyRef{Handle: u.Handle, Prefix: k.Prefix}); err != nil {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-key-revoked")
}

// peopleApp is a connected app: an OAuth client a person approved.
type peopleApp struct {
	ID        int64
	Client    string // its name, or its id when it gave none
	Scopes    []string
	Refreshed time.Time // when it last renewed its tokens
	Lapses    time.Time // when it ends unless it renews them again
	KeyPrefix string    // the key it was approved with, "" for none
}

func (s *server) peopleApps(ctx context.Context, handle string) ([]peopleApp, error) {
	grants, err := s.d.Grants.ListGrants(ctx, handle)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	apps := make([]peopleApp, 0, len(grants))
	for _, g := range grants {
		name, ok := names[g.ClientID]
		if !ok {
			name = g.ClientID
			client, err := s.d.Grants.GetClient(ctx, g.ClientID)
			switch {
			case err == nil && client.Name != "":
				name = client.Name
			case err != nil && !errors.Is(err, oauth.ErrNotFound):
				return nil, err
			}
			names[g.ClientID] = name
		}
		apps = append(apps, peopleApp{ID: g.FamilyID, Client: name, Scopes: g.Scopes, Refreshed: g.CreatedAt, Lapses: g.ExpiresAt, KeyPrefix: g.KeyPrefix})
	}
	return apps, nil
}

// peopleAppOf loads the app a path names if it is the person's.
func (s *server) peopleAppOf(r *http.Request) (*user.User, *peopleApp, error) {
	u, err := s.peopleUser(r)
	if err != nil {
		return nil, nil, err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, nil, notFound(u.Handle + " has no such connected app.")
	}
	apps, err := s.peopleApps(r.Context(), u.Handle)
	if err != nil {
		return nil, nil, err
	}
	for i := range apps {
		if apps[i].ID == id {
			return u, &apps[i], nil
		}
	}
	return nil, nil, notFound(u.Handle + " has no such connected app. It may have been disconnected already.")
}

func (s *server) peopleAppRevokeConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, app, err := s.peopleAppOf(r)
	if err != nil {
		return err
	}
	return s.confirm(w, r, c, confirmation{
		Handle:     u.Handle,
		Title:      "Disconnect " + app.Client,
		Lede:       "The app loses its access to this instance as " + u.Handle + ".",
		EndsTitle:  "What ends for good",
		Ends:       []string{"The app's connection: its next request is refused."},
		StaysTitle: "What stays",
		Stays: []string{
			"Their keys, their sessions and their other apps.",
			"The way back: they connect the app again by signing in from it and approving it.",
		},
		Action: peoplePath(u.Handle) + "/apps/" + strconv.FormatInt(app.ID, 10) + "/revoke",
		Button: "Disconnect the app", Danger: true, Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleAppRevoke(w http.ResponseWriter, r *http.Request, _ *request) error {
	u, app, err := s.peopleAppOf(r)
	if err != nil {
		return err
	}
	if err := s.d.Grants.RevokeGrant(r.Context(), app.ID); err != nil && !errors.Is(err, oauth.ErrNotFound) {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-app-revoked")
}
