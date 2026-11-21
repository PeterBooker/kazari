package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/user"
)

// Invites and reset links. Each is shown once, on the page that answers
// the POST that made it; Kazari sends no email, so the admin copies the
// link and hands it over.

// peopleInviteData is the form that adds a person, as it was sent.
type peopleInviteData struct {
	Handle, Name, Email, Role, Quota string
	Errors                           map[string]string
	Local                            bool // built-in accounts: the person gets an invite link
	DefaultQuota                     int64
}

func (s *server) peopleInviteTitle() string {
	if s.o.LocalAccounts {
		return "Invite a person"
	}
	return "Add a person"
}

func (s *server) peopleInviteForm(w http.ResponseWriter, r *http.Request, c *request) error {
	d := peopleInviteData{Role: string(user.RoleMember), Local: s.o.LocalAccounts, DefaultQuota: s.o.DefaultQuota}
	return s.render(w, r, c, http.StatusOK, "people-invite", peoplePage(s.peopleInviteTitle()), d)
}

func (s *server) peopleInvite(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	f := r.PostForm
	d := peopleInviteData{
		Handle: strings.ToLower(strings.TrimSpace(f.Get("handle"))), Name: strings.TrimSpace(f.Get("name")),
		Email: strings.TrimSpace(f.Get("email")), Role: f.Get("role"), Quota: strings.TrimSpace(f.Get("quota")),
		Errors: map[string]string{}, Local: s.o.LocalAccounts, DefaultQuota: s.o.DefaultQuota,
	}
	req := user.InviteRequest{Handle: d.Handle, Email: d.Email, DisplayName: d.Name, Role: user.Role(d.Role)}
	if d.Quota != "" {
		n, problem := peopleParseSize(d.Quota)
		if problem != "" {
			d.Errors["quota"] = problem
		}
		req.QuotaBytes = &n
	}
	if d.Handle == "" {
		d.Errors["handle"] = "Choose a handle: the short name they sign in and are listed with."
	}
	if len(d.Errors) == 0 {
		var (
			u     *user.User
			link  *user.Link
			token string
			err   error
		)
		if s.o.LocalAccounts {
			u, link, token, err = s.d.Users.Invite(ctx, req)
		} else {
			u, err = s.d.Users.CreateUser(ctx, user.CreateUserRequest(req))
		}
		switch field, message := peopleFieldError(err, d.Handle); {
		case err == nil && link == nil:
			return s.done(w, r, peoplePath(u.Handle), "people-added")
		case err == nil:
			return s.peopleShowLink(w, r, c, u, link, token)
		case field == "":
			return err
		default:
			d.Errors[field] = message
		}
	}
	return s.render(w, r, c, http.StatusUnprocessableEntity, "people-invite", peoplePage(s.peopleInviteTitle()), d)
}

// peopleShowLink shows a new link, once.
func (s *server) peopleShowLink(w http.ResponseWriter, r *http.Request, c *request, u *user.User, l *user.Link, token string) error {
	name := u.DisplayName
	if name == "" {
		name = u.Handle
	}
	d := peopleOnce{
		Handle: u.Handle, Name: name,
		Value:  s.d.Links.AccountLinkURL(l.Purpose, token),
		Button: "Copy the link",
		Notes: []string{
			"It works once, and stops working on " + l.ExpiresAt.UTC().Format(timeLayout) + ".",
			"Opening it changes nothing, so a chat application may preview it. Setting a password with it uses it up.",
			"Kazari keeps no copy. After you leave this page it cannot be shown again, only replaced.",
		},
	}
	if l.Purpose == user.PurposeReset {
		d.Title, d.Label = "Reset link for "+u.Handle, "Reset link"
		d.Lede = "Copy the link now and send it to " + name + " over a channel you trust: Kazari sends no email. " +
			"It lets whoever opens it set their password."
		if l.RevokeAll {
			d.Notes = append(d.Notes, "When it is used they are signed out everywhere, their apps are disconnected and their keys revoked.")
		} else {
			d.Notes = append(d.Notes, "When it is used they are signed out everywhere. Their apps and keys stay.")
		}
	} else {
		d.Title, d.Label = "Invite link for "+u.Handle, "Invite link"
		d.Lede = "Copy the link now and send it to " + name + " over a channel you trust: Kazari sends no email. " +
			"It lets whoever opens it set the password of this account."
		d.Notes = append(d.Notes, "Until they use it, "+u.Handle+" is listed as invited and cannot sign in.")
	}
	return s.peopleShowOnce(w, r, c, d)
}

// peopleLocalUser loads the person a path names on an instance with
// built-in accounts, where links exist.
func (s *server) peopleLocalUser(r *http.Request) (*user.User, error) {
	if !s.o.LocalAccounts {
		return nil, notFound("People sign in at the identity provider on this instance, so there are no links to make.")
	}
	return s.peopleUser(r)
}

// hasPassword reports whether the person has set a password.
func (s *server) peopleHasPassword(r *http.Request, handle string) (bool, error) {
	accounts, err := s.d.Users.Accounts(r.Context())
	if err != nil {
		return false, err
	}
	for _, a := range accounts {
		if a.Handle == handle {
			return a.HasPassword, nil
		}
	}
	return false, nil
}

