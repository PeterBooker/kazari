package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
)

// actionConstants reads the audit actions out of the source: every string
// constant of internal/audit/audit.go, and the Action constants of
// internal/oauth/oauth.go and internal/signin/signin.go.
func actionConstants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for file, prefix := range map[string]string{"../audit/audit.go": "", "../oauth/oauth.go": "Action", "../signin/signin.go": "Action"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) || !strings.HasPrefix(name.Name, prefix) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err == nil && strings.Contains(value, ".") && !strings.Contains(value, " ") {
					out[name.Name] = value
				}
			}
			return true
		})
	}
	return out
}

// Every action Kazari records reads as a sentence. A new constant without
// one fails here, so the Activity page never falls back to a code for an
// action the code base knows.
func TestEventSentenceForEveryAction(t *testing.T) {
	actions := actionConstants(t)
	if len(actions) < 25 {
		t.Fatalf("found only %d action constants: %v", len(actions), actions)
	}
	for name, action := range actions {
		if _, ok := sentences[action]; !ok {
			t.Errorf("%s (%q) has no sentence in events.go", name, action)
		}
	}
	known := map[string]bool{}
	for _, action := range actions {
		known[action] = true
	}
	for action := range sentences {
		if !known[action] {
			t.Errorf("events.go has a sentence for %q, which is no action constant", action)
		}
	}
}

