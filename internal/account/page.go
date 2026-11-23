package account

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/user"
)

// securityActions are the events about a person's account that the page
// lists, theirs and an admin's alike: nobody is told of them by email, so
// here is where they show.
var securityActions = []string{
	signin.ActionSignIn, audit.PasswordSet, "recovery.", "passkey.", "key.", oauth.ActionGrant, oauth.ActionRevoke, audit.SessionRevoke,
	"invite.", audit.UserCreate, audit.UserUpdate,
}

const (
	securityEvents = 10
	maxOwnPages    = 50
)

// keyRow is one API key as the page lists it.
type keyRow struct {
	user.APIKey
	Live  bool
	Admin bool // holds the admin scope: rotated on the admin API alone
}

// appRow is one connected app.
type appRow struct {
	ID        int64
	Client    string
	ClientID  string
	Scopes    []string
	KeyPrefix string
	Refreshed time.Time
	Lapses    time.Time
}

// pageRow is one of the person's artifacts.
type pageRow struct {
	Slug, Name, URL string
	Visibility      string
	UpdatedAt       time.Time
}

// keyForm is what the person typed into the new-key form. Scopes is nil
// until it was sent, when every box starts ticked.
type keyForm struct {
	Label, Slugs, Days string
	Scopes             []string
}

type accountPageData struct {
	base
	Flash string
	User  *user.User
	// MCPURL is the connector address; AdminURL the dashboard, "" when it
	// is off.
	MCPURL, AdminURL string
	// Scopes are the boxes of the new-key form: what the session may give
	// a key.
	Scopes []scopeBox
	// Local shows the password section. KeySession is a sign-in with an API
	// key, named by KeyPrefix, which bounds the keys issued from here.
	Local      bool
	KeySession bool
	KeyPrefix  string
	MinLength  int
	MaxKeyDays int // 0 = no limit
	// RecoveryCodes is how many unused recovery codes the person holds.
	RecoveryCodes int
	// PasskeysOn shows the passkey section, whose list is Passkeys and
	// whose form carries Ceremony; PasskeyName is what was typed into it.
	PasskeysOn  bool
	Passkeys    []user.Passkey
	Ceremony    *passkey.Ceremony
	PasskeyName string
	// Confirm lets the forms that ask for the current password take one of
	// the person's passkeys instead; nil when they have none.
	Confirm *passkey.Ceremony
	SignIns []audit.Event
	Events  []audit.Event
	Keys    []keyRow // live
	OldKeys []keyRow // revoked or expired
	Apps    []appRow
	Pages   []pageRow
	// Err names what is wrong with a field; Seconds is the wait of
	// Err["current"] == "wait".
	Err     map[string]string
	Seconds int
	Key     keyForm
}

func (s *server) accountPage(w http.ResponseWriter, r *http.Request, v visit) {
	d := accountPageData{Flash: s.sess.Flash(w, r)}
	s.renderAccount(w, r, http.StatusOK, v, d)
}

// renderAccount loads the page's lists and renders it with d's flash,
// errors and typed values.
func (s *server) renderAccount(w http.ResponseWriter, r *http.Request, status int, v visit, d accountPageData) {
	ctx := r.Context()
	d.base = s.baseFor(v)
	d.User = v.user
	d.Local = s.o.LocalAccounts
	d.KeySession, d.KeyPrefix = v.sess.KeyPrefix != "", v.sess.KeyPrefix
	d.MinLength = s.d.Users.MinPasswordLength()
	d.MaxKeyDays = int(s.o.MaxKeyTTL / (24 * time.Hour))
	d.MCPURL, d.AdminURL = s.o.MCPURL, s.o.AdminURL
	d.Scopes = scopeBoxes(principal(r).Scopes, d.Key.Scopes)

	var err error
	if d.Local {
		acc, err := s.d.Users.Account(ctx, v.user.Handle)
		if err != nil {
			s.fail(w, r, "load account", err)
			return
		}
		d.RecoveryCodes = acc.RecoveryCodes
	}
	if d.Local && s.d.Passkeys != nil {
		d.PasskeysOn = true
		if d.Passkeys, err = s.d.Users.Passkeys(ctx, v.user.Handle); err != nil {
			s.fail(w, r, "list passkeys", err)
			return
		}
		if len(d.Passkeys) < user.MaxPasskeys {
			c, err := s.d.Passkeys.Registration(ctx, v.user)
			if err != nil {
				s.fail(w, r, "begin passkey registration", err)
				return
			}
			d.Ceremony = &c
		}
		c, ok, err := s.d.Passkeys.Confirmation(ctx, v.user)
		if err != nil {
			s.fail(w, r, "begin passkey confirmation", err)
			return
		}
		if ok {
			d.Confirm = &c
		}
	}
	keys, err := s.keys(ctx, v.user.Handle)
	if err != nil {
		s.fail(w, r, "list keys", err)
		return
	}
	for _, k := range keys {
		if k.Live {
			d.Keys = append(d.Keys, k)
		} else {
			d.OldKeys = append(d.OldKeys, k)
		}
	}
	if d.Apps, err = s.apps(ctx, v.user.Handle); err != nil {
		s.fail(w, r, "list grants", err)
		return
	}
	if s.d.Audit != nil {
		signIns, err := s.d.Audit.List(ctx, audit.Query{Subject: v.user.Handle, Actions: []string{signin.ActionSignIn}, Limit: 2})
		if err != nil {
			s.fail(w, r, "list sign-ins", err)
			return
		}
		events, err := s.d.Audit.List(ctx, audit.Query{Subject: v.user.Handle, Actions: securityActions, Limit: securityEvents})
		if err != nil {
			s.fail(w, r, "list events", err)
			return
		}
		d.SignIns, d.Events = signIns.Events, events.Events
	}
	if s.d.Artifacts != nil {
		page, err := s.d.Artifacts.List(ctx, artifact.Actor{Handle: v.user.Handle},
			artifact.ListQuery{Owner: v.user.Handle, Limit: maxOwnPages})
		if err != nil {
			s.fail(w, r, "list pages", err)
			return
		}
		for _, a := range page.Items {
			vis := string(a.Visibility)
			if a.UnpublishedAt != nil {
				vis = string(artifact.Unpublished)
			}
			d.Pages = append(d.Pages, pageRow{Slug: a.Slug, Name: a.Name, URL: s.d.Links.ArtifactURL(a.Slug), Visibility: vis, UpdatedAt: a.UpdatedAt})
		}
	}
	s.render(w, r, status, "account", d)
}

