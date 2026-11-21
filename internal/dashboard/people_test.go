package dashboard_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

var (
	peopleLinkRE = regexp.MustCompile(`https://art\.test/account/(invite|reset)\?token=(kzl_[a-z2-7]{52})`)
	peopleKeyRE  = regexp.MustCompile(`kzk_[a-z2-7]{8}_[a-z2-7]{52}`)
)

// peopleHostile are names a person or a client can choose, each of which
// would run or break out of an attribute if a page wrote it as it is.
var peopleHostile = []string{`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`}

var (
	peopleScriptTag = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	peopleAsset     = regexp.MustCompile(`(?i)(?:src|href)="(?:https?:)?//[^"]*\.(?:css|js|woff2?|png|svg)`)
)

// peopleStrict fails unless body is a page that runs under the strict
// policy: no inline style or script, and nothing loaded from elsewhere.
func peopleStrict(t *testing.T, what, body string) {
	t.Helper()
	if strings.Contains(body, "style=") || strings.Contains(strings.ToLower(body), "<style") {
		t.Errorf("%s: inline style", what)
	}
	for _, tag := range peopleScriptTag.FindAllString(body, -1) {
		if !strings.Contains(tag, `src="/assets/`) {
			t.Errorf("%s: a script that is not the dashboard's own file: %s", what, tag)
		}
	}
	if strings.Contains(body, "</script>") && strings.Count(body, "</script>") != len(peopleScriptTag.FindAllString(body, -1)) {
		t.Errorf("%s: unbalanced script tags", what)
	}
	if m := peopleAsset.FindString(body); m != "" {
		t.Errorf("%s: an asset from another origin: %s", what, m)
	}
	for _, h := range peopleHostile {
		if strings.Contains(body, h) {
			t.Errorf("%s: a hostile name is written unescaped: %s", what, h)
		}
	}
	for _, attr := range []string{" onerror=", " onclick=", " onload="} {
		if strings.Contains(strings.ToLower(body), "<img src=x"+attr) {
			t.Errorf("%s: an injected element", what)
		}
	}
}

// peopleOperator is the artifact service's view of the test acting as an admin.
var peopleOperator = artifact.Actor{Handle: "admin", Admin: true, Operator: true}

func (e *env) peopleUser(handle string) *user.User {
	e.t.Helper()
	u, err := e.users.GetUser(context.Background(), handle)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) peopleOwner(slug string) string {
	e.t.Helper()
	a, _, err := e.artifacts.Get(context.Background(), peopleOperator, slug)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.OwnerHandle
}

// peopleConnectApp connects an OAuth client named clientName as handle, through
// the sign-in host as an agent would, and returns its access token.
func (e *env) peopleConnectApp(handle, clientName string) string {
	e.t.Helper()
	const redirect = "https://app.example/cb"
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(req *http.Request, want int) (*http.Response, string) {
		e.t.Helper()
		req.Header.Set("Origin", e.signIn.URL)
		resp, err := hc.Do(req)
		if err != nil {
			e.t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			e.t.Fatalf("%s %s: status %d, want %d: %s", req.Method, req.URL.Path, resp.StatusCode, want, body)
		}
		return resp, string(body)
	}
	form := func(path string, v url.Values, want int) (*http.Response, string) {
		e.t.Helper()
		req, _ := http.NewRequest(http.MethodPost, e.signIn.URL+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return do(req, want)
	}

	meta, _ := json.Marshal(map[string]any{"client_name": clientName, "redirect_uris": []string{redirect}})
	req, _ := http.NewRequest(http.MethodPost, e.signIn.URL+"/oauth/register", strings.NewReader(string(meta)))
	req.Header.Set("Content-Type", "application/json")
	_, body := do(req, http.StatusCreated)
	var client struct {
		ID string `json:"client_id"`
	}
	if err := json.Unmarshal([]byte(body), &client); err != nil {
		e.t.Fatal(err)
	}

	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "state": {"s"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	req, _ = http.NewRequest(http.MethodGet, e.signIn.URL+"/oauth/authorize?"+q.Encode(), nil)
	do(req, http.StatusOK)
	var requestID string
	u, _ := url.Parse(e.signIn.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == oauth.InsecureCookieName {
			requestID = c.Value
		}
	}
	form(oauth.PathSignIn, url.Values{"request_id": {requestID}, "login": {handle}, "password": {password}}, http.StatusSeeOther)
	resp, _ := form(oauth.PathConsent, url.Values{"request_id": {requestID}, "action": {"approve"}}, http.StatusFound)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	_, body = form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID},
		"redirect_uri": {redirect}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}}, http.StatusOK)
	var tok struct {
		Access string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &tok); err != nil || tok.Access == "" {
		e.t.Fatalf("token response: %v: %s", err, body)
	}
	return tok.Access
}

