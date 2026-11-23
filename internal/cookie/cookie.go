// Package cookie sets the host-only cookies every host of Kazari keeps.
// Browsers accept a __Host- cookie only when it is Secure, on path / and
// without Domain, so script in an artifact on a sibling host cannot plant
// one with Domain=<base>. Plain http cannot use the prefix.
package cookie

import (
	"net/http"
	"time"
)

// Host names, reads and sets the cookies of one host. Insecure drops the
// Secure attribute and the __Host- prefix, for plain http on this machine.
type Host struct {
	Insecure bool
}

// Name returns the cookie's name: base under the __Host- prefix, or base
// itself when the host is insecure.
func (h Host) Name(base string) string {
	if h.Insecure {
		return base
	}
	return "__Host-" + base
}

// Get returns the value of the cookie base, "" when the request has none.
func (h Host) Get(r *http.Request, base string) string {
	if ck, err := r.Cookie(h.Name(base)); err == nil {
		return ck.Value
	}
	return ""
}

// Set sets the cookie base for ttl, counted in whole seconds; a ttl under a
// second deletes it.
func (h Host) Set(w http.ResponseWriter, base, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     h.Name(base),
		Value:    value,
		Path:     "/", // required by the __Host- prefix
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   !h.Insecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Delete deletes the cookie base.
func (h Host) Delete(w http.ResponseWriter, base string) {
	h.Set(w, base, "", -1)
}
