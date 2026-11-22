package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/user"
)

var pagesFlashes = map[string]string{
	"page-visibility":          "Who can open the page is changed.",
	"page-hidden":              "The page is hidden. Its address answers 404 until it is published again.",
	"page-kept":                "The stale mark is cleared. The page stays.",
	"page-binned":              "The page is in the bin. It can be restored until it is removed for good.",
	"page-restored":            "The page is restored from the bin.",
	"page-handed-over":         "The page has a new owner.",
	"version-published":        "That version is now the published one.",
	"version-published-hidden": "That version is now the published one. The page is still hidden: publish it again under Address and access to show it.",
	"link-revoked":             "The share link is revoked.",
	"page-taken-down":          "The page is hidden. Its owner can still sign in.",
	"page-taken-down-owner":    "The page is hidden and its owner is disabled.",
	"takedown-last-admin":      "The page is hidden. Its owner is the last active admin and was not disabled.",
	"takedown-self":            "The page is hidden. You own it, and nobody disables their own account.",
	"page-removed":             "The page is removed for good.",
}

func (s *server) pagesRoutes(mux *http.ServeMux) {
	s.view(mux, "GET /pages", s.pagesList)
	// Not /pages/bin: "bin" is a slug a page may have.
	s.view(mux, "GET /bin", s.pagesBin)
	s.view(mux, "GET /pages/{slug}", s.pagesPage)
	s.action(mux, "POST /pages/{slug}/visibility", s.pagesVisibility)
	s.action(mux, "POST /pages/{slug}/hide", s.pagesHide)
	s.action(mux, "POST /pages/{slug}/keep", s.pagesKeep)
	s.action(mux, "POST /pages/{slug}/bin", s.pagesToBin)
	s.action(mux, "POST /pages/{slug}/restore", s.pagesRestore)
	s.action(mux, "POST /pages/{slug}/owner", s.pagesHandOver)
	s.view(mux, "GET /pages/{slug}/versions/{n}/publish", s.pagesPublishConfirm)
	s.action(mux, "POST /pages/{slug}/versions/{n}/publish", s.pagesPublish)
	s.issue(mux, "POST /pages/{slug}/links", s.pagesLinkCreate)
	s.view(mux, "GET /pages/{slug}/links/{prefix}/revoke", s.pagesLinkRevokeConfirm)
	s.action(mux, "POST /pages/{slug}/links/{prefix}/revoke", s.pagesLinkRevoke)
	s.view(mux, "GET /pages/{slug}/takedown", s.pagesTakeDownConfirm)
	s.action(mux, "POST /pages/{slug}/takedown", s.pagesTakeDown)
	s.view(mux, "GET /pages/{slug}/remove", s.pagesRemoveConfirm)
	s.action(mux, "POST /pages/{slug}/remove", s.pagesRemove)
}

const pagesPageSize = 50

// pageState is where a page is in its life, as the dashboard words it. A
// page has one: in the bin before unpublished before stale, as the
// lifecycle checks them.
type pageState struct {
	Key, Label, Tone string
}

var (
	stateBin         = pageState{"bin", "In the bin", "tone-danger"}
	stateUnpublished = pageState{"unpublished", "Unpublished", "tone-neutral"}
	stateStale       = pageState{"stale", "Stale", "tone-warn"}
	stateLive        = pageState{"live", "Live", "tone-ok"}
	pageStates       = []pageState{stateLive, stateStale, stateUnpublished, stateBin}
)

func stateOf(a *artifact.Artifact) pageState {
	switch {
	case a.DeletedAt != nil:
		return stateBin
	case a.UnpublishedAt != nil:
		return stateUnpublished
	case a.StaleAt != nil:
		return stateStale
	}
	return stateLive
}

// visibilities are who can open a published page, in the order of the
// selects.
var visibilities = []struct{ Key, Label, Help string }{
	{string(artifact.Public), "Public", "Anyone can open it, and it is listed for members."},
	{string(artifact.Unlisted), "Unlisted", "Anyone with the address can open it."},
	{string(artifact.Members), "Members only", "People sign in to open it, or use a share link."},
}

