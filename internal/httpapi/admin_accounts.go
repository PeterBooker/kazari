package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/user"
)

// The admin routes for built-in accounts (the links that set a password,
// browser sessions), the backup and the readiness checks.

// Sessions ends and counts a person's browser sessions. signin.Service
// implements it.
type Sessions interface {
	// EndSessions ends the user's sessions except the one with id keep; 0
	// keeps none. Their API keys and OAuth grants stay.
	EndSessions(ctx context.Context, handle string, keep int64) error
	CountSessions(ctx context.Context, handle string) (int, error)
}

// BackupWriter writes the instance's backup as a tar stream.
// backup.Service implements it; backup.ErrRunning and backup.ErrNoSpace
// arrive before the first byte.
type BackupWriter interface {
	Write(ctx context.Context, w io.Writer, o backup.WriteOptions) (backup.Summary, error)
}

// ReadyChecker runs the checks of a running server, or returns the last
// run while it is fresh. preflight.Checker implements it.
type ReadyChecker interface {
	Check(ctx context.Context, fresh bool) (preflight.Report, error)
}

// Updates knows the last answer of the update check. update.Checker
// implements it.
type Updates interface {
	Result() update.Result
}

// writeSecret is writeJSON for a response that holds a link's token: no
// cache may keep it, whatever sits in front of the handler.
func writeSecret(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

func (h *adminAPI) invite(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.InviteRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	u, l, token, err := h.d.Users.Invite(r.Context(), user.InviteRequest{
		Handle:      req.Handle,
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Role:        user.Role(req.Role),
		QuotaBytes:  req.QuotaBytes,
	})
	if err != nil {
		return err
	}
	out := api.Invited{User: present.User(u), Link: h.links.CreatedLink(l, token)}
	out.User.Invited = true
	writeSecret(w, http.StatusCreated, out)
	return nil
}

func (h *adminAPI) issueLink(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.LinkRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	purpose := user.Purpose(req.Purpose)
	if purpose != user.PurposeInvite && purpose != user.PurposeReset {
		return invalid("purpose must be invite or reset")
	}
	l, token, err := h.d.Users.IssueLink(r.Context(), r.PathValue("handle"), purpose, req.RevokeAll)
	if err != nil {
		return err
	}
	writeSecret(w, http.StatusCreated, h.links.CreatedLink(l, token))
	return nil
}

func (h *adminAPI) setupLink(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	l, token, err := h.d.Users.SetupLink(r.Context())
	if err != nil {
		return err
	}
	writeSecret(w, http.StatusCreated, h.links.CreatedLink(l, token))
	return nil
}

func (h *adminAPI) listLinks(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	links, err := h.d.Users.Links(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.Link, 0, len(links))
	for i := range links {
		out = append(out, present.Link(&links[i]))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (h *adminAPI) revokeLink(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return invalid("link id must be a positive integer")
	}
	if err := h.d.Users.RevokeLink(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) endSessions(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	handle := r.PathValue("handle")
	if _, err := h.d.Users.GetUser(r.Context(), handle); err != nil {
		return err
	}
	if err := h.d.Sessions.EndSessions(r.Context(), handle, 0); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) ready(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	fresh, err := queryBool(r, "fresh")
	if err != nil {
		return err
	}
	report, err := h.d.Ready.Check(r.Context(), fresh)
	if err != nil {
		return err
	}
	out := present.Ready(report)
	if h.d.Updates != nil {
		out.Update = present.Update(h.d.Updates.Result())
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// backup streams the archive. What Write refuses, it refuses before its
// first byte, so those are ordinary error responses; once the stream has
// begun the status is sent, and a failure can only cut the connection.
func (h *adminAPI) backup(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	certs, err := queryBool(r, "certs")
	if err != nil {
		return err
	}
	if certs && !h.o.Local {
		return &statusError{status: http.StatusForbidden, code: api.CodeForbidden,
			msg: "the certificate's private key leaves the host only over the admin socket; ask without certs, or run kazari admin backup on the server"}
	}
	// A large archive outlasts the server's write timeout, so each write
	// renews it: the download lasts while its reader reads, and one that
	// stalls ends, which frees the backup for the next caller.
	out := middleware.NewDownload(w, backup.FileName(h.o.BackupName, h.o.Now()), "application/x-tar")
	if _, err := h.d.Backup.Write(r.Context(), out, backup.WriteOptions{Certs: certs}); err != nil {
		if !out.Started() {
			return err
		}
		h.log.ErrorContext(r.Context(), "httpapi: backup failed mid-stream", "err", err)
		// A handler that returns ends the body cleanly, and the client would
		// keep a truncated archive as a whole one.
		panic(http.ErrAbortHandler)
	}
	out.Start()
	return nil
}