func (s *server) peopleReplaceConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleLocalUser(r)
	if err != nil {
		return err
	}
	if err := s.peopleInvitable(r, u); err != nil {
		return err
	}
	return s.peopleConfirmPage(w, r, c, peopleConfirm{
		Handle: u.Handle, Name: u.Handle,
		Title:     "New invite link for " + u.Handle,
		Lede:      "For an invite that was lost or has lapsed. The new link is shown once on the next page.",
		EndsTitle: "What ends for good",
		Ends:      []string{"Any invite link made for them before: only the new one works."},
		Action:    peoplePath(u.Handle) + "/invite", Button: "Make a new invite link", Cancel: peoplePath(u.Handle),
	})
}

// peopleInvitable refuses an invite link for someone it is not for.
func (s *server) peopleInvitable(r *http.Request, u *user.User) error {
	if u.DisabledAt != nil {
		return refused("%s is disabled. Enable them first.", u.Handle)
	}
	has, err := s.peopleHasPassword(r, u.Handle)
	if err != nil {
		return err
	}
	if has {
		return refused("%s has set a password already. Create a reset link instead.", u.Handle)
	}
	return nil
}

func (s *server) peopleReplace(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleLocalUser(r)
	if err != nil {
		return err
	}
	if err := s.peopleInvitable(r, u); err != nil {
		return err
	}
	l, token, err := s.d.Users.IssueLink(r.Context(), u.Handle, user.PurposeInvite, false)
	if err != nil {
		return err
	}
	return s.peopleShowLink(w, r, c, u, l, token)
}

// peopleRevokeAllField is the option of a reset link that also ends the
// person's apps and keys.
const peopleRevokeAllField = "revoke_all"

func (s *server) peopleResetConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleLocalUser(r)
	if err != nil {
		return err
	}
	if u.DisabledAt != nil {
		return refused("%s is disabled. Enable them first.", u.Handle)
	}
	return s.peopleConfirmPage(w, r, c, peopleConfirm{
		Handle: u.Handle, Name: u.Handle,
		Title: "Reset link for " + u.Handle,
		Lede: "For a person who lost their password, or who signed in with a key until now and has none. " +
			"The link is shown once on the next page; nothing changes for them until they use it.",
		EndsTitle: "What ends when the link is used",
		Ends: []string{
			"Their old password.",
			"Every browser session of theirs: they sign in again with the new password.",
		},
		StaysTitle: "What ends now",
		Stays:      []string{"Any reset link made for them before: only the new one works."},
		Check: &peopleCheck{Name: peopleRevokeAllField, Label: "Also disconnect their apps and revoke their keys",
			Help: "For an account someone else may have got into. Leave it off for a forgotten password."},
		Action: peoplePath(u.Handle) + "/reset", Button: "Create the reset link", Cancel: peoplePath(u.Handle),
	})
}

func (s *server) peopleReset(w http.ResponseWriter, r *http.Request, c *request) error {
	u, err := s.peopleLocalUser(r)
	if err != nil {
		return err
	}
	l, token, err := s.d.Users.IssueLink(r.Context(), u.Handle, user.PurposeReset, r.PostForm.Get(peopleRevokeAllField) != "")
	if errors.Is(err, user.ErrDisabled) {
		return refused("%s is disabled. Enable them first.", u.Handle)
	}
	if err != nil {
		return err
	}
	return s.peopleShowLink(w, r, c, u, l, token)
}

// peopleLinkOf loads the live link a path names, or the 404 page.
func (s *server) peopleLinkOf(r *http.Request) (*peopleLink, error) {
	gone := notFound("That link is not waiting to be used. It may have been used, replaced or revoked, or it has expired.")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, gone
	}
	links, err := s.peopleLinks(r.Context())
	if err != nil {
		return nil, err
	}
	for i := range links {
		if links[i].ID == id {
			return &links[i], nil
		}
	}
	return nil, gone
}

func (s *server) peopleLinkRevokeConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	l, err := s.peopleLinkOf(r)
	if err != nil {
		return err
	}
	d := peopleConfirm{
		Handle: l.Handle, Name: l.Handle,
		Title:      "Revoke the " + peopleLinkName(l.Purpose) + " for " + l.Handle,
		Lede:       "For a link that went to the wrong place. A revoked link cannot be brought back, only replaced with a new one.",
		EndsTitle:  "What ends for good",
		Ends:       []string{"The link: whoever opens it is told it is no longer valid."},
		StaysTitle: "What stays",
		Action:     "/invites/" + strconv.FormatInt(l.ID, 10) + "/revoke", Button: "Revoke the link", Danger: true, Cancel: "/people",
	}
	if l.Purpose == user.PurposeInvite {
		d.Stays = []string{l.Handle + " stays listed as invited, without a way to sign in until you make a new link."}
	} else {
		d.Stays = []string{l.Handle + " keeps their account, their password if they have one, their keys and their apps."}
	}
	return s.peopleConfirmPage(w, r, c, d)
}

func (s *server) peopleLinkRevoke(w http.ResponseWriter, r *http.Request, _ *request) error {
	l, err := s.peopleLinkOf(r)
	if err != nil {
		return err
	}
	err = s.d.Users.RevokeLink(r.Context(), l.ID)
	if errors.Is(err, user.ErrLinkNotFound) {
		return notFound("That link is not waiting to be used any more.")
	}
	if err != nil {
		return err
	}
	return s.done(w, r, "/people", "people-link-revoked")
}
