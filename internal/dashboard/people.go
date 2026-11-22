package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/user"
)

var peopleFlashes = map[string]string{
	"people-saved":          "The changes are saved.",
	"people-added":          "The person is added. They sign in at the identity provider, or with a key you issue below.",
	"people-disabled":       "The person is disabled. Their keys, connected apps, sessions and share links have ended.",
	"people-disabled-moved": "The person is disabled and their pages are handed over. Their keys, connected apps, sessions and share links have ended.",
	"people-enabled":        "The person can sign in again. What ended when they were disabled stays ended.",
	"people-handed-over":    "Their pages are handed over.",
	"people-nothing-moved":  "They own no pages, so nothing was handed over.",
	"people-key-revoked":    "The key is revoked, with every key issued with it.",
	"people-app-revoked":    "The app is disconnected.",
	"people-link-revoked":   "The link no longer works.",
	"people-signed-out":     "Their browser sessions have ended. Connected apps keep working.",
	"people-signed-out-me":  "Your other browser sessions have ended. This one stays.",
	"people-linked":         "The provider sign-in is linked.",
	"people-unlinked":       "The provider sign-in is unlinked.",
}

const peoplePerPage = 50

// peoplePage is what every People page says about itself.
func peoplePage(title string) page { return page{Title: title, Nav: "people"} }

func (s *server) peopleRoutes(mux *http.ServeMux) {
	s.view(mux, "GET /people", s.peopleList)

	// Under /invites and not /people: "invite" and "links" are handles a
	// person may hold, and their page must not be shadowed.
	s.view(mux, "GET /invites/new", s.peopleInviteForm)
	s.issue(mux, "POST /invites/new", s.peopleInvite)
	s.view(mux, "GET /invites/{id}/revoke", s.peopleLinkRevokeConfirm)
	s.action(mux, "POST /invites/{id}/revoke", s.peopleLinkRevoke)

	s.view(mux, "GET /people/{handle}", s.peoplePerson)
	s.action(mux, "POST /people/{handle}", s.peopleEdit)
	s.view(mux, "GET /people/{handle}/disable", s.peopleDisableConfirm)
	s.action(mux, "POST /people/{handle}/disable", s.peopleDisable)
	s.action(mux, "POST /people/{handle}/enable", s.peopleEnable)
	s.view(mux, "GET /people/{handle}/handover", s.peopleHandOverConfirm)
	s.action(mux, "POST /people/{handle}/handover", s.peopleHandOver)
	s.action(mux, "POST /people/{handle}/sessions/end", s.peopleSessionsEnd)
	s.action(mux, "POST /people/{handle}/identities", s.peopleIdentityLink)
	s.view(mux, "GET /people/{handle}/identities/unlink", s.peopleIdentityUnlinkConfirm)
	s.action(mux, "POST /people/{handle}/identities/unlink", s.peopleIdentityUnlink)

	s.view(mux, "GET /people/{handle}/invite", s.peopleReplaceConfirm)
	s.issue(mux, "POST /people/{handle}/invite", s.peopleReplace)
	s.view(mux, "GET /people/{handle}/reset", s.peopleResetConfirm)
	s.issue(mux, "POST /people/{handle}/reset", s.peopleReset)

	s.issue(mux, "POST /people/{handle}/keys", s.peopleKeyIssue)
	s.view(mux, "GET /people/{handle}/keys/{prefix}/rotate", s.peopleKeyRotateConfirm)
	s.issue(mux, "POST /people/{handle}/keys/{prefix}/rotate", s.peopleKeyRotate)
	s.view(mux, "GET /people/{handle}/keys/{prefix}/revoke", s.peopleKeyRevokeConfirm)
	s.action(mux, "POST /people/{handle}/keys/{prefix}/revoke", s.peopleKeyRevoke)
	s.view(mux, "GET /people/{handle}/apps/{id}/revoke", s.peopleAppRevokeConfirm)
	s.action(mux, "POST /people/{handle}/apps/{id}/revoke", s.peopleAppRevoke)
}

// Statuses of a person, in the order the list checks them.
const (
	peopleActive   = "active"
	peopleInvited  = "invited"
	peopleDisabled = "disabled"
)

// peopleRow is a person as the list and the head of their page show them.
type peopleRow struct {
	Handle     string
	Name       string // the display name, or the handle when there is none
	Email      string
	Role       user.Role
	Status     string // peopleActive, peopleInvited or peopleDisabled
	Methods    []string
	Used       int64
	Quota      int64 // 0 is no limit
	Pages      int64
	LastActive *time.Time
}