func visibilityLabel(v artifact.Visibility) string {
	for _, o := range visibilities {
		if o.Key == string(v) {
			return o.Label
		}
	}
	return string(v)
}

// pageRow is a page as a list shows it.
type pageRow struct {
	*artifact.Artifact
	State pageState
	// Open is who can open the page once it is published, in words.
	Open string
	// Step is the next thing the lifecycle does to the page by itself, and
	// StepAt when; "" when nothing is due.
	Step   string
	StepAt time.Time
	// Views are the last viewDays days' views, on the Pages list.
	Views int64
}

func (s *server) pageRow(a *artifact.Artifact) pageRow {
	row := pageRow{Artifact: a, State: stateOf(a), Open: visibilityLabel(a.Visibility)}
	switch {
	case a.DeletedAt != nil:
		row.Step, row.StepAt = "Removed for good", a.DeletedAt.Add(s.o.Retention)
	case a.ExpiresAt != nil:
		row.Step, row.StepAt = "Moves to the bin", *a.ExpiresAt
	}
	return row
}

// pagesFilter is the filter of the Pages list as it travels in the URL.
type pagesFilter struct {
	Q, Status, Visibility, Owner string
}

func readPagesFilter(v url.Values) pagesFilter {
	return pagesFilter{
		Q: strings.TrimSpace(v.Get("q")), Status: v.Get("status"), Visibility: v.Get("visibility"), Owner: v.Get("owner"),
	}
}

// Set reports whether the filter narrows the list at all.
func (f pagesFilter) Set() bool { return f != pagesFilter{} }

// address is the list under the filter, continued at cursor.
func (f pagesFilter) address(cursor string) string {
	v := url.Values{}
	for k, val := range map[string]string{"q": f.Q, "status": f.Status, "visibility": f.Visibility, "owner": f.Owner, "cursor": cursor} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if len(v) == 0 {
		return "/pages"
	}
	return "/pages?" + v.Encode()
}

// query is the store's query for the filter. ok is false when no page can
// match: a status and a visibility that exclude each other.
func (f pagesFilter) query() (q artifact.ListQuery, ok bool) {
	q = artifact.ListQuery{All: true, Search: f.Q, Owner: f.Owner, Limit: pagesPageSize, Deleted: "include", Visibility: f.Visibility}
	switch f.Status {
	case "":
		// Asked who a page is open to, a page in the bin is open to nobody.
		if f.Visibility != "" {
			q.Deleted = "exclude"
		}
	case stateLive.Key, stateStale.Key:
		// A hidden page shows under that status alone.
		if f.Visibility == artifact.Unpublished {
			return q, false
		}
		q.Deleted, q.Stale, q.Published = "exclude", "exclude", true
		if f.Status == stateStale.Key {
			q.Stale = "only"
		}
	case stateUnpublished.Key:
		if f.Visibility != "" && f.Visibility != artifact.Unpublished {
			return q, false
		}
		q.Deleted, q.Visibility = "exclude", artifact.Unpublished
	case stateBin.Key:
		q.Deleted = "only"
	}
	return q, true
}

type pagesListData struct {
	ViewsOn      bool
	Filter       pagesFilter
	States       []pageState
	Visibilities any
	Owners       []user.User
	Rows         []pageRow
	// Next is the address of the next page, "" on the last. The cursor
	// only goes forward, so the way back is First, set on every page but
	// the first.
	Next, First string
}

func (s *server) pagesList(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	f := readPagesFilter(r.URL.Query())
	cursor := r.URL.Query().Get("cursor")
	data := pagesListData{ViewsOn: s.d.Views != nil, Filter: f, States: pageStates, Visibilities: visibilities}
	if cursor != "" {
		data.First = f.address("")
	}
	if q, ok := f.query(); ok {
		q.Cursor = cursor
		found, err := s.d.Artifacts.List(ctx, c.actor, q)
		if err != nil {
			return pageErr(err)
		}
		for i := range found.Items {
			data.Rows = append(data.Rows, s.pageRow(&found.Items[i]))
		}
		if found.NextCursor != "" {
			data.Next = f.address(found.NextCursor)
		}
		if err := s.rowViews(ctx, data.Rows); err != nil {
			return err
		}
	}
	var err error
	if data.Owners, err = s.d.Users.ListUsers(ctx); err != nil {
		return err
	}
	return s.render(w, r, c, http.StatusOK, "pages", page{Title: "Pages", Nav: "pages"}, data)
}

