package account

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/present"
)

// A person's own pages, as the REST API lets them change them: every action
// calls the artifact service as the session's principal, with the scope the
// API's route for it takes, so nothing here goes past what their own key
// could do.

func (s *server) pageRoutes(handle func(string, http.HandlerFunc)) {
	handle("GET "+Prefix+"pages/{slug}", s.signedIn(s.pagePage))
	handle("POST "+Prefix+"pages/{slug}/visibility", s.signedIn(s.pageAction(auth.ScopeShare, s.setVisibility)))
	handle("POST "+Prefix+"pages/{slug}/links", s.signedIn(s.once(s.pageAction(auth.ScopeShare, s.createLink))))
	handle("GET "+Prefix+"pages/{slug}/links/{prefix}/revoke", s.signedIn(s.revokeLinkPage))
	handle("POST "+Prefix+"pages/{slug}/links/{prefix}/revoke", s.signedIn(s.pageAction(auth.ScopeShare, s.revokeLink)))
	handle("GET "+Prefix+"pages/{slug}/versions/{n}/publish", s.signedIn(s.publishPage))
	handle("POST "+Prefix+"pages/{slug}/versions/{n}/publish", s.signedIn(s.pageAction(auth.ScopeWrite, s.publishVersion)))
	handle("POST "+Prefix+"pages/{slug}/stale", s.signedIn(s.pageAction(auth.ScopeWrite, s.markStale)))
	handle("POST "+Prefix+"pages/{slug}/keep", s.signedIn(s.pageAction(auth.ScopeWrite, s.keepPage)))
	handle("POST "+Prefix+"pages/{slug}/bin", s.signedIn(s.pageAction(auth.ScopeDelete, s.binPage)))
	handle("POST "+Prefix+"pages/{slug}/restore", s.signedIn(s.pageAction(auth.ScopeWrite, s.restorePage)))
}

func ownPagePath(slug string) string { return Prefix + "pages/" + url.PathEscape(slug) }

// principal is the person as this request acts: set by signedIn.
func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// pageAction runs fn when the session holds scope, as the API's route for
// the same change would ask, and answers a refusal by the service with the
// page that says why.
func (s *server) pageAction(scope string, fn func(http.ResponseWriter, *http.Request, visit, artifact.Actor) error) func(http.ResponseWriter, *http.Request, visit) {
	return func(w http.ResponseWriter, r *http.Request, v visit) {
		p := principal(r)
		if !p.HasScope(scope) {
			s.refusal(w, r, artifact.Errorf(artifact.ErrForbidden, "the key you signed in with may not do this"))
			return
		}
		if err := fn(w, r, v, present.Actor(p)); err != nil {
			s.refusal(w, r, err)
		}
	}
}

// refusal answers an error of the artifact service with its status and a
// sentence for a person; anything else is logged and not shown.
func (s *server) refusal(w http.ResponseWriter, r *http.Request, err error) {
	slug := r.PathValue("slug")
	d := messagePage{base: s.base(), Kind: "refused", Back: ownPagePath(slug)}
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		s.message(w, r, http.StatusNotFound, "page-not-found")
		return
	case errors.Is(err, artifact.ErrGone):
		s.message(w, r, http.StatusGone, "page-gone")
		return
	case errors.Is(err, artifact.ErrVersionNotFound), errors.Is(err, artifact.ErrLinkNotFound):
		d.Text = "That is no longer on the page. Open the page again to see it as it is now."
	case errors.Is(err, artifact.ErrForbidden):
		d.Text = "You may not make this change to the page. Its owner or an admin of this instance can."
	}
	ae, ok := errors.AsType[*artifact.Error](err)
	if !ok {
		s.fail(w, r, "change page", err)
		return
	}
	if d.Text == "" {
		d.Text = sentence(strings.ReplaceAll(ae.Message, "artifact", "page"))
	}
	status, _ := present.Error(err)
	s.render(w, r, status, "message", d)
}

// sentence starts s with a capital and ends it with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// ownVersion is a version as the page lists it.
type ownVersion struct {
	artifact.Version
	URL, Host string
	Published bool
}

// ownLink is a share link as the page lists it.
type ownLink struct {
	artifact.ShareLink
	State string // active, expired or revoked
}

type ownPageData struct {
	base
	Flash string
	Page  *artifact.Artifact
	// State is where the page is in its life: live, stale, unpublished or
	// bin.
	State     string
	URL, Host string
	Versions  []ownVersion
	Links     []ownLink
	// Mine is the person's own page; Changeable whether they may change it
	// at all, which the write policy may allow for another's.
	Mine, Changeable bool
	// May are the scopes of the session, by name: share, write, delete.
	May map[string]bool
	// The share-link form: its limit, and what was typed when it was
	// refused.
	MaxLinkDays, LinkDays int
	LinkLabel             string
	StaleDays             string
	Err                   map[string]string
}

func stateOf(a *artifact.Artifact) string {
	switch {
	case a.DeletedAt != nil:
		return "bin"
	case a.UnpublishedAt != nil:
		return "unpublished"
	case a.StaleAt != nil:
		return "stale"
	}
	return "live"
}

