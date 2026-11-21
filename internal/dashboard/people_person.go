package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/user"
)

// peopleForm is the profile form as it was sent, with the sentence for
// each field that was refused.
type peopleForm struct {
	Name, Email, Role, Quota string
	Errors                   map[string]string
}

// peopleOwnedPage is one of a person's pages, with who can open it and its
// state in the words of the Pages list.
type peopleOwnedPage struct {
	Slug, Name string
	Open       string
	State      pageState
	Updated    time.Time
}

type peoplePersonData struct {
	Row        peopleRow
	CreatedAt  time.Time
	DisabledAt *time.Time
	// Self is the admin looking at their own page, where disabling is not
	// offered.
	Self  bool
	Local bool
	Form  peopleForm

	HasPassword bool
	Pending     []peopleLink // their links that can still be used

	Keys, PastKeys []peopleKey
	KeyForm        peopleKeyForm
	MaxKeyDays     int

	Apps []peopleApp

	Identities []user.Identity
	// CanLink shows the form that links a provider sign-in by hand.
	CanLink bool

	Sessions int

	Pages     []peopleOwnedPage
	MorePages bool
	// Owns is whether they own any page at all, those in the bin included:
	// what a hand-over would move.
	Owns bool

	Events []audit.Event
}

func (s *server) peoplePerson(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	d, err := s.peoplePersonData(r.Context(), c, u)
	if err != nil {
		return err
	}
	return s.render(w, r, c, http.StatusOK, "people-person", peoplePage(d.Row.Name), d)
}

// peopleQuotaText is a quota as the profile form shows and reads it.
func peopleQuotaText(n int64) string {
	if n == 0 {
		return "no limit"
	}
	return humanBytes(n)
}

// peoplePersonData builds one person's page from queries about that person
// alone: the page is opened, and rebuilt for every refused form, on
// instances of any size.
func (s *server) peoplePersonData(ctx context.Context, c *request, u *user.User) (*peoplePersonData, error) {
	acc, err := s.d.Users.Account(ctx, u.Handle)
	if err != nil {
		return nil, err
	}
	used, err := s.d.Artifacts.Usage(ctx, u.Handle)
	if err != nil {
		return nil, err
	}
	d := &peoplePersonData{
		Row:       newPeopleRow(u, acc, artifact.OwnerUsage{Handle: u.Handle, Bytes: used}),
		CreatedAt: u.CreatedAt, DisabledAt: u.DisabledAt, Self: u.Handle == c.me.Handle, Local: s.o.LocalAccounts,
		Form:        peopleForm{Name: u.DisplayName, Email: u.Email, Role: string(u.Role), Quota: peopleQuotaText(u.QuotaBytes)},
		KeyForm:     newPeopleKeyForm(u),
		MaxKeyDays:  s.maxKeyDays(),
		HasPassword: acc.HasPassword,
	}
	links, err := s.peopleLinks(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.Handle == u.Handle {
			d.Pending = append(d.Pending, l)
		}
	}

	keys, err := s.d.Users.ListKeys(ctx, u.Handle)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for i := range keys {
		k := newPeopleKey(&keys[i], now)
		if k.State == peopleKeyLive {
			d.Keys = append(d.Keys, k)
		} else {
			d.PastKeys = append(d.PastKeys, k)
		}
	}
	if d.Apps, err = s.peopleApps(ctx, u.Handle); err != nil {
		return nil, err
	}
	if d.Identities, err = s.d.Users.ListIdentities(ctx, u.Handle); err != nil {
		return nil, err
	}
	d.CanLink = !s.o.LocalAccounts || len(d.Identities) > 0
	if d.Sessions, err = s.d.Grants.CountSessions(ctx, u.Handle); err != nil {
		return nil, err
	}

	owned, err := s.d.Artifacts.List(ctx, c.actor, artifact.ListQuery{All: true, Owner: u.Handle, Limit: peoplePerPage})
	if err != nil {
		return nil, err
	}
	d.MorePages = owned.NextCursor != ""
	for i := range owned.Items {
		a := &owned.Items[i]
		row := s.pageRow(a)
		d.Pages = append(d.Pages, peopleOwnedPage{Slug: a.Slug, Name: a.Name, Open: row.Open, State: row.State, Updated: a.UpdatedAt})
	}
	if d.Owns = len(d.Pages) > 0; !d.Owns {
		binned, err := s.d.Artifacts.List(ctx, c.actor, artifact.ListQuery{All: true, Owner: u.Handle, Deleted: "only", Limit: 1})
		if err != nil {
			return nil, err
		}
		d.Owns = len(binned.Items) > 0
	}
	events, err := s.d.Audit.List(ctx, audit.Query{Actor: u.Handle, Limit: 10})
	if err != nil {
		return nil, err
	}
	d.Events = events.Events
	return d, nil
}

