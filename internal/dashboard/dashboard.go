// Package dashboard is the web dashboard on the admin host: Overview,
// People, Pages, Activity and System, for an admin signed in in a browser.
// It calls the services as the admin API does and does nothing that API and
// the command line cannot.
//
// A session here is a credential with admin rights, so every request checks
// it afresh: the person exists, is enabled and is an admin now, and a
// session opened with an API key holds that key's admin scope. Every change
// is a POST from the dashboard's own origin that carries the session's form
// token. The session cookie opens these pages and nothing else: the JSON
// API beside them takes bearer credentials only.
package dashboard

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/session"
	"github.com/PeterBooker/kazari/internal/theme"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

// Grants are a person's connected applications and browser sessions.
// oauth.Service implements it.
type Grants interface {
	ListGrants(ctx context.Context, handle string) ([]oauth.Token, error)
	RevokeGrant(ctx context.Context, id int64) error
	GetClient(ctx context.Context, id string) (*oauth.Client, error)
	EndSessions(ctx context.Context, handle string, keep int64) error
	CountSessions(ctx context.Context, handle string) (int, error)
}

// Readiness runs the health checks. preflight.Checker implements it.
type Readiness interface {
	// Check returns the cached findings, or runs the checks when fresh.
	Check(ctx context.Context, fresh bool) (preflight.Report, error)
	// Last returns the cached findings without ever running a check.
	Last() (preflight.Report, bool)
}

// Backups writes a backup. backup.Service implements it.
type Backups interface {
	Write(ctx context.Context, w io.Writer, o backup.WriteOptions) (backup.Summary, error)
	Estimate(ctx context.Context) (backup.Estimate, error)
}

// Updates knows the newest release. update.Checker implements it.
type Updates interface {
	Result() update.Result
}

// Deps are the dashboard's collaborators. Audit, Jobs, Ready, Backup,
// Updates and Settings may be nil; what they show is then left out.
type Deps struct {
	Sessions  session.Provider
	Users     *user.Service
	Artifacts *artifact.Service
	Grants    Grants
	Audit     audit.Reader
	Recorder  audit.Recorder
	Jobs      *scheduler.Scheduler
	Ready     Readiness
	Backup    Backups
	Updates   Updates
	Settings  func() map[string]any // the effective configuration, secrets redacted
	Links     present.Links
	Logger    *slog.Logger
}

// Options describe the instance the dashboard runs on.
type Options struct {
	// URL is the dashboard's origin, admin.public_url, without a trailing slash.
	// Every link and the origin check come from it, never from a request.
	URL string
	// SignInURL is the sign-in page, e.g. https://mcp.<base>/oauth/signin.
	SignInURL string
	// AccountURL is the account page on the apex, e.g.
	// https://<base>/account/.
	AccountURL string
	MCPURL     string // the connector address
	APIURL     string // the user API's /v1 root
	BaseDomain string
	// LocalAccounts: people sign in with a built-in account, so the
	// dashboard invites them and makes reset links.
	LocalAccounts bool
	// InsecureCookies drops the Secure attribute and the __Host- prefix,
	// for a URL that is plain http on this machine.
	InsecureCookies bool

	Now     func() time.Time
	Started time.Time
	Build   buildinfo.Info
	DataDir string
	// TLSMode is tls.mode, or "proxy" when a proxy holds the certificate;
	// CertNotAfter then reports when the served certificate expires, and is
	// nil or returns the zero time when Kazari serves none.
	TLSMode      string
	CertNotAfter func() time.Time
	// DiskUsage reports the free and total bytes of the filesystem holding
	// path. Default: disk.Usage.
	DiskUsage func(path string) (free, total uint64, err error)

	// Lifecycle timings, for the dates the pages show.
	Retention, Tombstone time.Duration
	// Limits a form states.
	MaxKeyTTL, MaxShareLinkTTL time.Duration
	DefaultQuota               int64

	Telemetry instrument.Providers
}

//go:embed templates/*.html
var templateFS embed.FS