func TestEventSentences(t *testing.T) {
	type d = map[string]string
	tests := []struct {
		e    audit.Event
		want string
	}{
		{audit.Event{Actor: "ana", Action: audit.ArtifactCreate, Slug: "q3-report", Details: d{"version": "1"}}, "ana created q3-report"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactImport, Slug: "q3", Details: d{"source": "https://example.com/"}}, "ana imported q3 from https://example.com/"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactUpdate, Slug: "q3-report", Details: d{"version": "4", "size": "10"}}, "ana published version 4 of q3-report"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactUpdate, Slug: "q3-report", Details: d{"version": "5", "size": "10", "staged": "true"}}, "ana staged version 5 of q3-report"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactUpdate, Slug: "q3-report"}, "ana changed the details of q3-report"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactVersionDelete, Slug: "q3-report", Details: d{"versions": "2"}}, "ana deleted version 2 of q3-report"},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactVersionDelete, Slug: "q3-report", Details: d{"versions": "1,2,3", "reason": "keep_versions"}}, "the scheduler deleted versions 1, 2, 3 of q3-report, past the number it keeps"},
		{audit.Event{Actor: "boss", Action: audit.ArtifactPublish, Slug: "q3-report", Details: d{"version": "2"}}, "boss switched q3-report to version 2"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactStale, Slug: "notes", Details: d{"expires_at": "2026-12-05T12:00:00Z"}}, "ana marked notes stale, to move to the bin on 5 Dec 2026"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactUnstale, Slug: "notes"}, "ana cleared the stale mark on notes"},
		{audit.Event{Actor: "admin", Action: audit.ArtifactVisibility, Slug: "q3-report", Details: d{"visibility": "unpublished"}}, "the operator hid q3-report"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactVisibility, Slug: "q3-report", Details: d{"visibility": "members"}}, "ana opened q3-report to members only"},
		{audit.Event{Actor: "ana", Action: audit.ArtifactVisibility, Slug: "q3-report", Details: d{"visibility": "public"}}, "ana made q3-report public"},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactDelete, Slug: "old-notes", Details: d{"reason": "expired"}}, "the scheduler moved old-notes to the bin"},
		{audit.Event{Actor: "boss", Action: audit.ArtifactRestore, Slug: "old-notes"}, "boss restored old-notes from the bin"},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactPurge, Slug: "old-notes", Details: d{"reason": "retention"}}, "the scheduler removed old-notes for good"},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactFree, Slug: "old-notes"}, "the scheduler freed the slug old-notes"},
		{audit.Event{Actor: "boss", Action: audit.ArtifactTransfer, Slug: "q3-report", Details: d{"from": "ana", "to": "ben"}}, "boss handed q3-report over from ana to ben"},
		{audit.Event{Actor: "ana", Action: audit.ShareLinkCreate, Slug: "q3-report", Details: d{"prefix": "abcdefgh"}}, "ana created share link abcdefgh for q3-report"},
		{audit.Event{Actor: "boss", Action: audit.ShareLinkRevoke, Slug: "q3-report", Details: d{"prefix": "abcdefgh", "created_by": "ana"}}, "boss revoked share link abcdefgh for q3-report, which ana created"},
		{audit.Event{Actor: "boss", Action: audit.UserCreate, Details: d{"handle": "ben", "role": "member"}}, "boss added ben as a member"},
		{audit.Event{Actor: "peter", Action: audit.UserCreate, Details: d{"handle": "peter", "role": "admin", "via": "setup"}}, "peter set up the instance as its first admin"},
		{audit.Event{Actor: "mira", Action: audit.UserCreate, Details: d{"handle": "mira", "role": "member", "via": "identity"}}, "mira signed in for the first time and joined as a member"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "disabled": "true"}}, "boss disabled ben"},
		{audit.Event{Actor: "admin", Action: audit.UserUpdate, Details: d{"handle": "ben", "disabled": "false", "role": "admin"}}, "the operator enabled ben and made them an admin"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "role": "admin"}}, "boss made ben an admin"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "email": "b@example.com", "quota_bytes": "0"}}, "boss changed the email and quota of ben"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "role": "member", "display_name": "Ben"}}, "boss made ben a member and changed their name"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "identity": "link", "issuer": "https://idp", "subject": "x"}}, "boss linked a sign-in method to ben"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben", "identity": "unlink"}}, "boss unlinked a sign-in method from ben"},
		{audit.Event{Actor: "admin", Action: audit.UserUpdate, Details: d{"handle": "admin-2", "renamed_from": "admin"}}, "the operator renamed admin to admin-2"},
		{audit.Event{Actor: "boss", Action: audit.UserUpdate, Details: d{"handle": "ben"}}, "boss updated ben"},
		{audit.Event{Actor: "ana", Action: audit.KeyIssue, Details: d{"handle": "ana", "prefix": "abcdefgh"}}, "ana issued key abcdefgh"},
		{audit.Event{Actor: "boss", Action: audit.KeyIssue, Details: d{"handle": "ana", "prefix": "abcdefgh"}}, "boss issued key abcdefgh for ana"},
		{audit.Event{Actor: "ana", Action: audit.KeyRevoke, Details: d{"handle": "ana", "prefix": "bbbbbbbb", "issued_by": "abcdefgh"}}, "ana revoked key bbbbbbbb, issued with abcdefgh"},
		{audit.Event{Actor: "ana", Action: audit.KeyRotate, Details: d{"handle": "ana", "prefix": "abcdefgh", "new_prefix": "cccccccc"}}, "ana replaced key abcdefgh with cccccccc"},
		{audit.Event{Actor: "boss", Action: audit.InviteCreate, Details: d{"handle": "ben", "purpose": "invite"}}, "boss invited ben"},
		{audit.Event{Actor: "boss", Action: audit.InviteCreate, Details: d{"handle": "ben", "purpose": "reset"}}, "boss made a password reset link for ben"},
		{audit.Event{Actor: "admin", Action: audit.InviteCreate, Details: d{"purpose": "setup"}}, "the operator made a setup link for the first admin"},
		{audit.Event{Actor: "ben", Action: audit.InviteUse, Details: d{"handle": "ben", "purpose": "invite"}}, "ben accepted their invite"},
		{audit.Event{Actor: "ben", Action: audit.InviteUse, Details: d{"handle": "ben", "purpose": "reset"}}, "ben used their password reset link"},
		{audit.Event{Actor: "peter", Action: audit.InviteUse, Details: d{"purpose": "setup"}}, "peter used the setup link"},
		{audit.Event{Actor: "boss", Action: audit.InviteRevoke, Details: d{"handle": "ben", "purpose": "invite"}}, "boss revoked the invite of ben"},
		{audit.Event{Actor: "boss", Action: audit.InviteRevoke, Details: d{"handle": "ben", "purpose": "reset"}}, "boss revoked the password reset link of ben"},
		{audit.Event{Actor: "ben", Action: audit.PasswordSet, Details: d{"handle": "ben", "via": "invite"}}, "ben set a password"},
		{audit.Event{Actor: "ben", Action: audit.PasswordSet, Details: d{"handle": "ben", "via": "change"}}, "ben changed their password"},
		{audit.Event{Actor: "ben", Action: audit.PasswordSet, Details: d{"handle": "ben", "via": "recovery"}}, "ben set a new password with a recovery code"},
		{audit.Event{Actor: "ben", Action: audit.RecoveryCreate, Details: d{"handle": "ben", "codes": "10"}}, "ben made new recovery codes"},
		{audit.Event{Actor: "ben", Action: audit.PasskeyCreate, Details: d{"handle": "ben", "name": "Laptop"}}, "ben added the passkey Laptop"},
		{audit.Event{Actor: "ben", Action: audit.PasskeyDelete, Details: d{"handle": "ben", "name": "Laptop"}}, "ben removed the passkey Laptop"},
		{audit.Event{Actor: "ana", Action: audit.PasskeyDelete, Details: d{"handle": "ben", "name": "Phone"}}, "ana removed the passkey Phone of ben"},
		{audit.Event{Actor: "ben", Action: audit.RecoveryUse, Details: d{"handle": "ben", "left": "9"}}, "ben used a recovery code, 9 left"},
		{audit.Event{Actor: "boss", Action: audit.SessionRevoke, Details: d{"user": "ben", "sessions": "2"}}, "boss ended 2 browser sessions of ben"},
		{audit.Event{Actor: "ben", Action: audit.SessionRevoke, Details: d{"user": "ben", "sessions": "1"}}, "ben ended 1 browser session of their own"},
		{audit.Event{Actor: "boss", Action: audit.DashboardSignIn, Details: d{"credential": "session"}}, "boss signed in to the dashboard"},
		{audit.Event{Actor: "boss", Action: audit.AuditExport, Details: d{"rows": "120"}}, "boss exported the activity log, 120 rows"},
		{audit.Event{Actor: "boss", Action: audit.BackupCreate, Details: d{"files": "3"}}, "boss took a backup"},
		{audit.Event{Actor: "ana", Action: "oauth.grant", Details: d{"client_id": "dcr_abc", "via": "password"}}, "ana connected the app dcr_abc"},
		{audit.Event{Actor: "ana", Action: "oauth.grant", Details: d{"client_id": "dcr_abc", "via": "api_key", "key_prefix": "abcdefgh"}}, "ana connected the app dcr_abc with key abcdefgh"},
		{audit.Event{Actor: "boss", Action: "oauth.revoke", Details: d{"grant": "7"}}, "boss disconnected an app"},
		{audit.Event{Actor: "boss", Action: "oauth.revoke", Details: d{"user": "ben"}}, "boss disconnected every app of ben and ended their sessions"},
		{audit.Event{Actor: "ana", Action: "oauth.revoke", Details: d{"grant": "7", "client_id": "dcr_abc", "reason": "refresh_replay"}}, "ana lost the connection of the app dcr_abc: one of its tokens was used twice"},
		{audit.Event{Actor: "ana", Action: "oauth.signin", Details: d{"via": "password"}}, "ana signed in with their password"},
		{audit.Event{Actor: "ana", Action: "oauth.signin", Details: d{"via": "api_key", "key_prefix": "abcdefgh"}}, "ana signed in with key abcdefgh"},
		{audit.Event{Actor: "ana", Action: "oauth.signin", Details: d{"via": "oidc"}}, "ana signed in at the provider"},
		// An action from a later version is not lost.
		{audit.Event{Actor: "ana", Action: "artifact.comment", Slug: "q3-report"}, "ana did artifact.comment on q3-report"},
		{audit.Event{Actor: "", Action: "something.new"}, "someone did something.new"},
	}
	seen := map[string]bool{}
	for _, tt := range tests {
		seen[tt.e.Action] = true
		tt.e.At = time.Unix(0, 0)
		got := describe(tt.e).String()
		if got != tt.want {
			t.Errorf("%s %v:\n got %q\nwant %q", tt.e.Action, tt.e.Details, got, tt.want)
		}
		// A sentence is written in the words of the pages around it: pages,
		// apps and sign-in methods, never the names the code gives them.
		if _, known := sentences[tt.e.Action]; known {
			for _, word := range []string{"identity", "grant", "artifact"} {
				if strings.Contains(strings.ToLower(got), word) {
					t.Errorf("%s %v reads %q, which says %q", tt.e.Action, tt.e.Details, got, word)
				}
			}
		}
	}
	for action := range sentences {
		if !seen[action] {
			t.Errorf("no example for %q", action)
		}
	}
}