func TestPeopleList(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	ctx := asAdmin()
	if _, err := e.users.UpdateUser(ctx, "ana", user.UpdateUserRequest{DisplayName: ptr("Ana Sousa"), Email: ptr("ana@example.org"), QuotaBytes: ptr(int64(1 << 20))}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.users.Invite(ctx, user.InviteRequest{Handle: "cara"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(ctx, "ben", user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	e.publish("ana", "report")
	e.publish("ana", "notes")
	b := e.signedIn("boss")

	r := b.get("/people")
	peopleStrict(t, "the list", r.body)
	for _, want := range []string{
		`<span class="ja" lang="ja" aria-hidden="true">利用者</span>`, "Invite a person", `href="/invites/new"`,
		`<a href="/people/ana">Ana Sousa</a>`, "ana@example.org", `<span class="status tone-ok">Active</span>`,
		`<span class="status tone-warn">Invited</span>`, `<span class="status tone-danger">Disabled</span>`,
		`<meter min="0" max="100"`, "of 1 MiB", "no limit", `<td class="num" data-label="Pages">2</td>`,
		"password", "1 to 4 of 4", "Links waiting to be used", `href="/people/cara/invite"`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	for _, banned := range []string{"artifact", "Artifact", "grant", "identity", "Identity", "Delete"} {
		if strings.Contains(r.body, banned) {
			t.Errorf("the list says %q", banned)
		}
	}

	rows := func(r response) []string {
		var got []string
		for _, m := range regexp.MustCompile(`<td data-label="Person"><a href="/people/([a-z0-9_-]+)"`).FindAllStringSubmatch(r.body, -1) {
			got = append(got, m[1])
		}
		return got
	}
	for query, want := range map[string]string{
		"q=sousa":                   "ana",
		"q=ANA%40example":           "ana",
		"q=b":                       "ben boss",
		"role=admin":                "boss",
		"status=disabled":           "ben",
		"status=invited":            "cara",
		"status=active":             "ana boss",
		"role=member&status=active": "ana",
		"role=nonsense":             "ana ben boss cara",
	} {
		r := b.get("/people?" + query)
		if got := strings.Join(rows(r), " "); got != want {
			t.Errorf("/people?%s lists %q, want %q", query, got, want)
		}
	}
	r = b.get("/people?q=nobody-at-all")
	if !strings.Contains(r.body, "Nobody matches these filters") || !strings.Contains(r.body, `value="nobody-at-all"`) {
		t.Errorf("an empty result: %s", r.body)
	}
}

func TestPeopleListPaging(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	for i := range 60 {
		if _, err := e.users.CreateUser(asAdmin(), user.CreateUserRequest{Handle: fmt.Sprintf("member%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	b := e.signedIn("boss")
	r := b.get("/people?role=member")
	if !strings.Contains(r.body, "1 to 50 of 60 matching, 61 in all") || !strings.Contains(r.body, `href="/people?page=2&amp;role=member" rel="next"`) ||
		strings.Contains(r.body, `rel="prev"`) || strings.Contains(r.body, "/people/member50") {
		t.Errorf("first page: %s", r.body)
	}
	r = b.get("/people?page=2&role=member")
	if !strings.Contains(r.body, "51 to 60 of 60") || !strings.Contains(r.body, `href="/people?role=member" rel="prev"`) ||
		strings.Contains(r.body, `rel="next"`) || !strings.Contains(r.body, "/people/member59") || strings.Contains(r.body, "/people/member49") {
		t.Errorf("second page: %s", r.body)
	}
	// A page past the end is the last one.
	if r = b.get("/people?page=99&role=member"); !strings.Contains(r.body, "51 to 60 of 60") {
		t.Errorf("a page past the end: %s", r.body)
	}
}

func TestInviteShowsLinkOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	ctx := context.Background()

	r := b.get("/invites/new")
	peopleStrict(t, "the invite form", r.body)
	if !strings.Contains(r.body, "Kazari sends no email") || !strings.Contains(r.body, `name="handle"`) {
		t.Fatalf("invite form: %s", r.body)
	}

	r = b.post("/invites/new", url.Values{"handle": {"Cara"}, "name": {"Cara Lima"}, "email": {"cara@example.org"}, "role": {"member"}, "quota": {"500 MiB"}})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("invite: status %d: %s", r.StatusCode, r.body)
	}
	peopleStrict(t, "the invite result", r.body)
	m := peopleLinkRE.FindStringSubmatch(r.body)
	if m == nil || m[1] != "invite" {
		t.Fatalf("no invite link in %s", r.body)
	}
	token := m[2]
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Location") != "" {
		t.Errorf("Cache-Control %q, Location %q", r.Header.Get("Cache-Control"), r.Header.Get("Location"))
	}
	for _, c := range r.Header.Values("Set-Cookie") {
		if strings.Contains(c, token) || strings.Contains(c, "flash") {
			t.Errorf("the response sets a cookie beside the link: %s", c)
		}
	}
	for _, want := range []string{"Shown once", "It works once", "Opening it changes nothing", "Kazari sends no email", "cannot be shown again, only replaced", "data-copy-target"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the invite result lacks %q", want)
		}
	}
	l, u, err := e.users.Link(ctx, token)
	if err != nil || u.Handle != "cara" || l.Purpose != user.PurposeInvite || l.CreatedBy != "boss" {
		t.Fatalf("Link = %+v, %+v, %v", l, u, err)
	}
	if u.DisplayName != "Cara Lima" || u.Email != "cara@example.org" || u.Role != user.RoleMember || u.QuotaBytes != 500<<20 {
		t.Errorf("invited person = %+v", u)
	}
	if ev := e.events(audit.InviteCreate); len(ev) == 0 || ev[0].Actor != "boss" || ev[0].Details["handle"] != "cara" {
		t.Errorf("invite audit = %+v", ev)
	}
	// The link is on no later page.
	for _, path := range []string{"/people", "/people/cara", "/invites/new"} {
		if strings.Contains(b.get(path).body, token) {
			t.Errorf("%s shows the link again", path)
		}
	}
	if r = b.get("/people/cara"); !strings.Contains(r.body, "Invited") || !strings.Contains(r.body, "Make a new invite link") {
		t.Errorf("an invited person's page: %s", r.body)
	}
	// The link works: it sets the password, once.
	if _, err := e.users.Redeem(ctx, token, user.RedeemRequest{Password: password}); err != nil {
		t.Fatal(err)
	}

	// An admin invites an admin, and the invite names the inviter.
	r = b.post("/invites/new", url.Values{"handle": {"dana"}, "role": {"admin"}})
	m = peopleLinkRE.FindStringSubmatch(r.body)
	if r.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("admin invite: status %d: %s", r.StatusCode, r.body)
	}
	l, u, err = e.users.Link(ctx, m[2])
	if err != nil || u.Role != user.RoleAdmin || l.CreatedBy != "boss" {
		t.Errorf("admin invite = %+v, %+v, %v", l, u, err)
	}
}

func TestInviteFormErrors(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	if _, err := e.users.UpdateUser(asAdmin(), "boss", user.UpdateUserRequest{Email: ptr("boss@example.org")}); err != nil {
		t.Fatal(err)
	}
	b := e.signedIn("boss")
	b.get("/invites/new")
	for name, tt := range map[string]struct {
		form url.Values
		want string
	}{
		"a taken handle":    {url.Values{"handle": {"boss"}}, "That handle is taken"},
		"a reserved handle": {url.Values{"handle": {"admin"}}, "That handle is reserved"},
		"an invalid handle": {url.Values{"handle": {"a b"}}, "Use 2 to 32 characters"},
		"no handle":         {url.Values{"handle": {""}}, "Choose a handle"},
		"a taken email":     {url.Values{"handle": {"eve"}, "email": {"Boss@Example.org"}}, "Another person has that email"},
		"a size of no unit": {url.Values{"handle": {"eve"}, "quota": {"500"}}, "Say the unit"},
		"a size of words":   {url.Values{"handle": {"eve"}, "quota": {"lots"}}, "Enter a size such as"},
		"a role of its own": {url.Values{"handle": {"eve"}, "role": {"owner"}}, "Choose member or admin"},
	} {
		r := b.post("/invites/new", tt.form)
		if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, tt.want) || !strings.Contains(r.body, `aria-invalid="true"`) ||
			peopleLinkRE.MatchString(r.body) {
			t.Errorf("%s: status %d: %s", name, r.StatusCode, r.body)
		}
		peopleStrict(t, name, r.body)
	}
	if _, err := e.users.GetUser(context.Background(), "eve"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("a refused invite created the person: %v", err)
	}
}

func TestInviteReplaceKillsOldLink(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	b := e.signedIn("boss")
	ctx := context.Background()
	b.get("/invites/new")
	first := peopleLinkRE.FindStringSubmatch(b.post("/invites/new", url.Values{"handle": {"dana"}}).body)
	if first == nil {
		t.Fatal("no first link")
	}

	r := b.get("/people/dana/invite")
	peopleStrict(t, "the replace confirmation", r.body)
	if !strings.Contains(r.body, "only the new one works") || !strings.Contains(r.body, `action="/people/dana/invite"`) {
		t.Fatalf("replace confirmation: %s", r.body)
	}
	if _, _, err := e.users.Link(ctx, first[2]); err != nil {
		t.Fatalf("looking at the confirmation page ended the link: %v", err)
	}
	r = b.post("/people/dana/invite", nil)
	second := peopleLinkRE.FindStringSubmatch(r.body)
	if r.StatusCode != http.StatusOK || second == nil || second[2] == first[2] || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("replace: status %d: %s", r.StatusCode, r.body)
	}
	if _, _, err := e.users.Link(ctx, first[2]); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("the replaced link: err = %v, want ErrLinkNotFound", err)
	}
	if _, _, err := e.users.Link(ctx, second[2]); err != nil {
		t.Errorf("the new link: %v", err)
	}
	// Someone with a password gets a reset link, not an invite.
	if r = b.post("/people/ana/invite", nil); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "Create a reset link instead") {
		t.Errorf("an invite for a person with a password: status %d: %s", r.StatusCode, r.body)
	}
}