// Tone is the theme's tone of the status dot.
func (p peopleRow) Tone() string {
	switch p.Status {
	case peopleDisabled:
		return "tone-danger"
	case peopleInvited:
		return "tone-warn"
	}
	return "tone-ok"
}

// StatusWord is the status as a person reads it.
func (p peopleRow) StatusWord() string {
	return strings.ToUpper(p.Status[:1]) + p.Status[1:]
}

// Percent is the usage of the quota from 0 to 100.
func (p peopleRow) Percent() int { return percent(p.Used, p.Quota) }

func (p peopleRow) Admin() bool { return p.Role == user.RoleAdmin }

func newPeopleRow(u *user.User, acc user.Account, own artifact.OwnerUsage) peopleRow {
	r := peopleRow{
		Handle: u.Handle, Name: u.DisplayName, Email: u.Email, Role: u.Role,
		Status: peopleActive, Used: own.Bytes, Quota: u.QuotaBytes, Pages: own.Artifacts, LastActive: acc.LastActive(),
	}
	if r.Name == "" {
		r.Name = u.Handle
	}
	switch {
	case u.DisabledAt != nil:
		r.Status = peopleDisabled
	case acc.Invited:
		r.Status = peopleInvited
	}
	if acc.HasPassword {
		r.Methods = append(r.Methods, "password")
	}
	if acc.Passkeys > 0 {
		r.Methods = append(r.Methods, "passkey")
	}
	if acc.Identities > 0 {
		r.Methods = append(r.Methods, "provider")
	}
	if acc.LiveKeys > 0 {
		r.Methods = append(r.Methods, "keys")
	}
	return r
}

// peopleRows returns every person as a row, ordered by handle, in three
// queries however many people there are.
func (s *server) peopleRows(ctx context.Context) ([]peopleRow, error) {
	users, err := s.d.Users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.d.Users.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	owned, err := s.d.Artifacts.OwnerUsage(ctx)
	if err != nil {
		return nil, err
	}
	byHandle := make(map[string]user.Account, len(accounts))
	for _, a := range accounts {
		byHandle[a.Handle] = a
	}
	usage := make(map[string]artifact.OwnerUsage, len(owned))
	for _, o := range owned {
		usage[o.Handle] = o
	}
	rows := make([]peopleRow, 0, len(users))
	for i := range users {
		rows = append(rows, newPeopleRow(&users[i], byHandle[users[i].Handle], usage[users[i].Handle]))
	}
	return rows, nil
}

// peopleLink is an invite or reset link that can still be used.
type peopleLink struct {
	ID        int64
	Handle    string
	Purpose   user.Purpose
	CreatedBy string
	ExpiresAt time.Time
}

// Replace is the confirmation page that makes a new link of the same
// purpose.
func (l peopleLink) Replace() string {
	return "/people/" + url.PathEscape(l.Handle) + "/" + peopleLinkPath(l.Purpose)
}

func peopleLinkPath(p user.Purpose) string {
	if p == user.PurposeReset {
		return "reset"
	}
	return "invite"
}

// What a link of a purpose is called.
func peopleLinkName(p user.Purpose) string {
	if p == user.PurposeReset {
		return "reset link"
	}
	return "invite link"
}

func (s *server) peopleLinks(ctx context.Context) ([]peopleLink, error) {
	if !s.o.LocalAccounts {
		return nil, nil
	}
	links, err := s.d.Users.Links(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]peopleLink, 0, len(links))
	for _, l := range links {
		// A setup link names nobody, and is dead once anyone can open this
		// page.
		if l.Purpose == user.PurposeSetup {
			continue
		}
		out = append(out, peopleLink{ID: l.ID, Handle: l.UserHandle, Purpose: l.Purpose, CreatedBy: l.CreatedBy, ExpiresAt: l.ExpiresAt})
	}
	return out, nil
}

type peopleListData struct {
	Rows            []peopleRow
	Total           int // people on the instance
	Matched         int // people the filters keep
	From, To        int // the rows shown, counted from 1
	Q, Role, Status string
	Filtered        bool
	Prev, Next      string
	Links           []peopleLink
	Local           bool // built-in accounts: people are invited
}