// peopleName is the name a person is called by on a page about them.
func peopleName(u *user.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Handle
}

// peopleOthers are the people who can take over handle's pages: everyone
// else who is not disabled.
func (s *server) peopleOthers(ctx context.Context, handle string) ([]user.User, error) {
	users, err := s.d.Users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	others := users[:0]
	for _, o := range users {
		if o.Handle != handle && o.DisabledAt == nil {
			others = append(others, o)
		}
	}
	return others, nil
}

// peopleEdit changes a person's name, email, role and quota. Only what the
// form changed is sent on, so the activity log says what was changed and a
// form that leaves the role alone is no demotion.
func (s *server) peopleEdit(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := peopleForm{
		Name: strings.TrimSpace(r.PostForm.Get("name")), Email: strings.TrimSpace(r.PostForm.Get("email")),
		Role: r.PostForm.Get("role"), Quota: strings.TrimSpace(r.PostForm.Get("quota")), Errors: map[string]string{},
	}
	var req user.UpdateUserRequest
	if f.Name != u.DisplayName {
		req.DisplayName = &f.Name
	}
	if !strings.EqualFold(f.Email, u.Email) {
		req.Email = &f.Email
	}
	if f.Role != string(u.Role) {
		role := user.Role(f.Role)
		req.Role = &role
	}
	// The field shows a rounded size: unless it was edited, the exact quota
	// stays.
	if f.Quota != peopleQuotaText(u.QuotaBytes) {
		n, problem := peopleParseSize(f.Quota)
		if problem != "" {
			f.Errors["quota"] = problem
		}
		req.QuotaBytes = &n
	}
	if len(f.Errors) == 0 {
		if req == (user.UpdateUserRequest{}) {
			return s.done(w, r, peoplePath(u.Handle), "people-saved")
		}
		_, err := s.d.Users.UpdateUser(ctx, u.Handle, req)
		if err == nil {
			return s.done(w, r, peoplePath(u.Handle), "people-saved")
		}
		field, message := peopleFieldError(err, u.Handle)
		if field == "" {
			return err
		}
		f.Errors[field] = message
	}
	d, err := s.peoplePersonData(ctx, c, u)
	if err != nil {
		return err
	}
	d.Form = f
	return s.render(w, r, c, http.StatusUnprocessableEntity, "people-person", peoplePage(d.Row.Name), d)
}

// peopleHandOverField is the form field that names who gets a person's pages.
const peopleHandOverField = "transfer_to"

