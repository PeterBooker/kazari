package dashboard

import (
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

// An audit event reads as a sentence on every page that shows activity:
// "ana published version 4 of q3-report". The raw action stays beside it,
// for whoever knows the log by its codes.

// part is one run of a sentence: plain words, or a name that links to its
// person or page. Everything in it is data from the log and is escaped by
// the template like any other value.
type part struct {
	Text string
	Href string // "" for plain words
	Mono bool   // a handle, a slug, a key prefix: what is typed or copied
}

// eventLine is an event as a sentence.
type eventLine []part

// String is the sentence without its links, for the export.
func (s eventLine) String() string {
	var b strings.Builder
	for _, p := range s {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (s *eventLine) words(text string) *eventLine {
	*s = append(*s, part{Text: text})
	return s
}

func (s *eventLine) mono(text string) *eventLine {
	*s = append(*s, part{Text: text, Mono: true})
	return s
}

// The actors Kazari itself writes to the log; user.ReservedHandles keeps
// any person from holding these names.
const (
	actorOperator  = "admin"
	actorScheduler = "scheduler"
)

// person names who an event is by or about. The operator and the scheduler
// are no people; anyone else links to their page, but only when the name
// can be a handle: a detail is whatever was recorded, and a link is built
// from nothing else.
func (s *eventLine) person(name string) *eventLine {
	switch {
	case name == actorOperator:
		return s.words("the operator")
	case name == actorScheduler:
		return s.words("the scheduler")
	case name == "":
		return s.words("someone")
	case user.ValidateHandle(name) != nil:
		return s.mono(name)
	}
	*s = append(*s, part{Text: name, Href: "/people/" + url.PathEscape(name), Mono: true})
	return s
}

// page names a page and links to its own page while it can have one.
func (s *eventLine) page(slug string, link bool) *eventLine {
	if slug == "" {
		return s.words("a page")
	}
	if !link || artifact.ValidateSlug(slug, nil) != nil {
		return s.mono(slug)
	}
	*s = append(*s, part{Text: slug, Href: "/pages/" + url.PathEscape(slug), Mono: true})
	return s
}

// describe turns an event into its sentence. An action it does not know
// reads as the actor and the raw action, so a new one is never lost.
func describe(e audit.Event) eventLine {
	s := &eventLine{}
	s.person(e.Actor).words(" ")
	if write, ok := sentences[e.Action]; ok {
		write(s, e)
		return *s
	}
	s.words("did ").mono(e.Action)
	if e.Slug != "" {
		s.words(" on ").page(e.Slug, true)
	}
	return *s
}

// sentences says what each action reads as, after the actor and a space.
// A test walks the action constants and fails for one that is missing here.
var sentences = map[string]func(s *eventLine, e audit.Event){
	audit.ArtifactCreate: func(s *eventLine, e audit.Event) { s.words("created ").page(e.Slug, true) },
	audit.ArtifactImport: func(s *eventLine, e audit.Event) {
		s.words("imported ").page(e.Slug, true)
		if src := e.Details["source"]; src != "" {
			s.words(" from ").mono(src)
		}
	},
	audit.ArtifactUpdate: func(s *eventLine, e audit.Event) {
		if v := e.Details["version"]; v != "" {
			if e.Details["staged"] == "true" {
				s.words("staged version ").words(v).words(" of ").page(e.Slug, true)
				return
			}
			s.words("published version ").words(v).words(" of ").page(e.Slug, true)
			return
		}
		s.words("changed the details of ").page(e.Slug, true)
	},
	audit.ArtifactPublish: func(s *eventLine, e audit.Event) {
		s.words("switched ").page(e.Slug, true).words(" to version ").words(e.Details["version"])
	},
	audit.ArtifactVersionDelete: func(s *eventLine, e audit.Event) {
		vs := strings.Split(e.Details["versions"], ",")
		noun := "version "
		if len(vs) > 1 {
			noun = "versions "
		}
		s.words("deleted "+noun+strings.Join(vs, ", ")+" of ").page(e.Slug, true)
		if e.Details["reason"] == "keep_versions" {
			s.words(", past the number it keeps")
		}
	},
	audit.ArtifactStale: func(s *eventLine, e audit.Event) {
		s.words("marked ").page(e.Slug, true).words(" stale")
		if at, err := time.Parse(time.RFC3339, e.Details["expires_at"]); err == nil {
			s.words(", to move to the bin on " + at.UTC().Format("2 Jan 2006"))
		}
	},
	audit.ArtifactUnstale: func(s *eventLine, e audit.Event) { s.words("cleared the stale mark on ").page(e.Slug, true) },
	audit.ArtifactVisibility: func(s *eventLine, e audit.Event) {
		switch v := e.Details["visibility"]; v {
		case artifact.Unpublished:
			s.words("hid ").page(e.Slug, true)
		case string(artifact.Members):
			s.words("opened ").page(e.Slug, true).words(" to members only")
		case "":
			s.words("changed who can open ").page(e.Slug, true)
		default:
			s.words("made ").page(e.Slug, true).words(" ").words(v)
		}
	},
	audit.ArtifactDelete:  func(s *eventLine, e audit.Event) { s.words("moved ").page(e.Slug, true).words(" to the bin") },
	audit.ArtifactRestore: func(s *eventLine, e audit.Event) { s.words("restored ").page(e.Slug, true).words(" from the bin") },
	audit.ArtifactPurge:   func(s *eventLine, e audit.Event) { s.words("removed ").page(e.Slug, false).words(" for good") },
	audit.ArtifactFree:    func(s *eventLine, e audit.Event) { s.words("freed the slug ").page(e.Slug, false) },
	audit.ArtifactTransfer: func(s *eventLine, e audit.Event) {
		s.words("handed ").page(e.Slug, true).words(" over from ").person(e.Details["from"]).words(" to ").person(e.Details["to"])
	},
	audit.ShareLinkCreate: func(s *eventLine, e audit.Event) {
		s.words("created share link ").mono(e.Details["prefix"]).words(" for ").page(e.Slug, true)
		if label := e.Details["label"]; label != "" {
			s.words(", for " + label)
		}
	},
	audit.ShareLinkRevoke: func(s *eventLine, e audit.Event) {
		s.words("revoked share link ").mono(e.Details["prefix"]).words(" for ").page(e.Slug, true)
		if by := e.Details["created_by"]; by != "" {
			s.words(", which ").person(by).words(" created")
		}
	},
	audit.UserCreate: func(s *eventLine, e audit.Event) {
		who, role := e.Details["handle"], roleWords(e.Details["role"])
		switch e.Details["via"] {
		case "setup":
			s.words("set up the instance as its first admin")
		case "identity":
			s.words("signed in for the first time and joined as " + role)
		default:
			s.words("added ").person(who).words(" as " + role)
		}
	},
	audit.UserUpdate: describeUserUpdate,
	audit.KeyIssue: func(s *eventLine, e audit.Event) {
		s.words("issued key ").mono(e.Details["prefix"])
		ownerOfKey(s, e, " for ")
	},
	audit.KeyRevoke: func(s *eventLine, e audit.Event) {
		s.words("revoked key ").mono(e.Details["prefix"])
		ownerOfKey(s, e, " of ")
		if by := e.Details["issued_by"]; by != "" {
			s.words(", issued with ").mono(by)
		}
	},
	audit.KeyRotate: func(s *eventLine, e audit.Event) {
		s.words("replaced key ").mono(e.Details["prefix"])
		if next := e.Details["new_prefix"]; next != "" {
			s.words(" with ").mono(next)
		}
		ownerOfKey(s, e, " of ")
	},
	audit.InviteCreate: func(s *eventLine, e audit.Event) {
		switch user.Purpose(e.Details["purpose"]) {
		case user.PurposeInvite:
			s.words("invited ").person(e.Details["handle"])
		case user.PurposeReset:
			s.words("made a password reset link for ").person(e.Details["handle"])
		default:
			s.words("made a setup link for the first admin")
		}
	},
	audit.InviteUse: func(s *eventLine, e audit.Event) {
		switch user.Purpose(e.Details["purpose"]) {
		case user.PurposeInvite:
			s.words("accepted their invite")
		case user.PurposeReset:
			s.words("used their password reset link")
		default:
			s.words("used the setup link")
		}
	},
	audit.InviteRevoke: func(s *eventLine, e audit.Event) {
		switch user.Purpose(e.Details["purpose"]) {
		case user.PurposeInvite:
			s.words("revoked the invite of ").person(e.Details["handle"])
		case user.PurposeReset:
			s.words("revoked the password reset link of ").person(e.Details["handle"])
		default:
			s.words("revoked the setup link")
		}
	},
	audit.PasswordSet: func(s *eventLine, e audit.Event) {
		switch e.Details["via"] {
		case "change":
			s.words("changed their password")
		case "recovery":
			s.words("set a new password with a recovery code")
		default:
			s.words("set a password")
		}
	},
	audit.RecoveryCreate: func(s *eventLine, e audit.Event) { s.words("made new recovery codes") },
	audit.PasskeyCreate: func(s *eventLine, e audit.Event) {
		s.words("added the passkey " + e.Details["name"])
		ownerOfPasskey(s, e)
	},
	audit.PasskeyDelete: func(s *eventLine, e audit.Event) {
		s.words("removed the passkey " + e.Details["name"])
		ownerOfPasskey(s, e)
	},
	audit.RecoveryUse: func(s *eventLine, e audit.Event) {
		s.words("used a recovery code")
		if n := e.Details["left"]; n != "" {
			s.words(", " + n + " left")
		}
	},
	audit.SessionRevoke: func(s *eventLine, e audit.Event) {
		n := e.Details["sessions"]
		sessions := n + " browser sessions"
		if n == "1" {
			sessions = "1 browser session"
		}
		if who := e.Details["user"]; who != e.Actor {
			s.words("ended " + sessions + " of ").person(who)
			return
		}
		s.words("ended " + sessions + " of their own")
	},
	audit.DashboardSignIn: func(s *eventLine, e audit.Event) { s.words("signed in to the dashboard") },
	audit.AuditExport: func(s *eventLine, e audit.Event) {
		s.words("exported the activity log")
		if n := e.Details["rows"]; n != "" {
			s.words(", " + n + " rows")
		}
	},
	audit.BackupCreate: func(s *eventLine, e audit.Event) { s.words("took a backup") },
	oauth.ActionGrant: func(s *eventLine, e audit.Event) {
		s.words("connected the app ").mono(e.Details["client_id"])
		if key := e.Details["key_prefix"]; key != "" {
			s.words(" with key ").mono(key)
		}
	},
	oauth.ActionRevoke: func(s *eventLine, e audit.Event) {
		switch {
		case e.Details["reason"] == oauth.ReasonRefreshReplay:
			s.words("lost the connection of the app ").mono(e.Details["client_id"]).words(": one of its tokens was used twice")
		case e.Details["user"] != "":
			s.words("disconnected every app of ").person(e.Details["user"]).words(" and ended their sessions")
		default:
			s.words("disconnected an app")
		}
	},
	oauth.ActionSignIn: func(s *eventLine, e audit.Event) {
		switch e.Details["via"] {
		case "password":
			s.words("signed in with their password")
		case "passkey":
			s.words("signed in with a passkey")
		case "api_key":
			s.words("signed in with key ").mono(e.Details["key_prefix"])
		case "oidc":
			s.words("signed in at the provider")
		case oauth.ViaLink:
			s.words("signed in by choosing a password with a link")
		default:
			s.words("signed in")
		}
	},
}

// ownerOfPasskey names whose passkey it is when that is not the actor.
func ownerOfPasskey(s *eventLine, e audit.Event) {
	if who := e.Details["handle"]; who != "" && who != e.Actor {
		s.words(" of ").person(who)
	}
}

func roleWords(role string) string {
	if role == string(user.RoleAdmin) {
		return "an admin"
	}
	return "a member"
}

// ownerOfKey names whose key it is, after link, when that is not the
// actor's own.
func ownerOfKey(s *eventLine, e audit.Event, link string) {
	if who := e.Details["handle"]; who != "" && who != e.Actor {
		s.words(link).person(who)
	}
}

// describeUserUpdate reads the fields a user.update recorded: one event
// can change several.
func describeUserUpdate(s *eventLine, e audit.Event) {
	d, who := e.Details, e.Details["handle"]
	switch {
	case d["renamed_from"] != "":
		s.words("renamed ").mono(d["renamed_from"]).words(" to ").person(who)
		return
	case d["identity"] == "link":
		s.words("linked a sign-in method to ").person(who)
		return
	case d["identity"] == "unlink":
		s.words("unlinked a sign-in method from ").person(who)
		return
	}
	named := false
	name := func(before, after string) {
		s.words(before).person(who).words(after)
		named = true
	}
	switch d["disabled"] {
	case "true":
		name("disabled ", "")
	case "false":
		name("enabled ", "")
	}
	if role := d["role"]; role != "" {
		if named {
			s.words(" and made them " + roleWords(role))
		} else {
			name("made ", " "+roleWords(role))
		}
	}
	var fields []string
	for _, f := range []struct{ key, name string }{{"display_name", "name"}, {"email", "email"}, {"quota_bytes", "quota"}} {
		if _, ok := d[f.key]; ok {
			fields = append(fields, f.name)
		}
	}
	switch {
	case len(fields) > 0 && named:
		s.words(" and changed their " + list(fields))
	case len(fields) > 0:
		name("changed the "+list(fields)+" of ", "")
	case !named:
		name("updated ", "")
	}
}

// list joins words as a eventLine does: a, b and c.
func list(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// detailPairs are an event's details as key="value" pairs in key order,
// for the export; the credential that acted is among them.
func detailPairs(d map[string]string) string {
	var b strings.Builder
	for i, k := range slices.Sorted(maps.Keys(d)) {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k + "=" + strconv.Quote(d[k]))
	}
	return b.String()
}