type pagesBinData struct {
	Rows        []pageRow
	Retention   time.Duration
	Next, First string
}

func (s *server) pagesBin(w http.ResponseWriter, r *http.Request, c *request) error {
	cursor := r.URL.Query().Get("cursor")
	found, err := s.d.Artifacts.List(r.Context(), c.actor, artifact.ListQuery{All: true, Deleted: "only", Limit: pagesPageSize, Cursor: cursor})
	if err != nil {
		return pageErr(err)
	}
	data := pagesBinData{Retention: s.o.Retention}
	for i := range found.Items {
		data.Rows = append(data.Rows, s.pageRow(&found.Items[i]))
	}
	if cursor != "" {
		data.First = "/bin"
	}
	if found.NextCursor != "" {
		data.Next = "/bin?" + url.Values{"cursor": {found.NextCursor}}.Encode()
	}
	return s.render(w, r, c, http.StatusOK, "pages_bin", page{Title: "The bin", Nav: "pages"}, data)
}

// pageErr words an error of the artifact service for a person: a page, not
// an artifact. Anything else passes through.
func pageErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifact.ErrNotFound):
		return notFound("There is no page with that slug.")
	case errors.Is(err, artifact.ErrVersionNotFound):
		return notFound("The page has no such version.")
	case errors.Is(err, artifact.ErrLinkNotFound):
		return notFound("The page has no such share link.")
	case errors.Is(err, artifact.ErrGone):
		return &problem{status: http.StatusGone, title: "Removed for good",
			message: "That page was removed for good. Its slug stays taken until the copies browsers hold have expired."}
	}
	if ae, ok := errors.AsType[*artifact.Error](err); ok {
		status, _ := present.Error(err)
		return &problem{status: status, title: "That did not work", message: asSentence(strings.ReplaceAll(ae.Message, "artifact", "page"))}
	}
	return err
}

// load returns the page with slug and its versions, newest first.
func (s *server) load(ctx context.Context, c *request, slug string) (*artifact.Artifact, []artifact.Version, error) {
	a, versions, err := s.d.Artifacts.Get(ctx, c.actor, slug)
	return a, versions, pageErr(err)
}

func pagePath(slug string) string { return "/pages/" + url.PathEscape(slug) }

type versionRow struct {
	artifact.Version
	URL, Host string
	Published bool
}

type linkRow struct {
	artifact.ShareLink
	// State is active, expired or revoked.
	State pageState
}

func linkState(l *artifact.ShareLink, now time.Time) pageState {
	switch {
	case l.RevokedAt != nil:
		return pageState{"revoked", "Revoked", "tone-danger"}
	case !now.Before(l.ExpiresAt):
		return pageState{"expired", "Expired", "tone-neutral"}
	}
	return pageState{"active", "Active", "tone-ok"}
}

type pageData struct {
	Page         pageRow
	URL, Host    string
	Versions     []versionRow
	Links        []linkRow
	Events       []audit.Event
	Views        pageViews
	Owners       []user.User // who the page can be handed to
	Visibilities any
	// The share-link form: its limits, and what was typed when it was
	// refused.
	MaxLinkDays, LinkDays int
	LinkLabel             string
	// LinkErr names what is wrong with a field of that form, by the
	// field's name.
	LinkErr map[string]string
}

const (
	pageEvents      = 20
	defaultLinkDays = int(artifact.DefaultShareLinkTTL / (24 * time.Hour))
)

func (s *server) maxLinkDays() int {
	return max(1, int(s.o.MaxShareLinkTTL/(24*time.Hour)))
}