func TestPeopleResetLink(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	b := e.signedIn("boss")
	ctx := context.Background()

	r := b.get("/people/ana/reset")
	peopleStrict(t, "the reset confirmation", r.body)
	for _, want := range []string{"Every browser session of theirs", `name="revoke_all"`, "Also disconnect their apps and revoke their keys", "nothing changes for them until they use it"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the reset confirmation lacks %q", want)
		}
	}
	if strings.Contains(r.body, `name="revoke_all" value="1" checked`) {
		t.Error("revoke-all is ticked by default")
	}
	if links, _ := e.users.Links(ctx); len(links) != 0 {
		t.Fatalf("the confirmation page made a link: %+v", links)
	}

	for _, revokeAll := range []bool{false, true} {
		form := url.Values{}
		if revokeAll {
			form.Set("revoke_all", "1")
		}
		r = b.post("/people/ana/reset", form)
		m := peopleLinkRE.FindStringSubmatch(r.body)
		if r.StatusCode != http.StatusOK || m == nil || m[1] != "reset" || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("reset (revoke all %v): status %d: %s", revokeAll, r.StatusCode, r.body)
		}
		peopleStrict(t, "the reset result", r.body)
		l, u, err := e.users.Link(ctx, m[2])
		if err != nil || u.Handle != "ana" || l.Purpose != user.PurposeReset || l.RevokeAll != revokeAll || l.CreatedBy != "boss" {
			t.Errorf("reset link (revoke all %v) = %+v, %+v, %v", revokeAll, l, u, err)
		}
		if said := strings.Contains(r.body, "their apps are disconnected and their keys revoked"); said != revokeAll {
			t.Errorf("reset result (revoke all %v) says so: %v", revokeAll, said)
		}
	}
	// The person's page and the list show that a link waits, never the link.
	for _, path := range []string{"/people", "/people/ana"} {
		r = b.get(path)
		if !strings.Contains(r.body, "/revoke") || peopleLinkRE.MatchString(r.body) || strings.Contains(r.body, "kzl_") {
			t.Errorf("%s with a reset link waiting: %s", path, r.body)
		}
	}
}

func TestInviteLinkRevoke(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	ctx := context.Background()
	b.get("/invites/new")
	m := peopleLinkRE.FindStringSubmatch(b.post("/invites/new", url.Values{"handle": {"dana"}}).body)
	links, err := e.users.Links(ctx)
	if err != nil || len(links) != 1 || m == nil {
		t.Fatalf("Links = %+v, %v", links, err)
	}
	path := "/invites/" + strconv.FormatInt(links[0].ID, 10) + "/revoke"

	r := b.get("/people")
	if !strings.Contains(r.body, `href="`+path+`"`) {
		t.Fatalf("the list does not offer to revoke the link: %s", r.body)
	}
	r = b.get(path)
	peopleStrict(t, "the revoke confirmation", r.body)
	if !strings.Contains(r.body, "Revoke the invite link for dana") || !strings.Contains(r.body, "stays listed as invited") {
		t.Fatalf("revoke confirmation: %s", r.body)
	}
	if _, _, err := e.users.Link(ctx, m[2]); err != nil {
		t.Fatalf("the confirmation page revoked the link: %v", err)
	}
	r = b.submit(path, nil)
	if !strings.Contains(r.body, "The link no longer works.") {
		t.Errorf("no confirmation line: %s", r.body)
	}
	if _, _, err := e.users.Link(ctx, m[2]); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("the revoked link: err = %v, want ErrLinkNotFound", err)
	}
	for _, again := range []response{b.navigate(path), b.post(path, nil), b.navigate("/invites/999/revoke"), b.navigate("/invites/x/revoke")} {
		if again.StatusCode != http.StatusNotFound {
			t.Errorf("a link that is not waiting: status %d, want 404", again.StatusCode)
		}
	}
}

