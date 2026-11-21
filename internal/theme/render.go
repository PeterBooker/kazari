package theme

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
)

// Render executes the template name of t with data and writes it as an HTML
// page with status. It writes nothing when the template fails, so the caller
// can still answer with an error page. The policy headers of a page come
// from middleware.Pages; this sets what belongs to the body.
func Render(w http.ResponseWriter, r *http.Request, t *template.Template, name string, status int, data any) error {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w)
	}
	return nil
}