func (s *server) peopleDisableConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	if u.Handle == c.me.Handle {
		return user.ErrSelf
	}
	if u.DisabledAt != nil {
		return refused("%s is disabled already.", u.Handle)
	}
	others, err := s.peopleOthers(r.Context(), u.Handle)
	if err != nil {
		return err
	}
	sel := &confirmSelect{
		Name: peopleHandOverField, Label: "Their pages",
		Help:    "Their pages stay online either way. Handing them over gives them an owner who can still sign in.",
		Options: []confirmOption{{Value: "", Label: "Leave their pages with them", Selected: true}},
	}
	for i := range others {
		o := &others[i]
		sel.Options = append(sel.Options, confirmOption{Value: o.Handle, Label: "Hand them all to " + peopleName(o) + " (" + o.Handle + ")"})
	}
	return s.confirm(w, r, c, confirmation{
		Handle: u.Handle,
		Title:  "Disable " + peopleName(u),
		Lede: "Disabling is how a person leaves this instance. There is no way to delete a person: " +
			"their name stays on what they published and in the activity log.",
		EndsTitle: "What ends for good",
		Ends: []string{
			"Every API key of theirs is revoked.",
			"Every connected app of theirs is disconnected, and they are signed out everywhere.",
			"Every share link they created stops working.",
			"Their invite and reset links stop working.",
		},
		StaysTitle: "What stays",
		Stays: []string{
			"Their pages, online and unchanged.",
			"Their account, which you can enable again. Enabling restores none of the above: they need new keys and new links.",
		},
		Select: sel,
		Action: peoplePath(u.Handle) + "/disable", Button: "Disable " + u.Handle, Danger: true, Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleDisable(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	to := r.PostForm.Get(peopleHandOverField)
	if to != "" {
		if err := s.checkNewOwner(ctx, u.Handle, to); err != nil {
			return err
		}
	}
	// Disabled first: the guards on the last admin and on oneself refuse
	// here, before any page has moved.
	disabled := true
	if _, err := s.d.Users.UpdateUser(ctx, u.Handle, user.UpdateUserRequest{Disabled: &disabled}); err != nil {
		return err
	}
	if to == "" {
		return s.done(w, r, peoplePath(u.Handle), "people-disabled")
	}
	if _, err := s.d.Artifacts.TransferAll(ctx, c.actor, u.Handle, to); err != nil {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-disabled-moved")
}

// checkNewOwner refuses a hand-over to anyone who could not look after the
// pages: nobody, the same person, or someone disabled.
func (s *server) checkNewOwner(ctx context.Context, from, to string) error {
	if to == from {
		return refused("Choose another person to hand the pages to.")
	}
	n, err := s.d.Users.GetUser(ctx, to)
	if errors.Is(err, user.ErrNotFound) {
		return refused("There is no person with the handle %s to hand the pages to.", to)
	}
	if err != nil {
		return err
	}
	if n.DisabledAt != nil {
		return refused("%s is disabled. Hand the pages to someone who can sign in.", to)
	}
	return nil
}

func (s *server) peopleEnable(w http.ResponseWriter, r *http.Request, _ *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	disabled := false
	if _, err := s.d.Users.UpdateUser(r.Context(), u.Handle, user.UpdateUserRequest{Disabled: &disabled}); err != nil {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-enabled")
}

// peopleHandOverConfirm asks who gets every page a person owns, and says
// what moving them does, before any of them moves.
func (s *server) peopleHandOverConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	others, err := s.peopleOthers(r.Context(), u.Handle)
	if err != nil {
		return err
	}
	if len(others) == 0 {
		return refused("There is nobody else to hand the pages to. Invite or enable a person first.")
	}
	sel := &confirmSelect{Name: peopleHandOverField, Label: "New owner", Required: true,
		Options: []confirmOption{{Value: "", Label: "Choose a person", Selected: true}}}
	for i := range others {
		o := &others[i]
		sel.Options = append(sel.Options, confirmOption{Value: o.Handle, Label: peopleName(o) + " (" + o.Handle + ")"})
	}
	return s.confirm(w, r, c, confirmation{
		Handle:    u.Handle,
		Title:     "Hand over the pages of " + peopleName(u),
		Lede:      "Every page " + u.Handle + " owns gets the owner you choose, in one step.",
		EndsTitle: "What changes",
		Ends: []string{
			"Every page they own moves, those in the bin too.",
			"The pages count against the new owner's quota from then on.",
			"They can be handed back only one at a time, each from its own page.",
		},
		StaysTitle: "What stays",
		Stays: []string{
			"The pages stay online at their addresses, with their versions and share links.",
			u.Handle + " keeps their account and whatever they could do before.",
		},
		Select: sel,
		Action: peoplePath(u.Handle) + "/handover", Button: "Hand over their pages", Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleHandOver(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	to := r.PostForm.Get(peopleHandOverField)
	if to == "" {
		return refused("Choose who to hand the pages to.")
	}
	if err := s.checkNewOwner(ctx, u.Handle, to); err != nil {
		return err
	}
	moved, err := s.d.Artifacts.TransferAll(ctx, c.actor, u.Handle, to)
	if err != nil {
		return err
	}
	if len(moved) == 0 {
		return s.done(w, r, peoplePath(u.Handle), "people-nothing-moved")
	}
	return s.done(w, r, peoplePath(u.Handle), "people-handed-over")
}

func (s *server) peopleSessionsEnd(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	// An admin who signs themselves out everywhere keeps the session they
	// are doing it in.
	if u.Handle == c.me.Handle {
		if err := s.d.Grants.EndSessions(r.Context(), u.Handle, c.sess.SessionID); err != nil {
			return err
		}
		return s.done(w, r, peoplePath(u.Handle), "people-signed-out-me")
	}
	if err := s.d.Grants.EndSessions(r.Context(), u.Handle, 0); err != nil {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-signed-out")
}

func (s *server) peopleIdentityLink(w http.ResponseWriter, r *http.Request, _ *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	f := r.PostForm
	err = s.d.Users.LinkIdentity(r.Context(), u.Handle, f.Get("issuer"), f.Get("subject"), f.Get("email"))
	if errors.Is(err, user.ErrInvalidIdentity) {
		return refused("Enter the issuer and the subject as the provider sends them.")
	}
	if err != nil {
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-linked")
}

// peopleIdentityUnlinkConfirm shows the sign-in about to be unlinked. Its
// issuer and subject are what links it again, and after the unlink no page
// shows them.
func (s *server) peopleIdentityUnlinkConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	issuer, subject := r.URL.Query().Get("issuer"), r.URL.Query().Get("subject")
	ids, err := s.d.Users.ListIdentities(r.Context(), u.Handle)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(ids, func(id user.Identity) bool { return id.Issuer == issuer && id.Subject == subject }) {
		return notFound("That sign-in is not linked to " + u.Handle + ".")
	}
	return s.confirm(w, r, c, confirmation{
		Handle:    u.Handle,
		Title:     "Unlink a sign-in of " + peopleName(u),
		Lede:      "Signing in at the provider with this account will no longer open " + u.Handle + ".",
		EndsTitle: "What is unlinked",
		Ends: []string{
			"Issuer: " + issuer,
			"Subject: " + subject,
			"Note both down if the sign-in may be linked again: after this, no page shows them.",
		},
		StaysTitle: "What stays",
		Stays: []string{
			"Their sessions, keys and connected apps, until they end or are ended.",
			"Any other way they sign in.",
		},
		Hidden: map[string]string{"issuer": issuer, "subject": subject},
		Action: peoplePath(u.Handle) + "/identities/unlink", Button: "Unlink the sign-in", Danger: true, Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleIdentityUnlink(w http.ResponseWriter, r *http.Request, _ *request) error {
	u, err := s.peopleUser(r)
	if err != nil {
		return err
	}
	f := r.PostForm
	err = s.d.Users.UnlinkIdentity(r.Context(), u.Handle, f.Get("issuer"), f.Get("subject"))
	switch {
	case errors.Is(err, user.ErrInvalidIdentity), errors.Is(err, user.ErrNotFound):
		return notFound("That sign-in is not linked to " + u.Handle + ".")
	case err != nil:
		return err
	}
	return s.done(w, r, peoplePath(u.Handle), "people-unlinked")
}

// peopleScopeNames says what each scope allows, beside its name in mono.
var peopleScopeNames = map[string]string{
	auth.ScopeRead:   "List and read pages",
	auth.ScopeWrite:  "Create and update pages",
	auth.ScopeShare:  "Change who can open pages, and create share links",
	auth.ScopeDelete: "Delete pages",
	auth.ScopeAdmin:  "Administer the instance: the admin API and this dashboard",
}