// pageView builds the page's own page.
func (s *server) pageView(ctx context.Context, c *request, slug string) (*pageData, error) {
	a, versions, err := s.load(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	u := s.d.Links.ArtifactURL(a.Slug)
	d := &pageData{
		Page: s.pageRow(a), URL: u, Host: hostOf(u), Visibilities: visibilities,
		MaxLinkDays: s.maxLinkDays(), LinkDays: min(defaultLinkDays, s.maxLinkDays()),
	}
	for _, v := range versions {
		vu := s.d.Links.VersionURL(a.Slug, v.Number)
		d.Versions = append(d.Versions, versionRow{Version: v, URL: vu, Host: hostOf(vu), Published: v.Number == a.PublishedVersion})
	}
	// A page in the bin has links, but they open nothing and cannot change
	// until it is restored.
	if a.DeletedAt == nil {
		links, err := s.d.Artifacts.ListShareLinks(ctx, c.actor, a.Slug)
		if err != nil {
			return nil, pageErr(err)
		}
		now := s.now()
		for i := range links {
			d.Links = append(d.Links, linkRow{ShareLink: links[i], State: linkState(&links[i], now)})
		}
	}
	if d.Views, err = s.pageViews(ctx, a.Slug, a.Visibility == artifact.Members); err != nil {
		return nil, err
	}
	events, err := s.d.Audit.List(ctx, audit.Query{Slug: a.Slug, Limit: pageEvents})
	if err != nil {
		return nil, err
	}
	d.Events = events.Events
	people, err := s.d.Users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		if p.DisabledAt == nil && p.Handle != a.OwnerHandle {
			d.Owners = append(d.Owners, p)
		}
	}
	return d, nil
}

func (s *server) pagesPage(w http.ResponseWriter, r *http.Request, c *request) error {
	d, err := s.pageView(r.Context(), c, r.PathValue("slug"))
	if err != nil {
		return err
	}
	return s.render(w, r, c, http.StatusOK, "pages_page", page{Title: d.Page.Slug, Nav: "pages"}, d)
}

func (s *server) pagesVisibility(w http.ResponseWriter, r *http.Request, c *request) error {
	slug, v := r.PathValue("slug"), r.PostForm.Get("visibility")
	if _, err := s.d.Artifacts.SetVisibility(r.Context(), c.actor, slug, v); err != nil {
		return pageErr(err)
	}
	if v == artifact.Unpublished {
		return s.done(w, r, pagePath(slug), "page-hidden")
	}
	return s.done(w, r, pagePath(slug), "page-visibility")
}

func (s *server) pagesHide(w http.ResponseWriter, r *http.Request, c *request) error {
	slug := r.PathValue("slug")
	if _, err := s.d.Artifacts.SetVisibility(r.Context(), c.actor, slug, artifact.Unpublished); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "page-hidden")
}

func (s *server) pagesKeep(w http.ResponseWriter, r *http.Request, c *request) error {
	slug := r.PathValue("slug")
	if _, err := s.d.Artifacts.UnmarkStale(r.Context(), c.actor, slug); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "page-kept")
}

func (s *server) pagesToBin(w http.ResponseWriter, r *http.Request, c *request) error {
	slug := r.PathValue("slug")
	if err := s.d.Artifacts.Delete(r.Context(), c.actor, slug, false); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "page-binned")
}

func (s *server) pagesRestore(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx, slug := r.Context(), r.PathValue("slug")
	a, _, err := s.load(ctx, c, slug)
	if err != nil {
		return err
	}
	// A form sent twice, or from a page loaded before someone else
	// restored it: there is nothing left to do, and an update would only
	// add a line to the log.
	if a.DeletedAt == nil {
		return s.done(w, r, pagePath(slug), "")
	}
	if _, _, err := s.d.Artifacts.Update(ctx, c.actor, artifact.UpdateRequest{Slug: slug, Restore: true}); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "page-restored")
}

func (s *server) pagesHandOver(w http.ResponseWriter, r *http.Request, c *request) error {
	slug, owner := r.PathValue("slug"), strings.TrimSpace(r.PostForm.Get("owner"))
	if owner == "" {
		return refused("Choose the person to hand the page to.")
	}
	if _, err := s.d.Artifacts.Transfer(r.Context(), c.actor, slug, owner); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "page-handed-over")
}