const (
	assetPrefix  = "/assets/"
	callbackPath = "/session/callback"
	signOutPath  = "/session/signout"
	// CallbackPath is where a sign-in for the dashboard ends, relative to
	// Options.URL; internal/app gives it to the sign-in host.
	CallbackPath = callbackPath

	// maxFormBytes bounds a posted form; none carries a file.
	maxFormBytes = 64 << 10
)

type server struct {
	d      Deps
	o      Options
	log    *slog.Logger
	now    func() time.Time
	sess   *session.Manager
	tmpl   *template.Template
	assets theme.Assets
	auth   auth.Sessions
}

// New returns the dashboard's handler, to be mounted at the root of the
// admin host beside the admin API's /v1/, /health and /metrics.
func New(d Deps, o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Recorder == nil {
		d.Recorder = audit.Nop{}
	}
	if d.Audit == nil {
		d.Audit = noActivity{}
	}
	o.URL = strings.TrimSuffix(o.URL, "/")
	s := &server{
		d: d, o: o, log: d.Logger, now: o.Now, assets: theme.URLs(assetPrefix), auth: auth.NewSessions(o.Telemetry),
		sess: session.New(d.Sessions, session.Options{
			Host: oauth.HostAdmin, Cookie: "kazari_admin", SignInURL: o.SignInURL,
			InsecureCookies: o.InsecureCookies, Now: o.Now,
		}),
	}
	funcs := template.FuncMap{}
	for _, m := range []template.FuncMap{commonFuncs, overviewFuncs, activityFuncs, systemFuncs} {
		maps.Copy(funcs, m)
	}
	s.tmpl = template.Must(theme.Templates().Funcs(funcs).ParseFS(templateFS, "templates/*.html"))

	pages := http.NewServeMux()
	pages.HandleFunc("GET "+callbackPath, s.callback)
	pages.HandleFunc("POST "+signOutPath, s.signOut)
	s.overviewRoutes(pages)
	s.peopleRoutes(pages)
	s.pagesRoutes(pages)
	s.activityRoutes(pages)
	s.systemRoutes(pages)
	s.view(pages, "/", func(w http.ResponseWriter, r *http.Request, c *request) error {
		return notFound("There is nothing at this address.")
	})

	// The assets stand outside the pages' guard: they are the same for
	// everyone, and a browser fetches them before any session exists.
	mux := http.NewServeMux()
	mux.Handle("GET "+assetPrefix, theme.Handler(assetPrefix))
	mux.Handle("/", middleware.Pages(o.URL)(middleware.Routed(pages)))
	return middleware.Routed(mux)
}

// noActivity is the activity log of an instance wired without one: every
// page that lists events shows none, and none has to ask whether there is
// a log.
type noActivity struct{}

func (noActivity) List(context.Context, audit.Query) (audit.Page, error) { return audit.Page{}, nil }

// request is one request of a signed-in admin.
type request struct {
	sess *session.Session
	me   *user.User
	// actor is the admin as the artifact service knows them.
	actor artifact.Actor
}

// handler serves a request of a signed-in admin. An error becomes the
// error page: a *problem as it is, a domain error by its code, anything
// else as an internal error that is logged and not shown.
type handler func(w http.ResponseWriter, r *http.Request, c *request) error

// view registers a page. It changes nothing, whatever its method.
func (s *server) view(mux *http.ServeMux, pattern string, fn handler) {
	mux.Handle(pattern, s.admin(fn, false))
}

// action registers a POST that changes something: it also needs the
// session's form token, which only the dashboard's own pages hold.
func (s *server) action(mux *http.ServeMux, pattern string, fn handler) {
	if !strings.HasPrefix(pattern, "POST ") {
		panic("dashboard: an action is a POST: " + pattern)
	}
	mux.Handle(pattern, s.admin(fn, true))
}