func (s *server) peopleList(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	rows, err := s.peopleRows(ctx)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	d := peopleListData{
		Total: len(rows), Local: s.o.LocalAccounts,
		Q: strings.TrimSpace(q.Get("q")), Role: q.Get("role"), Status: q.Get("status"),
	}
	if d.Role != string(user.RoleAdmin) && d.Role != string(user.RoleMember) {
		d.Role = ""
	}
	if d.Status != peopleActive && d.Status != peopleInvited && d.Status != peopleDisabled {
		d.Status = ""
	}
	d.Filtered = d.Q != "" || d.Role != "" || d.Status != ""

	needle := strings.ToLower(d.Q)
	kept := rows[:0:0]
	for _, p := range rows {
		if d.Role != "" && string(p.Role) != d.Role || d.Status != "" && p.Status != d.Status {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(p.Name), needle) &&
			!strings.Contains(p.Handle, needle) && !strings.Contains(p.Email, needle) {
			continue
		}
		kept = append(kept, p)
	}
	d.Matched = len(kept)

	n, _ := strconv.Atoi(q.Get("page"))
	pages := max(1, (len(kept)+peoplePerPage-1)/peoplePerPage)
	n = min(max(n, 1), pages)
	start := (n - 1) * peoplePerPage
	end := min(start+peoplePerPage, len(kept))
	d.Rows = kept[start:end]
	if len(d.Rows) > 0 {
		d.From, d.To = start+1, end
	}
	link := func(n int) string {
		v := url.Values{}
		for k, val := range map[string]string{"q": d.Q, "role": d.Role, "status": d.Status} {
			if val != "" {
				v.Set(k, val)
			}
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return "/people"
		}
		return "/people?" + v.Encode()
	}
	if n > 1 {
		d.Prev = link(n - 1)
	}
	if n < pages {
		d.Next = link(n + 1)
	}
	if d.Links, err = s.peopleLinks(ctx); err != nil {
		return err
	}
	return s.render(w, r, c, http.StatusOK, "people", peoplePage("People"), d)
}

// person loads the person a path names, or the 404 page.
func (s *server) peopleUser(r *http.Request) (*user.User, error) {
	u, err := s.d.Users.GetUser(r.Context(), r.PathValue("handle"))
	if errors.Is(err, user.ErrNotFound) {
		return nil, notFound("There is no person with that handle.")
	}
	return u, err
}

func peoplePath(handle string) string { return "/people/" + url.PathEscape(handle) }

// peopleMaxQuota keeps a typed size inside what the disk of any host holds.
const peopleMaxQuota = 1 << 50

// peopleParseSize reads a size as the configuration and the command line
// do, so 500 MB is the same number of bytes wherever it is typed: a whole
// number and a unit, MiB and GiB in powers of 1024, MB and GB in powers of
// 1000. 0 and "no limit" are no limit. A bare number is refused: nobody
// means a quota in bytes. problem is the sentence for the field when the
// text is no size.
func peopleParseSize(text string) (n int64, problem string) {
	text = strings.TrimSpace(text)
	switch strings.ToLower(text) {
	case "0", "none", "no limit", "unlimited":
		return 0, ""
	}
	if _, err := strconv.ParseUint(text, 10, 64); err == nil {
		return 0, "Say the unit: MiB or GiB, as in 500 MiB."
	}
	size, err := config.ParseBytes(text)
	if err != nil {
		return 0, "Enter a whole number and a unit, such as 500 MiB or 2 GiB, or 0 for no limit."
	}
	if size > peopleMaxQuota {
		return 0, "That size is too large."
	}
	return int64(size), ""
}

// peopleFieldError is the sentence shown beside a field for a refusal of the user
// service, and which field it belongs to; "" when err is none of them.
func peopleFieldError(err error, handle string) (field, message string) {
	switch {
	case errors.Is(err, user.ErrInvalidHandle):
		if slices.Contains(user.ReservedHandles, handle) {
			return "handle", "That handle is reserved. Choose another."
		}
		return "handle", fmt.Sprintf("Use %d to %d characters of a to z, 0 to 9, - and _, starting with a letter or a digit.",
			user.MinHandleLen, user.MaxHandleLen)
	case errors.Is(err, user.ErrHandleTaken):
		return "handle", "That handle is taken. Choose another."
	case errors.Is(err, user.ErrEmailTaken):
		return "email", "Another person has that email."
	case errors.Is(err, user.ErrInvalidRole):
		return "role", "Choose member or admin."
	}
	return "", ""
}