// keys returns the person's keys, newest first.
func (s *server) keys(ctx context.Context, handle string) ([]keyRow, error) {
	keys, err := s.d.Users.ListKeys(ctx, handle)
	if err != nil {
		return nil, err
	}
	now := s.now()
	rows := make([]keyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, keyRow{
			APIKey: k,
			Live:   k.RevokedAt == nil && (k.ExpiresAt == nil || now.Before(*k.ExpiresAt)),
			Admin:  slices.Contains(k.Scopes, auth.ScopeAdmin),
		})
	}
	return rows, nil
}

// apps returns the person's connected apps with their clients' names.
func (s *server) apps(ctx context.Context, handle string) ([]appRow, error) {
	grants, err := s.d.Grants.ListGrants(ctx, handle)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	rows := make([]appRow, 0, len(grants))
	for _, g := range grants {
		name, ok := names[g.ClientID]
		if !ok {
			// A client that is gone leaves its id to show; any other
			// failure is not a reason to show an app under the wrong name.
			c, err := s.d.Grants.GetClient(ctx, g.ClientID)
			switch {
			case err == nil:
				name = c.Name
			case !errors.Is(err, oauth.ErrNotFound):
				return nil, err
			}
			names[g.ClientID] = name
		}
		rows = append(rows, appRow{
			ID: g.FamilyID, Client: name, ClientID: g.ClientID, Scopes: g.Scopes, KeyPrefix: g.KeyPrefix,
			Refreshed: g.CreatedAt, Lapses: g.ExpiresAt,
		})
	}
	return rows, nil
}

// changePassword replaces the person's password, once they confirm it is
// them, and ends their sessions in other browsers.
func (s *server) changePassword(w http.ResponseWriter, r *http.Request, v visit) {
	f := r.PostForm
	refuse := func(status int, d accountPageData) { s.renderAccount(w, r, status, v, d) }
	if f.Get("new") != f.Get("confirm") {
		refuse(http.StatusUnprocessableEntity, accountPageData{Err: map[string]string{"confirm": "mismatch"}})
		return
	}
	if s.refused(w, r, v, "current", accountPageData{}) {
		return
	}
	err := s.d.Users.ChangePasswordConfirmed(r.Context(), v.user.Handle, f.Get("new"), v.sess.SessionID)
	switch {
	case errors.Is(err, user.ErrWeakPassword):
		refuse(http.StatusUnprocessableEntity, accountPageData{Err: map[string]string{"new": "weak"}})
		return
	case errors.Is(err, user.ErrBusy):
		s.busy(w, r)
		return
	case err != nil:
		s.fail(w, r, "change password", err)
		return
	}
	s.done(w, r, "password-changed")
}

// endSessions signs the person out of every browser but this one.
func (s *server) endSessions(w http.ResponseWriter, r *http.Request, v visit) {
	if err := s.d.Grants.EndSessions(r.Context(), v.user.Handle, v.sess.SessionID); err != nil {
		s.fail(w, r, "end sessions", err)
		return
	}
	s.done(w, r, "sessions-ended")
}

// confirmPage asks before something that cannot be undone.
type confirmPage struct {
	base
	Kind string // "key" or "app"
	Key  *keyRow
	// Own is the key this session was opened with: revoking it signs out.
	Own bool
	// Issued are the live keys issued with Key; Apps the apps approved
	// with it. Both end with it.
	Issued []keyRow
	Apps   []appRow
	App    *appRow
	// Err is why revoking Key was refused, "" for none.
	Err string
}

// ownApp returns the person's connected app the path names. Another
// person's grant, like one that does not exist, is not found.
func (s *server) ownApp(w http.ResponseWriter, r *http.Request, v visit) (*appRow, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		s.message(w, r, http.StatusNotFound, "not-found")
		return nil, false
	}
	apps, err := s.apps(r.Context(), v.user.Handle)
	if err != nil {
		s.fail(w, r, "list grants", err)
		return nil, false
	}
	for i := range apps {
		if apps[i].ID == id {
			return &apps[i], true
		}
	}
	s.message(w, r, http.StatusNotFound, "not-found")
	return nil, false
}

func (s *server) revokeAppPage(w http.ResponseWriter, r *http.Request, v visit) {
	app, ok := s.ownApp(w, r, v)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "confirm", confirmPage{base: s.baseFor(v), Kind: "app", App: app})
}

func (s *server) revokeApp(w http.ResponseWriter, r *http.Request, v visit) {
	app, ok := s.ownApp(w, r, v)
	if !ok {
		return
	}
	if err := s.d.Grants.RevokeGrant(r.Context(), app.ID); err != nil && !errors.Is(err, oauth.ErrNotFound) {
		s.fail(w, r, "revoke grant", err)
		return
	}
	s.done(w, r, "app-revoked")
}