// admin admits a request only for an admin who is one now.
func (s *server) admin(fn handler, change bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The assets alone may be cached.
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		sess, err := s.sess.Get(r)
		if err != nil {
			s.log.ErrorContext(ctx, "dashboard: load session", "err", err)
			s.door(w, r, http.StatusInternalServerError, doorPage{Title: "Something went wrong", Message: "Please try again later."})
			return
		}
		if sess == nil {
			result := auth.ResultMissing
			if s.sess.Presented(r) {
				result = auth.ResultInvalid
			}
			s.auth.Record(ctx, result, nil)
			if session.Navigation(r) {
				s.sess.SignIn(w, r, r.URL.RequestURI())
				return
			}
			s.door(w, r, http.StatusUnauthorized, doorPage{Title: "Signed out",
				Message: "Your sign-in has ended. Open the dashboard again to sign in.", Link: "/", LinkText: "Open the dashboard"})
			return
		}
		c, err := s.admit(ctx, sess)
		if err != nil {
			s.log.ErrorContext(ctx, "dashboard: check admin", "err", err)
			s.door(w, r, http.StatusInternalServerError, doorPage{Title: "Something went wrong", Message: "Please try again later."})
			return
		}
		if c == nil {
			s.auth.Record(ctx, auth.ResultForbidden, nil)
			s.door(w, r, http.StatusForbidden, s.notAdmin(sess))
			return
		}
		if change {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
			if err := r.ParseForm(); err != nil {
				s.fail(w, r, c, &problem{status: http.StatusBadRequest, title: "The form could not be read", message: "Go back and try again."})
				return
			}
			if !sess.ValidForm(r.PostForm.Get(tokenField)) {
				s.auth.Record(ctx, auth.ResultForbidden, nil)
				s.fail(w, r, c, &problem{status: http.StatusForbidden, title: "This form has expired",
					message: "It was not sent from a page of this dashboard, or the page is from an earlier sign-in. Go back, reload the page and try again."})
				return
			}
		}
		p := auth.Principal{
			Handle: c.me.Handle, Admin: true, Scopes: auth.AllScopes, Source: auth.SourceSession,
			KeyID: sess.KeyPrefix, QuotaBytes: c.me.QuotaBytes,
		}
		s.auth.Record(ctx, auth.ResultOK, &p)
		r = r.WithContext(auth.ContextWithPrincipal(ctx, p))
		if err := fn(w, r, c); err != nil {
			s.fail(w, r, c, err)
		}
	})
}

// admit returns the request of the session's person if they are an admin
// now, and nil if they are not: demoted, or signed in with an API key that
// does not carry the admin scope.
func (s *server) admit(ctx context.Context, sess *session.Session) (*request, error) {
	u, err := s.d.Users.GetUser(ctx, sess.Handle)
	if errors.Is(err, user.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil || u.Role != user.RoleAdmin {
		return nil, nil
	}
	if sess.KeyPrefix != "" {
		st, err := s.d.Users.KeyStatus(ctx, sess.KeyPrefix)
		if errors.Is(err, user.ErrKeyNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !st.Active || st.Handle != u.Handle || !slices.Contains(st.Scopes, auth.ScopeAdmin) {
			return nil, nil
		}
	}
	return &request{sess: sess, me: u, actor: artifact.Actor{
		Handle: u.Handle, Admin: true, CanDelete: true, Credential: credential(sess),
	}}, nil
}

// credential names the session in the audit log, as present.Actor names
// an API key or a token.
func credential(sess *session.Session) string {
	if sess.KeyPrefix == "" {
		return auth.SourceSession
	}
	return auth.SourceSession + ":" + sess.KeyPrefix
}

func (s *server) notAdmin(sess *session.Session) doorPage {
	p := doorPage{
		Title:   "Not an admin",
		Message: "The dashboard is open to the admins of this Kazari instance, and " + sess.Handle + " is not one.",
		SignOut: sess.FormToken(),
		Link:    s.o.AccountURL, LinkText: "Open your account",
	}
	if sess.KeyPrefix != "" {
		p.Message = "You signed in with an API key that does not hold the admin scope. Sign out, then sign in with your password or with an admin key."
	}
	return p
}

// callback ends a sign-in: the code becomes the session cookie, and the
// browser goes on to the page it asked for.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	sess, path, err := s.sess.Open(w, r)
	if errors.Is(err, session.ErrSignIn) {
		s.door(w, r, http.StatusBadRequest, doorPage{Title: "Sign-in failed",
			Message: "This sign-in has expired, was already used or was started in another browser.",
			Link:    "/", LinkText: "Open the dashboard"})
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "dashboard: redeem", "err", err)
		s.door(w, r, http.StatusInternalServerError, doorPage{Title: "Something went wrong", Message: "Please try again later."})
		return
	}
	if c, err := s.admit(ctx, sess); err == nil && c != nil {
		details := map[string]string{"credential": credential(sess)}
		if err := s.d.Recorder.Record(ctx, audit.Event{At: s.now(), Actor: sess.Handle, Action: audit.DashboardSignIn, Details: details}); err != nil {
			s.log.WarnContext(ctx, "dashboard: audit", "err", err)
		}
	}
	// Location is set as is: http.Redirect would clean the path, and
	// cleaning can turn a path on this host into one a browser reads as
	// another host's.
	w.Header().Set("Location", path)
	w.WriteHeader(http.StatusFound)
}