func TestPersonKeys(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	b := e.signedIn("boss")
	ctx := context.Background()

	r := b.get("/people/ana")
	peopleStrict(t, "a person's page", r.body)
	if !strings.Contains(r.body, `name="scopes" value="artifacts:read" checked`) || strings.Contains(r.body, `name="scopes" value="admin"`) {
		t.Fatalf("the key form of a member: %s", r.body)
	}
	r = b.post("/people/ana/keys", url.Values{"label": {"laptop"}, "scopes": {auth.ScopeRead, auth.ScopeWrite}, "slugs": {"docs-*, report"}, "days": {"30"}})
	secret := peopleKeyRE.FindString(r.body)
	if r.StatusCode != http.StatusOK || secret == "" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issue: status %d: %s", r.StatusCode, r.body)
	}
	peopleStrict(t, "the new key", r.body)
	for _, want := range []string{"Shown once", "claude mcp add --transport http kazari https://mcp.art.test/", "Authorization: Bearer " + secret,
		"export KAZARI_URL=https://api.art.test", "export KAZARI_TOKEN=" + secret, "cannot be shown again", "docs-*, report"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the new key's page lacks %q", want)
		}
	}
	p, err := e.users.Authenticate(ctx, secret)
	if err != nil || p.Handle != "ana" || strings.Join(p.Scopes, " ") != "artifacts:read artifacts:write" || len(p.Slugs) != 2 ||
		p.ExpiresAt == nil || !p.ExpiresAt.Equal(t0.Add(30*24*60*60*1e9)) {
		t.Fatalf("the issued key = %+v, %v", p, err)
	}
	prefix := p.KeyID
	r = b.get("/people/ana")
	if strings.Contains(r.body, secret) || !strings.Contains(r.body, "kzk_"+prefix) || !strings.Contains(r.body, "laptop") {
		t.Errorf("the person's page after issuing: %s", r.body)
	}

	// Rotation shows the new key once and ends the old one when asked.
	r = b.get("/people/ana/keys/" + prefix + "/rotate")
	peopleStrict(t, "the rotate confirmation", r.body)
	if !strings.Contains(r.body, "Rotate kzk_"+prefix) || !strings.Contains(r.body, `name="overlap"`) {
		t.Fatalf("rotate confirmation: %s", r.body)
	}
	r = b.post("/people/ana/keys/"+prefix+"/rotate", url.Values{"overlap": {"0"}})
	rotated := peopleKeyRE.FindString(r.body)
	if r.StatusCode != http.StatusOK || rotated == "" || rotated == secret || !strings.Contains(r.body, "has stopped working") {
		t.Fatalf("rotate: status %d: %s", r.StatusCode, r.body)
	}
	if _, err := e.users.Authenticate(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the rotated key still works: %v", err)
	}
	p, err = e.users.Authenticate(ctx, rotated)
	if err != nil || len(p.Slugs) != 2 {
		t.Fatalf("the new key = %+v, %v", p, err)
	}

	// Revoking goes through a page that says what else ends.
	path := "/people/ana/keys/" + p.KeyID + "/revoke"
	r = b.get(path)
	peopleStrict(t, "the revoke confirmation", r.body)
	for _, want := range []string{"Every key issued with it ends too", "Every app approved with one of them is disconnected", "cannot be brought back"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the revoke confirmation lacks %q", want)
		}
	}
	if _, err := e.users.Authenticate(ctx, rotated); err != nil {
		t.Fatalf("the confirmation page revoked the key: %v", err)
	}
	if r = b.submit(path, nil); !strings.Contains(r.body, "The key is revoked") || !strings.Contains(r.body, "Revoked and expired keys (2)") {
		t.Errorf("after revoking: %s", r.body)
	}
	if _, err := e.users.Authenticate(ctx, rotated); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the revoked key still works: %v", err)
	}

	// Another person's prefix names nothing on this person's page.
	bens, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ben"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"rotate", "revoke"} {
		path := "/people/ana/keys/" + bens.Prefix + "/" + action
		if r := b.navigate(path); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, r.StatusCode)
		}
		if r := b.post(path, nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, r.StatusCode)
		}
	}
	if st, _ := e.users.KeyStatus(ctx, bens.Prefix); !st.Active {
		t.Error("another person's key was revoked through this person's page")
	}
}

func TestPersonKeyRules(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	b := e.signedIn("boss")
	ctx := context.Background()

	// An admin-scoped key comes from here, for an admin alone.
	r := b.get("/people/boss")
	if !strings.Contains(r.body, `name="scopes" value="admin"`) || strings.Contains(r.body, `value="admin" checked`) {
		t.Fatalf("the key form of an admin: %s", r.body)
	}
	r = b.post("/people/boss/keys", url.Values{"scopes": {auth.ScopeRead, auth.ScopeAdmin}})
	p, err := e.users.Authenticate(ctx, peopleKeyRE.FindString(r.body))
	if err != nil || !p.Admin {
		t.Fatalf("an admin key = %+v, %v", p, err)
	}
	for name, tt := range map[string]struct {
		form url.Values
		want string
	}{
		"admin for a member":   {url.Values{"scopes": {auth.ScopeAdmin}}, "Tick at least one thing"},
		"no scope":             {url.Values{"label": {"empty"}}, "Tick at least one thing"},
		"a pattern of its own": {url.Values{"scopes": {auth.ScopeRead}, "slugs": {"Not A Slug!"}}, "No key was issued"},
		"days of words":        {url.Values{"scopes": {auth.ScopeRead}, "days": {"soon"}}, "Enter the number of days"},
	} {
		r := b.post("/people/ana/keys", tt.form)
		if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, tt.want) || peopleKeyRE.MatchString(r.body) {
			t.Errorf("%s: status %d: %s", name, r.StatusCode, r.body)
		}
	}
	if keys, _ := e.users.ListKeys(ctx, "ana"); len(keys) != 0 {
		t.Fatalf("refused forms issued keys: %+v", keys)
	}
	// The cap says what to do.
	for range user.DefaultMaxKeys {
		if _, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"}); err != nil {
			t.Fatal(err)
		}
	}
	r = b.post("/people/ana/keys", url.Values{"scopes": {auth.ScopeRead}})
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, "as many live keys as a person may. Revoke one first.") {
		t.Errorf("at the cap: status %d", r.StatusCode)
	}
}