// A sentence links a person or a page only by a name that can be one.
func TestEventLinks(t *testing.T) {
	links := func(e audit.Event) []string {
		var out []string
		for _, p := range describe(e) {
			if p.Href != "" {
				out = append(out, p.Href)
			}
		}
		return out
	}
	tests := []struct {
		e    audit.Event
		want string
	}{
		{audit.Event{Actor: "ana", Action: audit.ArtifactUpdate, Slug: "q3-report", Details: map[string]string{"version": "2"}}, "/people/ana /pages/q3-report"},
		{audit.Event{Actor: "boss", Action: audit.ArtifactTransfer, Slug: "q3", Details: map[string]string{"from": "ana", "to": "ben"}}, "/people/boss /people/ana /people/ben"},
		{audit.Event{Actor: "admin", Action: audit.ArtifactVisibility, Slug: "q3-report"}, "/pages/q3-report"},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactPurge, Slug: "q3-report"}, ""},
		{audit.Event{Actor: "scheduler", Action: audit.ArtifactFree, Slug: "q3-report"}, ""},
		{audit.Event{Actor: "../../session/signout", Action: audit.ArtifactUpdate, Slug: "../people/boss"}, ""},
		{audit.Event{Actor: "Ana Sousa", Action: audit.UserUpdate, Details: map[string]string{"handle": "https://evil.example", "disabled": "true"}}, ""},
		{audit.Event{Actor: "ana", Action: audit.ArtifactCreate, Slug: "a--v2"}, "/people/ana"},
	}
	for _, tt := range tests {
		if got := strings.Join(links(tt.e), " "); got != tt.want {
			t.Errorf("%s by %q on %q: links %q, want %q", tt.e.Action, tt.e.Actor, tt.e.Slug, got, tt.want)
		}
	}
}

func TestEventDetailPairs(t *testing.T) {
	got := detailPairs(map[string]string{"version": "4", "credential": "session", "note": `a "quoted"; value`})
	if want := `credential="session"; note="a \"quoted\"; value"; version="4"`; got != want {
		t.Errorf("detailPairs = %s, want %s", got, want)
	}
	if detailPairs(nil) != "" {
		t.Error("no details are not empty")
	}
}

func TestActivityExportInertCells(t *testing.T) {
	for in, want := range map[string]string{
		"=HYPERLINK(\"http://evil\")": "'=HYPERLINK(\"http://evil\")", "+1": "'+1", "-1": "'-1", "@SUM(A1)": "'@SUM(A1)",
		"\tx": "'\tx", "\rx": "'\rx", "ana": "ana", "": "", "a=b": "a=b", "2026-11-21T12:00:00Z": "2026-11-21T12:00:00Z",
	} {
		if got := inert(in); got != want {
			t.Errorf("inert(%q) = %q, want %q", in, got, want)
		}
	}
}