// signOut ends the whole sign-in, on every host. It asks for a session
// and its form token and not for an admin: whoever signed in here, a
// member included, can sign out here.
func (s *server) signOut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	signedOut := doorPage{Title: "Signed out",
		Message: "You are signed out of this Kazari instance in this browser.", Link: "/", LinkText: "Sign in again"}
	sess, err := s.sess.Get(r)
	if err == nil && sess != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if perr := r.ParseForm(); perr != nil || !sess.ValidForm(r.PostForm.Get(tokenField)) {
			s.auth.Record(ctx, auth.ResultForbidden, nil)
			s.door(w, r, http.StatusForbidden, doorPage{Title: "This form has expired",
				Message: "It was not sent from a page of this dashboard. Open the dashboard and sign out from there.",
				Link:    "/", LinkText: "Open the dashboard"})
			return
		}
		err = s.sess.End(w, r)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "dashboard: sign out", "err", err)
		s.door(w, r, http.StatusInternalServerError, doorPage{Title: "Something went wrong", Message: "Please try again later."})
		return
	}
	s.door(w, r, http.StatusOK, signedOut)
}

// problem is an error page a handler chose: a wrong address, a refused
// change.
type problem struct {
	status         int
	title, message string
	// code is the error code on the request's span, when the status alone
	// would name a vaguer one.
	code string
}

func (p *problem) Error() string { return p.title + ": " + p.message }

func notFound(message string) error {
	return &problem{status: http.StatusNotFound, title: "Not found", message: message}
}

func refused(format string, args ...any) error {
	return &problem{status: http.StatusUnprocessableEntity, title: "That did not work", message: fmt.Sprintf(format, args...)}
}

// fail writes the error page for err.
func (s *server) fail(w http.ResponseWriter, r *http.Request, c *request, err error) {
	var p *problem
	if !errors.As(err, &p) {
		status, body := present.Error(err)
		p = &problem{status: status, code: body.Code, title: "That did not work", message: asSentence(withoutPackage(body.Message))}
		switch {
		case status >= http.StatusInternalServerError:
			s.log.ErrorContext(r.Context(), "dashboard: request failed", "method", r.Method, "route", r.Pattern, "err", err)
			p.title, p.message = "Something went wrong", "The server could not do that. The log has the details."
		case status == http.StatusNotFound:
			p.title = "Not found"
		}
	}
	code := p.code
	if code == "" {
		code = present.PageCode(p.status)
	}
	middleware.SetErrorCode(r.Context(), code)
	if rerr := s.render(w, r, c, p.status, "problem", page{Title: p.title}, p); rerr != nil {
		s.log.ErrorContext(r.Context(), "dashboard: render error page", "err", rerr)
		http.Error(w, p.title, p.status)
	}
}

// Title and Message are what the "problem" template reads.
func (p *problem) Title() string   { return p.title }
func (p *problem) Message() string { return p.message }