// version reads the version number of the path and checks the page has it.
func (s *server) version(r *http.Request, c *request) (*artifact.Artifact, int, error) {
	a, _, err := s.load(r.Context(), c, r.PathValue("slug"))
	if err != nil {
		return nil, 0, err
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > a.LatestVersion {
		return nil, 0, notFound("The page has no such version.")
	}
	return a, n, nil
}

func (s *server) pagesPublishConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	a, n, err := s.version(r, c)
	if err != nil {
		return err
	}
	if a.DeletedAt != nil {
		return refused("The page is in the bin. Restore it first.")
	}
	if n == a.PublishedVersion {
		if a.UnpublishedAt != nil {
			return refused("Version %d is already the published one, and the page is hidden. Publish the page again under Address and access to show it.", n)
		}
		return refused("Version %d is already the published one.", n)
	}
	host := hostOf(s.d.Links.ArtifactURL(a.Slug))
	version := strconv.Itoa(n)
	d := pageConfirmation(a.Slug)
	d.Title = "Publish version " + version + " of " + a.Slug
	d.Ends = []string{"The address " + host + " shows version " + version + " from now on."}
	d.Stays = []string{
		"No version is deleted, and each keeps its own address.",
		"The next publish, from an agent or the command line, makes a new version and goes live as usual.",
	}
	d.Action, d.Button = pagePath(a.Slug)+"/versions/"+version+"/publish", "Publish version "+version
	if a.PublishedVersion != 0 {
		d.Ends[0] = "The address " + host + " shows version " + version + " from now on, in place of version " + strconv.Itoa(a.PublishedVersion) + "."
	}
	// Choosing a version does not show a hidden page, and must not say so.
	if a.UnpublishedAt != nil {
		d.Ends = []string{"Version " + version + " becomes the published version."}
		d.Stays = append([]string{"The page stays hidden: " + host + " answers 404 until you publish the page again under Address and access."}, d.Stays...)
	}
	return s.confirm(w, r, c, d)
}

func (s *server) pagesPublish(w http.ResponseWriter, r *http.Request, c *request) error {
	a, n, err := s.version(r, c)
	if err != nil {
		return err
	}
	if _, err := s.d.Artifacts.Publish(r.Context(), c.actor, a.Slug, n); err != nil {
		return pageErr(err)
	}
	if a.UnpublishedAt != nil {
		return s.done(w, r, pagePath(a.Slug), "version-published-hidden")
	}
	return s.done(w, r, pagePath(a.Slug), "version-published")
}

// pageConfirmation starts the confirmation of a change to the page slug,
// with the headings every such page has.
func pageConfirmation(slug string) confirmation {
	return confirmation{Slug: slug, EndsTitle: "What happens", StaysTitle: "What stays as it is", Cancel: pagePath(slug)}
}

// pagesLinkCreate makes a share link and shows it in the response to the
// POST itself: the token is stored as a hash, so no later page can show it
// and no redirect may carry it.
func (s *server) pagesLinkCreate(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx, slug := r.Context(), r.PathValue("slug")
	label, typed := strings.TrimSpace(r.PostForm.Get("label")), strings.TrimSpace(r.PostForm.Get("days"))
	refuse := func(field, msg string) error {
		d, err := s.pageView(ctx, c, slug)
		if err != nil {
			return err
		}
		d.LinkLabel, d.LinkErr = label, map[string]string{field: msg}
		if n, err := strconv.Atoi(typed); err == nil {
			d.LinkDays = n
		}
		return s.render(w, r, c, http.StatusUnprocessableEntity, "pages_page", page{Title: d.Page.Slug, Nav: "pages"}, d)
	}
	days, err := strconv.Atoi(typed)
	if err != nil || days < 1 || days > s.maxLinkDays() {
		return refuse("days", "Give a number of days from 1 to "+strconv.Itoa(s.maxLinkDays())+".")
	}
	// The days are checked above, so what the service still refuses here is
	// the note.
	if artifact.ValidateLabel(label) != nil {
		return refuse("label", "Keep the note to one line of plain text, at most "+strconv.Itoa(artifact.LabelMaxBytes)+" bytes.")
	}
	l, token, err := s.d.Artifacts.CreateShareLink(ctx, c.actor, slug, label, time.Duration(days)*24*time.Hour)
	if errors.Is(err, artifact.ErrInvalidArgument) {
		return refuse("label", "That note was not accepted. Use plain text on one line.")
	}
	if err != nil {
		return pageErr(err)
	}
	what := "Share link"
	if l.Label != "" {
		what += " for " + l.Label
	}
	// The link is in this response and nowhere else.
	return s.showOnce(w, r, c, shownOnce{
		Slug: slug, Title: "Share link for " + slug, Label: what,
		Lede: "Copy it now and send it over a channel you trust. Whoever has it can open " +
			hostOf(s.d.Links.ArtifactURL(slug)) + " without signing in.",
		Value: s.d.Links.ShareURL(slug, token), Button: "Copy the link",
		Notes: []string{
			"It works until " + l.ExpiresAt.UTC().Format(dayLayout) + ", or until it is revoked from the page's own page.",
			"Kazari keeps no copy. After you leave this page it cannot be shown again; create another if it is lost.",
		},
		Back: "Back to the page",
	})
}