func TestPersonApps(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	ctx := context.Background()
	token := e.peopleConnectApp("ana", "Claude")
	bens := e.peopleConnectApp("ben", "Other")
	b := e.signedIn("boss")

	grants, err := e.oauth.ListGrants(ctx, "ana")
	if err != nil || len(grants) != 1 {
		t.Fatalf("ListGrants = %+v, %v", grants, err)
	}
	path := "/people/ana/apps/" + strconv.FormatInt(grants[0].FamilyID, 10) + "/revoke"
	r := b.get("/people/ana")
	if !strings.Contains(r.body, `<td data-label="App">Claude</td>`) || !strings.Contains(r.body, `href="`+path+`"`) || !strings.Contains(r.body, "their sign-in") {
		t.Fatalf("connected apps: %s", r.body)
	}
	r = b.get(path)
	peopleStrict(t, "the disconnect confirmation", r.body)
	if !strings.Contains(r.body, "Disconnect Claude") || !strings.Contains(r.body, "its next request is refused") {
		t.Fatalf("disconnect confirmation: %s", r.body)
	}
	if _, err := e.oauth.Authenticate(ctx, token); err != nil {
		t.Fatalf("the confirmation page disconnected the app: %v", err)
	}
	// Another person's app is not this person's to disconnect.
	others, _ := e.oauth.ListGrants(ctx, "ben")
	foreign := "/people/ana/apps/" + strconv.FormatInt(others[0].FamilyID, 10) + "/revoke"
	for _, r := range []response{b.navigate(foreign), b.post(foreign, nil), b.navigate("/people/ana/apps/x/revoke")} {
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("another person's app: status %d, want 404", r.StatusCode)
		}
	}
	if _, err := e.oauth.Authenticate(ctx, bens); err != nil {
		t.Errorf("another person's app was disconnected: %v", err)
	}

	if r = b.submit(path, nil); !strings.Contains(r.body, "The app is disconnected.") || !strings.Contains(r.body, "has connected no app") {
		t.Errorf("after disconnecting: %s", r.body)
	}
	if _, err := e.oauth.Authenticate(ctx, token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("the disconnected app still works: %v", err)
	}
}

func TestPersonSessions(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	ctx := context.Background()
	// ana is signed in in a browser, and so is boss in a second one.
	anas := e.browser()
	anas.signInAt(anas.navigate("/").Header.Get("Location"), url.Values{"login": {"ana"}, "password": {password}})
	other := e.signedIn("boss")
	b := e.signedIn("boss")

	r := b.get("/people/ana")
	if !strings.Contains(r.body, "1 open in browsers") || !strings.Contains(r.body, "Sign out everywhere") {
		t.Fatalf("sessions: %s", r.body)
	}
	if r = b.submit("/people/ana/sessions/end", nil); !strings.Contains(r.body, "Their browser sessions have ended.") || !strings.Contains(r.body, "0 open in browsers") {
		t.Errorf("after signing ana out: %s", r.body)
	}
	if n, _ := e.oauth.CountSessions(ctx, "ana"); n != 0 {
		t.Errorf("ana has %d sessions", n)
	}
	if ev := e.events(audit.SessionRevoke); len(ev) != 1 || ev[0].Actor != "boss" || ev[0].Details["user"] != "ana" {
		t.Errorf("session audit = %+v", ev)
	}

	// An admin who signs themselves out everywhere stays signed in here.
	if r = b.submit("/people/boss/sessions/end", nil); !strings.Contains(r.body, "Your other browser sessions have ended. This one stays.") {
		t.Errorf("after signing oneself out: %s", r.body)
	}
	if n, _ := e.oauth.CountSessions(ctx, "boss"); n != 1 {
		t.Errorf("boss has %d sessions, want the one the change was made in", n)
	}
	if r := other.navigate("/people"); r.StatusCode != http.StatusFound {
		t.Errorf("the other browser: status %d, want a redirect to sign in", r.StatusCode)
	}
}

