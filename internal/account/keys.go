package account

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/user"
)

// The keys of the account page. A key issued here is bounded by how the
// person signed in: with a password or at the identity provider, by the
// member scopes, whatever their role; with an API key, by that key. So a
// stolen session mints no admin key and widens no restricted one. The
// handle is always the session's, never the form's.

// newKeyPage shows a key's secret, once.
type newKeyPage struct {
	base
	Key    *user.APIKey
	Secret string
	// Old is the key a rotation replaced, as it now is.
	Old *user.APIKey
	// OldLive is whether Old still works: a key rotated "at once" ended
	// with the rotation.
	OldLive        bool
	MCPURL, APIURL string
}

func (s *server) showKey(w http.ResponseWriter, r *http.Request, v visit, k *user.APIKey, secret string, old *user.APIKey) {
	s.render(w, r, http.StatusOK, "key", newKeyPage{
		base: s.baseFor(v), Key: k, Secret: secret, Old: old,
		OldLive: old != nil && old.ExpiresAt != nil && old.ExpiresAt.After(s.now()),
		// The command line takes the API's origin and adds the version
		// itself.
		MCPURL: s.o.MCPURL, APIURL: strings.TrimSuffix(strings.TrimSuffix(s.o.APIURL, "/"), "/v1"),
	})
}

// maxKeyDays bounds the expiry field where the instance sets no limit, as
// the dashboard's key form does: ten years, past which "no expiry" is the
// honest choice.
const maxKeyDays = 3650

func (s *server) issueKey(w http.ResponseWriter, r *http.Request, v visit) {
	f := r.PostForm
	form := keyForm{Label: strings.TrimSpace(f.Get("label")), Slugs: f.Get("slugs"), Days: strings.TrimSpace(f.Get("days"))}
	refuse := func(status int, field, why string) {
		s.renderAccount(w, r, status, v, accountPageData{Err: map[string]string{field: why}, Key: form})
	}
	var ttl time.Duration
	if form.Days != "" {
		days, err := strconv.Atoi(form.Days)
		limit := maxKeyDays
		if s.o.MaxKeyTTL > 0 {
			limit = int(s.o.MaxKeyTTL / (24 * time.Hour))
		}
		if err != nil || days < 1 || days > limit {
			refuse(http.StatusUnprocessableEntity, "days", "range")
			return
		}
		ttl = time.Duration(days) * 24 * time.Hour
	}
	k, secret, err := s.d.Users.IssueKey(r.Context(), user.IssueKeyRequest{
		Handle:    v.user.Handle,
		Label:     form.Label,
		Slugs:     strings.FieldsFunc(form.Slugs, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' || c == '\t' }),
		ExpiresIn: ttl,
		With:      v.sess.KeyPrefix,
		Session:   true,
	})
	switch {
	case errors.Is(err, user.ErrInvalidKey):
		refuse(http.StatusUnprocessableEntity, "slugs", "invalid")
	case errors.Is(err, user.ErrKeyTooWide):
		refuse(http.StatusForbidden, "key", "too-wide")
	case errors.Is(err, user.ErrTooManyKeys):
		refuse(http.StatusForbidden, "key", "too-many")
	case errors.Is(err, user.ErrInvalidScope):
		refuse(http.StatusUnprocessableEntity, "key", "scope")
	case err != nil:
		s.fail(w, r, "issue key", err)
	default:
		s.showKey(w, r, v, k, secret, nil)
	}
}

// ownKey returns the person's key the path names with the rest of their
// keys. Another person's prefix, like one that does not exist, is not
// found.
func (s *server) ownKey(w http.ResponseWriter, r *http.Request, v visit) (*keyRow, []keyRow, bool) {
	keys, err := s.keys(r.Context(), v.user.Handle)
	if err != nil {
		s.fail(w, r, "list keys", err)
		return nil, nil, false
	}
	prefix := r.PathValue("prefix")
	for i := range keys {
		if keys[i].Prefix == prefix {
			return &keys[i], keys, true
		}
	}
	s.message(w, r, http.StatusNotFound, "not-found")
	return nil, nil, false
}

// rotateData is the page that asks how long a rotated key keeps working.
type rotateData struct {
	base
	Key *keyRow
	Err string
}

func (s *server) rotatePage(w http.ResponseWriter, r *http.Request, v visit) {
	k, _, ok := s.ownKey(w, r, v)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "rotate", rotateData{base: s.baseFor(v), Key: k})
}

// overlaps are the choices of the rotate form.
var overlaps = map[string]time.Duration{"now": 0, "hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour}

func (s *server) rotateKey(w http.ResponseWriter, r *http.Request, v visit) {
	k, _, ok := s.ownKey(w, r, v)
	if !ok {
		return
	}
	overlap, known := overlaps[r.PostForm.Get("overlap")]
	if !known {
		overlap = user.DefaultRotateOverlap
	}
	refuse := func(status int, why string) {
		s.render(w, r, status, "rotate", rotateData{base: s.baseFor(v), Key: k, Err: why})
	}
	next, secret, old, err := s.d.Users.RotateKey(r.Context(),
		user.KeyRef{Handle: v.user.Handle, Prefix: k.Prefix, With: v.sess.KeyPrefix, Session: true}, overlap)
	switch {
	case errors.Is(err, user.ErrKeyNotFound):
		// Revoked or expired since the page was opened.
		refuse(http.StatusConflict, "ended")
	case errors.Is(err, user.ErrKeyTooWide):
		refuse(http.StatusForbidden, "too-wide")
	case errors.Is(err, user.ErrTooManyKeys):
		refuse(http.StatusForbidden, "too-many")
	case err != nil:
		s.fail(w, r, "rotate key", err)
	default:
		s.showKey(w, r, v, next, secret, old)
	}
}

func (s *server) revokeKeyPage(w http.ResponseWriter, r *http.Request, v visit) {
	k, keys, ok := s.ownKey(w, r, v)
	if !ok {
		return
	}
	d := confirmPage{base: s.baseFor(v), Kind: "key", Key: k, Own: k.Prefix == v.sess.KeyPrefix}
	for _, other := range keys {
		if other.Live && other.IssuedBy == k.Prefix {
			d.Issued = append(d.Issued, other)
		}
	}
	apps, err := s.apps(r.Context(), v.user.Handle)
	if err != nil {
		s.fail(w, r, "list grants", err)
		return
	}
	for _, a := range apps {
		if a.KeyPrefix == k.Prefix {
			d.Apps = append(d.Apps, a)
		}
	}
	s.render(w, r, http.StatusOK, "confirm", d)
}

func (s *server) revokeKey(w http.ResponseWriter, r *http.Request, v visit) {
	prefix := r.PathValue("prefix")
	err := s.d.Users.RevokeKey(r.Context(), user.KeyRef{Handle: v.user.Handle, Prefix: prefix, Session: true})
	if errors.Is(err, user.ErrKeyNotFound) {
		s.message(w, r, http.StatusNotFound, "not-found")
		return
	}
	if err != nil {
		s.fail(w, r, "revoke key", err)
		return
	}
	if prefix == v.sess.KeyPrefix {
		// The session went with its key. Answered here: a redirect would end
		// on the sign-in host, which this form's policy does not allow.
		if err := s.sess.End(w, r); err != nil {
			s.fail(w, r, "sign out", err)
			return
		}
		s.message(w, r, http.StatusOK, "signed-out-key")
		return
	}
	s.done(w, r, "key-revoked")
}