// link finds the page's share link with the prefix of the path.
func (s *server) link(r *http.Request, c *request) (string, *artifact.ShareLink, error) {
	slug, prefix := r.PathValue("slug"), r.PathValue("prefix")
	links, err := s.d.Artifacts.ListShareLinks(r.Context(), c.actor, slug)
	if err != nil {
		return "", nil, pageErr(err)
	}
	for i := range links {
		if links[i].Prefix == prefix {
			return slug, &links[i], nil
		}
	}
	return "", nil, notFound("The page has no such share link.")
}

func (s *server) pagesLinkRevokeConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	slug, l, err := s.link(r, c)
	if err != nil {
		return err
	}
	if !l.Active(s.now()) {
		return refused("That share link has already ended.")
	}
	name := l.Prefix
	if l.Label != "" {
		name += ", " + l.Label
	}
	d := pageConfirmation(slug)
	d.Title, d.Lede = "Revoke a share link of "+slug, "The link "+name+"."
	d.Ends = []string{
		"The link stops opening the page at once.",
		"Browsers that opened the page with it lose access on their next request.",
		"This cannot be undone.",
	}
	d.Stays = []string{"The page, its other share links and everyone who signs in to open it.", "A new link can be created at any time."}
	d.Action, d.Button, d.Danger = pagePath(slug)+"/links/"+url.PathEscape(l.Prefix)+"/revoke", "Revoke the link", true
	return s.confirm(w, r, c, d)
}

func (s *server) pagesLinkRevoke(w http.ResponseWriter, r *http.Request, c *request) error {
	slug, l, err := s.link(r, c)
	if err != nil {
		return err
	}
	if err := s.d.Artifacts.RevokeShareLink(r.Context(), c.actor, slug, l.Prefix); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, pagePath(slug), "link-revoked")
}

const disableOwnerField = "disable_owner"

// pagesTakeDownConfirm asks before a page is taken down at once: hidden,
// and its owner disabled, since hiding alone is not a lock.
func (s *server) pagesTakeDownConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	a, _, err := s.load(ctx, c, r.PathValue("slug"))
	if err != nil {
		return err
	}
	if a.DeletedAt != nil {
		return refused("The page is in the bin, so nobody can open it. Remove it for good to delete its content.")
	}
	d := pageConfirmation(a.Slug)
	d.Title, d.Lede = "Take down "+a.Slug, "For a page that has to go offline now."
	d.Ends = []string{"The page is hidden: " + hostOf(s.d.Links.ArtifactURL(a.Slug)) + " and the address of every version answer 404 at once."}
	d.Stays = []string{
		"Nothing is deleted. Every version stays stored, and the page can be published again.",
		"To delete the content as well, remove the page for good afterwards.",
	}
	d.Action, d.Button, d.Danger = pagePath(a.Slug)+"/takedown", "Take the page down", true
	owner, err := s.d.Users.GetUser(ctx, a.OwnerHandle)
	if err != nil {
		return err
	}
	switch {
	case owner.Handle == c.me.Handle:
		d.Note = "You own this page. Hiding it is all a take-down does here: nobody disables their own account."
	case owner.DisabledAt != nil:
		d.Note = "Its owner, " + owner.Handle + ", is already disabled."
	default:
		d.Check = &confirmCheck{
			Name: disableOwnerField, Legend: "The owner", Checked: true,
			Label: "Also disable its owner, " + owner.Handle,
			Help: "Hiding is not a lock: the owner, or anyone else who may change the page, can publish it again. " +
				"Disabling ends the owner's sign-in, their API keys, the apps they connected and the share links they created, for good. " +
				"Their pages stay, and the account can be enabled again.",
		}
	}
	return s.confirm(w, r, c, d)
}