func TestPersonEdit(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	if _, err := e.users.UpdateUser(asAdmin(), "ben", user.UpdateUserRequest{Email: ptr("ben@example.org")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "ana", user.UpdateUserRequest{QuotaBytes: ptr(int64(1234567))}); err != nil {
		t.Fatal(err)
	}
	b := e.signedIn("boss")

	r := b.get("/people/ana")
	if !strings.Contains(r.body, `name="quota" type="text" value="1.2 MiB"`) {
		t.Fatalf("the profile form: %s", r.body)
	}
	// A form that leaves the rounded quota alone keeps the exact one.
	r = b.submit("/people/ana", url.Values{"name": {"Ana Sousa"}, "email": {"ana@example.org"}, "role": {"member"}, "quota": {"1.2 MiB"}})
	if u := e.peopleUser("ana"); !strings.Contains(r.body, "The changes are saved.") || u.DisplayName != "Ana Sousa" || u.Email != "ana@example.org" ||
		u.Role != user.RoleMember || u.QuotaBytes != 1234567 {
		t.Errorf("after editing: %+v", u)
	}
	ev := e.events(audit.UserUpdate)
	if len(ev) == 0 || ev[0].Actor != "boss" || ev[0].Details["display_name"] != "Ana Sousa" || ev[0].Details["role"] != "" || ev[0].Details["quota_bytes"] != "" {
		t.Errorf("edit audit = %+v, want what changed and nothing else", ev)
	}
	b.submit("/people/ana", url.Values{"name": {"Ana Sousa"}, "email": {"ana@example.org"}, "role": {"admin"}, "quota": {"2 GiB"}})
	if u := e.peopleUser("ana"); u.Role != user.RoleAdmin || u.QuotaBytes != 2<<30 {
		t.Errorf("after a promotion: %+v", u)
	}
	b.submit("/people/ana", url.Values{"name": {"Ana Sousa"}, "email": {"ana@example.org"}, "role": {"admin"}, "quota": {"no limit"}})
	if u := e.peopleUser("ana"); u.QuotaBytes != 0 {
		t.Errorf("after lifting the quota: %+v", u)
	}

	for name, tt := range map[string]struct {
		form url.Values
		want string
	}{
		"a size of words": {url.Values{"name": {"X"}, "email": {"ana@example.org"}, "role": {"admin"}, "quota": {"plenty"}}, "Enter a size such as"},
		"a taken email":   {url.Values{"name": {"X"}, "email": {"ben@example.org"}, "role": {"admin"}, "quota": {"no limit"}}, "Another person has that email"},
		"a role of words": {url.Values{"name": {"X"}, "email": {"ana@example.org"}, "role": {"owner"}, "quota": {"no limit"}}, "Choose member or admin"},
	} {
		r := b.post("/people/ana", tt.form)
		if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(r.body, tt.want) || !strings.Contains(r.body, "The changes were not saved") ||
			!strings.Contains(r.body, `value="X"`) {
			t.Errorf("%s: status %d: %s", name, r.StatusCode, r.body)
		}
		if u := e.peopleUser("ana"); u.DisplayName != "Ana Sousa" || u.Email != "ana@example.org" || u.Role != user.RoleAdmin {
			t.Errorf("%s changed the person: %+v", name, u)
		}
	}
	if r := b.navigate("/people/nobody"); r.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown person: status %d, want 404", r.StatusCode)
	}
}

func TestPersonDisableAndEnable(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.person("ben", "")
	e.person("gone", "")
	e.publish("ana", "report")
	e.publish("ana", "notes")
	ctx := context.Background()
	_, secret, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.UpdateUser(asAdmin(), "gone", user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	b := e.signedIn("boss")

	r := b.get("/people/ana")
	if !strings.Contains(r.body, `href="/people/ana/disable"`) || regexp.MustCompile(`(?i)(delete|remove) (the |this |their )?(person|account)`).MatchString(r.body) {
		t.Fatalf("a person's page: %s", r.body)
	}
	r = b.get("/people/ana/disable")
	peopleStrict(t, "the disable confirmation", r.body)
	for _, want := range []string{"What ends for good", "Every API key of theirs is revoked", "Every connected app of theirs is disconnected",
		"Every share link they created stops working", "Enabling restores none of the above", "There is no way to delete a person",
		`<option value="" selected>Leave their pages with them</option>`, `<option value="ben">`, `<option value="boss">`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("the disable confirmation lacks %q", want)
		}
	}
	if strings.Contains(r.body, `<option value="gone"`) || strings.Contains(r.body, `<option value="ana"`) {
		t.Error("the pages can be handed to a disabled person, or to the person themselves")
	}
	if e.peopleUser("ana").DisabledAt != nil {
		t.Fatal("the confirmation page disabled the person")
	}
	// A hand-over nobody can receive is refused before anything changes.
	for _, to := range []string{"gone", "nobody", "ana"} {
		if r := b.post("/people/ana/disable", url.Values{"transfer_to": {to}}); r.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("handing over to %s: status %d, want 422", to, r.StatusCode)
		}
	}
	if e.peopleUser("ana").DisabledAt != nil || e.peopleOwner("report") != "ana" {
		t.Fatal("a refused hand-over changed something")
	}

	r = b.submit("/people/ana/disable", url.Values{"transfer_to": {"ben"}})
	if !strings.Contains(r.body, "The person is disabled and their pages are handed over.") || !strings.Contains(r.body, `<span class="status tone-danger">Disabled</span>`) ||
		!strings.Contains(r.body, `action="/people/ana/enable"`) || strings.Contains(r.body, `href="/people/ana/disable"`) {
		t.Errorf("after disabling: %s", r.body)
	}
	if e.peopleUser("ana").DisabledAt == nil || e.peopleOwner("report") != "ben" || e.peopleOwner("notes") != "ben" {
		t.Errorf("ana disabled %v, report owned by %s, notes by %s", e.peopleUser("ana").DisabledAt != nil, e.peopleOwner("report"), e.peopleOwner("notes"))
	}
	if _, err := e.users.Authenticate(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("a disabled person's key still works: %v", err)
	}

	r = b.submit("/people/ana/enable", nil)
	if !strings.Contains(r.body, "The person can sign in again.") || e.peopleUser("ana").DisabledAt != nil {
		t.Errorf("after enabling: %s", r.body)
	}
	if _, err := e.users.Authenticate(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("enabling restored a key: %v", err)
	}

	// Without a hand-over the pages stay.
	e.publish("ana", "later")
	if r = b.submit("/people/ana/disable", nil); !strings.Contains(r.body, "The person is disabled.") || e.peopleOwner("later") != "ana" {
		t.Errorf("disabling without a hand-over: later is owned by %s", e.peopleOwner("later"))
	}
	// They can still be handed over afterwards.
	if r = b.submit("/people/ana/handover", url.Values{"transfer_to": {"ben"}}); !strings.Contains(r.body, "Their pages are handed over.") || e.peopleOwner("later") != "ben" {
		t.Errorf("handing over afterwards: later is owned by %s", e.peopleOwner("later"))
	}
	if r = b.submit("/people/ana/handover", url.Values{"transfer_to": {"ben"}}); !strings.Contains(r.body, "They own no pages") {
		t.Errorf("handing over nothing: %s", r.body)
	}
}