// withoutPackage drops the package names an error from the user service is
// wrapped in.
func withoutPackage(msg string) string {
	if i := strings.LastIndex(msg, "user: "); i >= 0 {
		return msg[i+len("user: "):]
	}
	return msg
}

// asSentence makes a message read as a sentence of its own: a capital and
// a full stop.
func asSentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// page is what a view says about itself.
type page struct {
	Title string
	// Nav is the destination the page belongs to: overview, people, pages,
	// activity or system.
	Nav string
}

// tokenField is the name of the form field that carries the form token.
const tokenField = "form_token"

// view is what every page template reads: the shell around it, the form
// token for its forms, the one-line confirmation of what was just done,
// and the page's own data.
type viewData struct {
	Shell theme.Shell
	Token string
	Flash string
	Me    *user.User
	Now   time.Time
	Data  any
}

var destinations = []struct{ key, label, href string }{
	{"overview", "Overview", "/"},
	{"people", "People", "/people"},
	{"pages", "Pages", "/pages"},
	{"activity", "Activity", "/activity"},
	{"system", "System", "/system"},
}

// render writes the template name inside the dashboard's shell.
func (s *server) render(w http.ResponseWriter, r *http.Request, c *request, status int, name string, p page, data any) error {
	nav := make([]theme.Link, 0, len(destinations))
	for _, d := range destinations {
		nav = append(nav, theme.Link{Label: d.label, Href: d.href, Current: d.key == p.Nav})
	}
	person := c.me.DisplayName
	if person == "" {
		person = c.me.Handle
	}
	v := viewData{
		Shell: theme.Shell{
			Page: theme.Page{Title: p.Title, Assets: s.assets},
			Home: "/", Host: s.o.BaseDomain, Label: "Dashboard", Nav: nav,
			Person: person, Detail: c.me.Handle + " · admin",
			SignOut: theme.Form{Action: signOutPath, Hidden: map[string]string{tokenField: c.sess.FormToken()}},
		},
		Token: c.sess.FormToken(),
		Flash: flashes[s.sess.Flash(w, r)],
		Me:    c.me,
		Now:   s.now(),
		Data:  data,
	}
	return theme.Render(w, r, s.tmpl, name, status, v)
}

// done redirects after a change, leaving the word the next page turns into
// its one-line confirmation. to is a path on this host.
func (s *server) done(w http.ResponseWriter, r *http.Request, to, flash string) error {
	if _, ok := flashes[flash]; !ok && flash != "" {
		panic("dashboard: unknown flash " + flash)
	}
	s.sess.SetFlash(w, flash)
	http.Redirect(w, r, oauth.ReturnPath(to), http.StatusSeeOther)
	return nil
}

// flashes are the confirmations a redirect can leave, by their word. A
// flash is never free text: a new secret is shown in the response to its
// own POST and nowhere else.
var flashes = func() map[string]string {
	all := map[string]string{}
	for _, m := range []map[string]string{peopleFlashes, pagesFlashes, systemFlashes} {
		maps.Copy(all, m)
	}
	return all
}()

// doorPage is a page outside the shell: before a sign-in, after a sign-out,
// or for someone who is no admin.
type doorPage struct {
	Title, Message string
	Link, LinkText string
	// SignOut, when set, is the form token of a session to end.
	SignOut string
}

func (s *server) door(w http.ResponseWriter, r *http.Request, status int, p doorPage) {
	data := struct {
		Page                          theme.Page
		Door                          doorPage
		SignOutPath, TokenField, Host string
	}{theme.Page{Title: p.Title, Assets: s.assets}, p, signOutPath, tokenField, hostOf(s.o.URL)}
	if code := present.PageCode(status); code != "" {
		middleware.SetErrorCode(r.Context(), code)
	}
	if err := theme.Render(w, r, s.tmpl, "door", status, data); err != nil {
		s.log.ErrorContext(r.Context(), "dashboard: render page", "err", err)
		http.Error(w, p.Title, status)
	}
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