// pagesTakeDown hides the page first, so it is offline whatever becomes of
// the rest, then disables its owner if asked. A guard of the user service
// may refuse that; the page then says so and stays hidden.
func (s *server) pagesTakeDown(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	a, _, err := s.load(ctx, c, r.PathValue("slug"))
	if err != nil {
		return err
	}
	if _, err := s.d.Artifacts.SetVisibility(ctx, c.actor, a.Slug, artifact.Unpublished); err != nil {
		return pageErr(err)
	}
	if r.PostForm.Get(disableOwnerField) == "" {
		return s.done(w, r, pagePath(a.Slug), "page-taken-down")
	}
	disabled := true
	_, err = s.d.Users.UpdateUser(ctx, a.OwnerHandle, user.UpdateUserRequest{Disabled: &disabled})
	switch {
	case err == nil:
		return s.done(w, r, pagePath(a.Slug), "page-taken-down-owner")
	case errors.Is(err, user.ErrLastAdmin):
		return s.done(w, r, pagePath(a.Slug), "takedown-last-admin")
	case errors.Is(err, user.ErrSelf):
		return s.done(w, r, pagePath(a.Slug), "takedown-self")
	}
	s.log.ErrorContext(ctx, "dashboard: take down: disable owner", "err", err)
	return &problem{status: http.StatusInternalServerError, title: "The page is hidden, but its owner is not disabled",
		message: "The server could not disable " + a.OwnerHandle + ". Try again from their page under People."}
}

type removeData struct {
	Page      pageRow
	Host      string
	Tombstone time.Duration
	Retention time.Duration
	Typed     string
	Error     string
}

func (s *server) removeView(ctx context.Context, c *request, slug string) (*removeData, error) {
	a, _, err := s.load(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	return &removeData{Page: s.pageRow(a), Host: hostOf(s.d.Links.ArtifactURL(a.Slug)), Tombstone: s.o.Tombstone, Retention: s.o.Retention}, nil
}

func (s *server) pagesRemoveConfirm(w http.ResponseWriter, r *http.Request, c *request) error {
	d, err := s.removeView(r.Context(), c, r.PathValue("slug"))
	if err != nil {
		return err
	}
	return s.render(w, r, c, http.StatusOK, "pages_remove", page{Title: "Remove " + d.Page.Slug + " for good", Nav: "pages"}, d)
}

// pagesRemove purges the page once its slug was typed: the one change here
// that nothing brings back.
func (s *server) pagesRemove(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx, slug := r.Context(), r.PathValue("slug")
	d, err := s.removeView(ctx, c, slug)
	if err != nil {
		return err
	}
	if d.Typed = strings.TrimSpace(r.PostForm.Get("slug")); d.Typed != d.Page.Slug {
		d.Error = "That is not the slug of this page. Nothing was removed."
		return s.render(w, r, c, http.StatusUnprocessableEntity, "pages_remove", page{Title: "Remove " + d.Page.Slug + " for good", Nav: "pages"}, d)
	}
	if err := s.d.Artifacts.Delete(ctx, c.actor, slug, true); err != nil {
		return pageErr(err)
	}
	return s.done(w, r, "/pages", "page-removed")
}

// rowViews fills in each row's views with one query.
func (s *server) rowViews(ctx context.Context, rows []pageRow) error {
	if s.d.Views == nil || len(rows) == 0 {
		return nil
	}
	slugs := make([]string, len(rows))
	for i := range rows {
		slugs[i] = rows[i].Slug
	}
	sums, err := s.d.Views.Sums(ctx, slugs, viewDays)
	if err != nil {
		return err
	}
	for i := range rows {
		rows[i].Views = sums[rows[i].Slug].Views
	}
	return nil
}