const defaultLinkDays = int(artifact.DefaultShareLinkTTL / (24 * time.Hour))

func (s *server) maxLinkDays() int {
	if s.o.MaxShareLinkTTL <= 0 {
		return defaultLinkDays
	}
	return max(1, int(s.o.MaxShareLinkTTL/(24*time.Hour)))
}

// ownPage loads the page of the path as the person sees it.
func (s *server) ownPage(r *http.Request, v visit) (*ownPageData, error) {
	ctx := r.Context()
	p := principal(r)
	actor := present.Actor(p)
	a, versions, err := s.d.Artifacts.Get(ctx, actor, r.PathValue("slug"))
	if err != nil {
		return nil, err
	}
	u := s.d.Links.ArtifactURL(a.Slug)
	d := &ownPageData{
		base: s.baseFor(v), Page: a, State: stateOf(a), URL: u, Host: hostOf(u),
		Mine: a.OwnerHandle == v.user.Handle,
		May: map[string]bool{
			"share": p.HasScope(auth.ScopeShare), "write": p.HasScope(auth.ScopeWrite), "delete": p.HasScope(auth.ScopeDelete),
		},
		MaxLinkDays: s.maxLinkDays(), LinkDays: min(defaultLinkDays, s.maxLinkDays()),
	}
	for _, ver := range versions {
		vu := s.d.Links.VersionURL(a.Slug, ver.Number)
		d.Versions = append(d.Versions, ownVersion{Version: ver, URL: vu, Host: hostOf(vu), Published: ver.Number == a.PublishedVersion})
	}
	// The links are listed to whoever may change the page's sharing, which
	// is also who may change the page: the write policy decides both.
	switch {
	case a.DeletedAt != nil:
		d.Changeable = d.Mine
	default:
		links, err := s.d.Artifacts.ListShareLinks(ctx, actor, a.Slug)
		switch {
		case errors.Is(err, artifact.ErrForbidden):
		case err != nil:
			return nil, err
		default:
			d.Changeable = true
			now := s.now()
			for _, l := range links {
				state := "active"
				switch {
				case l.RevokedAt != nil:
					state = "revoked"
				case !now.Before(l.ExpiresAt):
					state = "expired"
				}
				d.Links = append(d.Links, ownLink{ShareLink: l, State: state})
			}
		}
	}
	return d, nil
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

func (s *server) pagePage(w http.ResponseWriter, r *http.Request, v visit) {
	d, err := s.ownPage(r, v)
	if err != nil {
		s.refusal(w, r, err)
		return
	}
	d.Flash = s.sess.Flash(w, r)
	s.render(w, r, http.StatusOK, "own-page", d)
}

// pageDone goes back to the page, which says flash.
func (s *server) pageDone(w http.ResponseWriter, r *http.Request, flash string) error {
	s.sess.SetFlash(w, flash)
	http.Redirect(w, r, ownPagePath(r.PathValue("slug")), http.StatusSeeOther)
	return nil
}

func (s *server) setVisibility(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	vis := r.PostForm.Get("visibility")
	if _, err := s.d.Artifacts.SetVisibility(r.Context(), actor, r.PathValue("slug"), vis); err != nil {
		return err
	}
	if vis == artifact.Unpublished {
		return s.pageDone(w, r, "page-hidden")
	}
	return s.pageDone(w, r, "page-visibility")
}

// sharedLink shows a new share link, once.
type sharedLink struct {
	base
	Slug, Host, URL string
	Link            *artifact.ShareLink
}

// createLink makes a share link and shows it in the answer to the POST
// itself: only its hash is stored, so no later page could show it.
func (s *server) createLink(w http.ResponseWriter, r *http.Request, v visit, actor artifact.Actor) error {
	slug := r.PathValue("slug")
	label, typed := strings.TrimSpace(r.PostForm.Get("label")), strings.TrimSpace(r.PostForm.Get("days"))
	refuse := func(field string) error {
		d, err := s.ownPage(r, v)
		if err != nil {
			return err
		}
		d.LinkLabel, d.Err = label, map[string]string{field: "invalid"}
		if n, err := strconv.Atoi(typed); err == nil {
			d.LinkDays = n
		}
		s.render(w, r, http.StatusUnprocessableEntity, "own-page", d)
		return nil
	}
	days, err := strconv.Atoi(typed)
	if err != nil || days < 1 || days > s.maxLinkDays() {
		return refuse("days")
	}
	if artifact.ValidateLabel(label) != nil {
		return refuse("label")
	}
	l, token, err := s.d.Artifacts.CreateShareLink(r.Context(), actor, slug, label, time.Duration(days)*24*time.Hour)
	if errors.Is(err, artifact.ErrInvalidArgument) {
		return refuse("label")
	}
	if err != nil {
		return err
	}
	s.render(w, r, http.StatusOK, "shared-link", sharedLink{
		base: s.baseFor(v), Slug: slug, Host: hostOf(s.d.Links.ArtifactURL(slug)), URL: s.d.Links.ShareURL(slug, token), Link: l,
	})
	return nil
}

// ownLinkOf finds the page's share link the path names.
func (s *server) ownLinkOf(r *http.Request, actor artifact.Actor) (*artifact.ShareLink, error) {
	links, err := s.d.Artifacts.ListShareLinks(r.Context(), actor, r.PathValue("slug"))
	if err != nil {
		return nil, err
	}
	for i := range links {
		if links[i].Prefix == r.PathValue("prefix") {
			return &links[i], nil
		}
	}
	return nil, artifact.ErrLinkNotFound
}

// pageConfirm asks before a change to one of the person's pages.
type pageConfirm struct {
	base
	Kind    string // "revoke" or "publish"
	Slug    string
	Host    string
	Link    *artifact.ShareLink
	Version int
	Current int  // the published version before
	Hidden  bool // the page is hidden, and stays so
}

func (s *server) revokeLinkPage(w http.ResponseWriter, r *http.Request, v visit) {
	l, err := s.ownLinkOf(r, present.Actor(principal(r)))
	if err != nil {
		s.refusal(w, r, err)
		return
	}
	if !l.Active(s.now()) {
		s.refusal(w, r, artifact.ErrLinkNotFound)
		return
	}
	s.render(w, r, http.StatusOK, "page-confirm", pageConfirm{base: s.baseFor(v), Kind: "revoke", Slug: r.PathValue("slug"), Link: l})
}

func (s *server) revokeLink(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	if err := s.d.Artifacts.RevokeShareLink(r.Context(), actor, r.PathValue("slug"), r.PathValue("prefix")); err != nil {
		return err
	}
	return s.pageDone(w, r, "link-revoked")
}

// version reads the version of the path, which the page must have.
func (s *server) version(r *http.Request, actor artifact.Actor) (*artifact.Artifact, int, error) {
	a, vs, err := s.d.Artifacts.Get(r.Context(), actor, r.PathValue("slug"))
	if err != nil {
		return nil, 0, err
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || !slices.ContainsFunc(vs, func(v artifact.Version) bool { return v.Number == n }) {
		return nil, 0, artifact.ErrVersionNotFound
	}
	return a, n, nil
}

func (s *server) publishPage(w http.ResponseWriter, r *http.Request, v visit) {
	a, n, err := s.version(r, present.Actor(principal(r)))
	if err != nil {
		s.refusal(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "page-confirm", pageConfirm{
		base: s.baseFor(v), Kind: "publish", Slug: a.Slug, Host: hostOf(s.d.Links.ArtifactURL(a.Slug)),
		Version: n, Current: a.PublishedVersion, Hidden: a.UnpublishedAt != nil,
	})
}

func (s *server) publishVersion(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	a, n, err := s.version(r, actor)
	if err != nil {
		return err
	}
	if _, err := s.d.Artifacts.Publish(r.Context(), actor, a.Slug, n); err != nil {
		return err
	}
	if a.UnpublishedAt != nil {
		return s.pageDone(w, r, "version-published-hidden")
	}
	return s.pageDone(w, r, "version-published")
}

// maxStaleDays bounds the grace period of the form: ten years, past which
// marking a page stale says nothing.
const maxStaleDays = 3650

func (s *server) markStale(w http.ResponseWriter, r *http.Request, v visit, actor artifact.Actor) error {
	typed := strings.TrimSpace(r.PostForm.Get("days"))
	var grace time.Duration
	if typed != "" {
		days, err := strconv.Atoi(typed)
		if err != nil || days < 1 || days > maxStaleDays {
			d, err := s.ownPage(r, v)
			if err != nil {
				return err
			}
			d.StaleDays, d.Err = typed, map[string]string{"stale": "invalid"}
			s.render(w, r, http.StatusUnprocessableEntity, "own-page", d)
			return nil
		}
		grace = time.Duration(days) * 24 * time.Hour
	}
	if _, err := s.d.Artifacts.MarkStale(r.Context(), actor, r.PathValue("slug"), grace); err != nil {
		return err
	}
	return s.pageDone(w, r, "page-stale")
}

func (s *server) keepPage(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	if _, err := s.d.Artifacts.UnmarkStale(r.Context(), actor, r.PathValue("slug")); err != nil {
		return err
	}
	return s.pageDone(w, r, "page-kept")
}

func (s *server) binPage(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	if err := s.d.Artifacts.Delete(r.Context(), actor, r.PathValue("slug"), false); err != nil {
		return err
	}
	return s.pageDone(w, r, "page-binned")
}

func (s *server) restorePage(w http.ResponseWriter, r *http.Request, _ visit, actor artifact.Actor) error {
	ctx, slug := r.Context(), r.PathValue("slug")
	a, _, err := s.d.Artifacts.Get(ctx, actor, slug)
	if err != nil {
		return err
	}
	// Sent twice, or after someone else restored it: nothing is left to do.
	if a.DeletedAt == nil {
		return s.pageDone(w, r, "")
	}
	if _, _, err := s.d.Artifacts.Update(ctx, actor, artifact.UpdateRequest{Slug: slug, Restore: true}); err != nil {
		return err
	}
	return s.pageDone(w, r, "page-restored")
}
