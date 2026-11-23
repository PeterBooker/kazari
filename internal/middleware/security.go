package middleware

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PeterBooker/kazari/internal/errcode"
)

// SecurityHeaders sets control-plane defaults before the handler runs, so
// the handler can override any of them.
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// PageCSP is the policy of the pages Kazari renders itself: everything from
// the page's own origin, and no inline script or style, so a name an artifact
// or a person chose can never run as either.
const PageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

const formActionSelf = "form-action 'self'"

// safeOrigin matches an origin that can be put in a CSP source list as is.
var safeOrigin = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:[0-9]+)?$|^http://\[::1\](:[0-9]+)?$`)

// PageCSPFor is PageCSP for a page whose form submission is redirected on
// to targets: browsers apply form-action to the redirect after a
// submission, so their origins join it. Nothing else is loosened, and a
// target that is empty, unparsable or not a plain origin is left out.
func PageCSPFor(targets ...string) string {
	list := formActionSelf
	for _, t := range targets {
		u, err := url.Parse(t)
		if err != nil || t == "" {
			continue
		}
		origin := u.Scheme + "://" + u.Host
		if safeOrigin.MatchString(origin) && !strings.Contains(list, " "+origin) {
			list += " " + origin
		}
	}
	return strings.Replace(PageCSP, formActionSelf, list, 1)
}

// Navigation reports whether r loads a page into a browser tab, where a
// redirect to sign in helps. Browsers that send no Fetch Metadata are
// judged by their Accept header.
func Navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// SameOrigin reports whether a form post comes from a page of origin
// (scheme://host[:port], as configured, never taken from the request).
// Same-site is not enough: artifacts on sibling hosts run member-authored
// script. A no-referrer policy makes browsers send Origin "null", so then
// Sec-Fetch-Site decides; with neither header (a non-browser client or an
// old browser) the caller's __Host- cookie and form token still hold.
func SameOrigin(r *http.Request, origin string) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		return strings.EqualFold(o, origin)
	}
	return sameOriginFetch(r)
}

// SameHost is SameOrigin for a host whose origin is not configured, such as
// an artifact host: the post must come from a page on the request's own
// host.
func SameHost(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	return sameOriginFetch(r)
}

func sameOriginFetch(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

// Pages guards the HTML pages of the control host at origin. Every response
// carries PageCSP, forbids framing and is readable by its own origin alone.
// A request that changes something must come from one of the origin's own
// pages, and one that reads must be a navigation or come from them: script
// on an artifact host is same-site, so cookies alone do not tell it from
// the person.
func Pages(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", PageCSP)
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			if !pageRequest(r, origin) {
				SetErrorCode(r.Context(), errcode.Forbidden)
				http.Error(w, "This request did not come from this server's own pages.", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func pageRequest(r *http.Request, origin string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return SameOrigin(r, origin)
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	// A link followed from another site, a chat application or a mail, into
	// a tab of its own and never into a frame.
	dest := r.Header.Get("Sec-Fetch-Dest")
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" && (dest == "" || dest == "document")
}