func TestPeopleGuards(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("ana", "")
	e.publish("boss", "handbook")
	b := e.signedIn("boss")

	r := b.get("/people/boss")
	if strings.Contains(r.body, `href="/people/boss/disable"`) || !strings.Contains(r.body, "Nobody disables or demotes their own account") {
		t.Fatalf("one's own page: %s", r.body)
	}
	unchanged := func(what string) {
		t.Helper()
		if u := e.peopleUser("boss"); u.DisabledAt != nil || u.Role != user.RoleAdmin {
			t.Fatalf("%s changed the last admin: %+v", what, u)
		}
		if got := e.peopleOwner("handbook"); got != "boss" {
			t.Fatalf("%s moved the last admin's pages to %s", what, got)
		}
	}
	for name, r := range map[string]response{
		"the disable page":           b.navigate("/people/boss/disable"),
		"disabling oneself":          b.post("/people/boss/disable", nil),
		"disabling with a hand-over": b.post("/people/boss/disable", url.Values{"transfer_to": {"ana"}}),
		"demoting oneself":           b.post("/people/boss", url.Values{"name": {""}, "email": {""}, "role": {"member"}, "quota": {"no limit"}}),
	} {
		if r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "Nobody disables or demotes their own account; ask another admin.") {
			t.Errorf("%s: status %d: %s", name, r.StatusCode, r.body)
		}
		peopleStrict(t, name, r.body)
		unchanged(name)
	}
	// The same form may still change what is not the role.
	if r = b.submit("/people/boss", url.Values{"name": {"The Boss"}, "email": {""}, "role": {"admin"}, "quota": {"no limit"}}); e.peopleUser("boss").DisplayName != "The Boss" {
		t.Errorf("editing one's own name: %s", r.body)
	}

	// With two admins, one demotes the other, and then is the last.
	e.person("chief", user.RoleAdmin)
	b.submit("/people/chief", url.Values{"name": {""}, "email": {""}, "role": {"member"}, "quota": {"no limit"}})
	if u := e.peopleUser("chief"); u.Role != user.RoleMember {
		t.Errorf("demoting another admin: %+v", u)
	}
	unchanged("demoting another admin")
}

func TestPeopleRefusedChanges(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	e.person("second", user.RoleAdmin)
	e.person("ana", "")
	e.publish("ana", "report")
	b := e.signedIn("second")
	b.get("/people/ana")
	token := b.token
	changes := map[string]url.Values{
		"/people/ana":              {"name": {"Changed"}, "email": {""}, "role": {"admin"}, "quota": {"no limit"}},
		"/people/ana/disable":      {"transfer_to": {"boss"}},
		"/people/ana/handover":     {"transfer_to": {"boss"}},
		"/people/ana/sessions/end": {},
		"/people/ana/keys":         {"scopes": {auth.ScopeRead}},
		"/people/ana/reset":        {},
		"/invites/new":             {"handle": {"eve"}},
	}
	unchanged := func(what string) {
		t.Helper()
		ctx := context.Background()
		u := e.peopleUser("ana")
		keys, _ := e.users.ListKeys(ctx, "ana")
		links, _ := e.users.Links(ctx)
		_, err := e.users.GetUser(ctx, "eve")
		if u.DisplayName != "" || u.Role != user.RoleMember || u.DisabledAt != nil || e.peopleOwner("report") != "ana" || len(keys) != 0 || len(links) != 0 ||
			!errors.Is(err, user.ErrNotFound) {
			t.Fatalf("%s changed something: %+v, %d keys, %d links, eve: %v", what, u, len(keys), len(links), err)
		}
	}

	for path, form := range changes {
		for name, bad := range map[string]string{"no form token": "", "a made-up form token": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
			f := url.Values{"form_token": {bad}}
			for k, v := range form {
				f[k] = v
			}
			r := b.postRaw(path, f, e.admin.URL)
			if r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "This form has expired") || peopleLinkRE.MatchString(r.body) || peopleKeyRE.MatchString(r.body) {
				t.Errorf("POST %s with %s: status %d", path, name, r.StatusCode)
			}
			unchanged("POST " + path + " with " + name)
		}
		f := url.Values{"form_token": {token}}
		for k, v := range form {
			f[k] = v
		}
		if r := b.postRaw(path, f, "https://report.art.test"); r.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s from a page of an artifact: status %d, want 403", path, r.StatusCode)
		}
		unchanged("POST " + path + " from an artifact")
	}
	// Nothing changes on a GET of an action.
	for _, path := range []string{"/people/ana/enable", "/people/ana/handover?transfer_to=boss", "/people/ana/sessions/end", "/people/ana/keys"} {
		if r := b.navigate(path); r.StatusCode == http.StatusOK || r.StatusCode == http.StatusSeeOther {
			t.Errorf("GET %s: status %d", path, r.StatusCode)
		}
	}
	unchanged("a GET")

	// A form sent after the admin was demoted, with their own token.
	if _, err := e.users.UpdateUser(asAdmin(), "second", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Fatal(err)
	}
	for path, form := range changes {
		f := url.Values{"form_token": {token}}
		for k, v := range form {
			f[k] = v
		}
		r := b.postRaw(path, f, e.admin.URL)
		if r.StatusCode != http.StatusForbidden || !strings.Contains(r.body, "Not an admin") || peopleLinkRE.MatchString(r.body) || peopleKeyRE.MatchString(r.body) {
			t.Errorf("POST %s after a demotion: status %d: %s", path, r.StatusCode, r.body)
		}
		unchanged("POST " + path + " after a demotion")
	}
}

func TestPeopleHostileNames(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	ctx := context.Background()
	for i, hostile := range peopleHostile {
		handle := "mallory" + strconv.Itoa(i)
		e.person(handle, "")
		if _, err := e.users.UpdateUser(asAdmin(), handle, user.UpdateUserRequest{DisplayName: &hostile, Email: ptr(hostile + "@example.org")}); err != nil {
			t.Fatal(err)
		}
		k, _, err := e.users.IssueKey(ctx, user.IssueKeyRequest{Handle: handle, Label: hostile})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.users.LinkIdentity(asAdmin(), handle, "https://idp.example/"+hostile, hostile, hostile); err != nil {
			t.Fatal(err)
		}
		e.peopleConnectApp(handle, hostile)
		grants, err := e.oauth.ListGrants(ctx, handle)
		if err != nil || len(grants) != 1 {
			t.Fatalf("ListGrants = %+v, %v", grants, err)
		}
		if _, _, err := e.users.IssueLink(asAdmin(), handle, user.PurposeReset, false); err != nil {
			t.Fatal(err)
		}
		links, _ := e.users.Links(ctx)

		b := e.signedIn("boss")
		for _, path := range []string{
			"/people", "/people?q=" + url.QueryEscape(hostile), "/people/" + handle, "/people/" + handle + "/disable", "/people/" + handle + "/reset",
			"/people/" + handle + "/keys/" + k.Prefix + "/revoke", "/people/" + handle + "/keys/" + k.Prefix + "/rotate",
			"/people/" + handle + "/apps/" + strconv.FormatInt(grants[0].FamilyID, 10) + "/revoke",
			"/invites/" + strconv.FormatInt(links[0].ID, 10) + "/revoke", "/people/boss/disable?x=" + url.QueryEscape(hostile),
		} {
			r := b.navigate(path)
			if r.StatusCode != http.StatusOK && path != "/people/boss/disable?x="+url.QueryEscape(hostile) {
				t.Errorf("GET %s: status %d", path, r.StatusCode)
			}
			peopleStrict(t, path, r.body)
			if r.Header.Get("Content-Security-Policy") == "" || r.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("GET %s: CSP %q, Cache-Control %q", path, r.Header.Get("Content-Security-Policy"), r.Header.Get("Cache-Control"))
			}
		}
		// The pages that answer a POST with a secret, and forms sent back.
		b.get("/people/" + handle)
		for path, form := range map[string]url.Values{
			"/people/" + handle + "/keys":                         {"label": {hostile}, "scopes": {auth.ScopeRead}},
			"/people/" + handle + "/keys/" + k.Prefix + "/rotate": {},
			"/people/" + handle + "/reset":                        {},
			"/people/" + handle:                                   {"name": {hostile}, "email": {hostile}, "role": {hostile}, "quota": {hostile}},
			"/invites/new":                                        {"handle": {hostile}, "name": {hostile}, "email": {hostile}, "role": {hostile}, "quota": {hostile}},
			"/people/" + handle + "/handover":                     {"transfer_to": {hostile}},
		} {
			r := b.post(path, form)
			peopleStrict(t, "POST "+path, r.body)
			b.get("/people/" + handle)
		}
	}
}

// A handle that is also a word of the dashboard's own addresses still has
// its page.
func TestPersonNamedLikeARoute(t *testing.T) {
	e := newEnv(t, nil)
	e.person("boss", user.RoleAdmin)
	for _, handle := range []string{"invite", "links", "new"} {
		if _, err := e.users.CreateUser(asAdmin(), user.CreateUserRequest{Handle: handle}); err != nil {
			t.Fatal(err)
		}
	}
	b := e.signedIn("boss")
	for _, handle := range []string{"invite", "links", "new"} {
		r := b.get("/people/" + handle)
		if !strings.Contains(r.body, `<p class="crumb"><a href="/people">People</a> / `+handle+`</p>`) {
			t.Errorf("/people/%s is not that person's page", handle)
		}
		b.submit("/people/"+handle, url.Values{"name": {"Renamed"}, "email": {""}, "role": {"member"}, "quota": {"no limit"}})
		if e.peopleUser(handle).DisplayName != "Renamed" {
			t.Errorf("editing %s did not reach them", handle)
		}
	}
}

func TestPeopleWithoutLocalAccounts(t *testing.T) {
	e := newEnv(t, func(_ *dashboard.Deps, o *dashboard.Options) { o.LocalAccounts = false })
	e.person("boss", user.RoleAdmin)
	b := e.signedIn("boss")
	r := b.get("/people")
	if !strings.Contains(r.body, "Add a person") || strings.Contains(r.body, "Invite a person") || strings.Contains(r.body, "Links waiting to be used") {
		t.Fatalf("the list: %s", r.body)
	}
	if r = b.get("/invites/new"); !strings.Contains(r.body, "People sign in at the identity provider") {
		t.Errorf("the form: %s", r.body)
	}
	r = b.submit("/invites/new", url.Values{"handle": {"ana"}, "name": {"Ana"}})
	if !strings.Contains(r.body, "The person is added.") || peopleLinkRE.MatchString(r.body) || e.peopleUser("ana").DisplayName != "Ana" {
		t.Errorf("adding a person: %s", r.body)
	}
	if strings.Contains(r.body, "reset link") || !strings.Contains(r.body, "Link a sign-in at the provider") {
		t.Errorf("a person's page without built-in accounts: %s", r.body)
	}
	for _, path := range []string{"/people/ana/reset", "/people/ana/invite"} {
		if r := b.navigate(path); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, r.StatusCode)
		}
		if r := b.post(path, nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 404", path, r.StatusCode)
		}
	}

	// Linking and unlinking a sign-in at the provider.
	r = b.submit("/people/ana/identities", url.Values{"issuer": {"https://idp.example"}, "subject": {"sub-1"}, "email": {"ana@idp.example"}})
	if !strings.Contains(r.body, "The provider sign-in is linked.") || !strings.Contains(r.body, "sub-1") {
		t.Errorf("after linking: %s", r.body)
	}
	if r := b.post("/people/ana/identities", url.Values{"issuer": {""}, "subject": {""}}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("linking nothing: status %d, want 422", r.StatusCode)
	}
	r = b.submit("/people/ana/identities/unlink", url.Values{"issuer": {"https://idp.example"}, "subject": {"sub-1"}})
	if ids, _ := e.users.ListIdentities(context.Background(), "ana"); !strings.Contains(r.body, "The provider sign-in is unlinked.") || len(ids) != 0 {
		t.Errorf("after unlinking: %+v", ids)
	}
	if r := b.post("/people/ana/identities/unlink", url.Values{"issuer": {"https://idp.example"}, "subject": {"sub-1"}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("unlinking what is not linked: status %d, want 404", r.StatusCode)
	}
}
