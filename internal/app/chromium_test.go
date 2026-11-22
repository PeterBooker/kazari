package app_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

// chromiumEnv names the Chromium or Chrome command the browser test runs,
// with any extra flags after it: "chromium", "google-chrome --no-sandbox".
const chromiumEnv = "KAZARI_TEST_CHROMIUM"

// A person goes from the setup link to the dashboard in a real browser:
// the first admin with a password, a passkey added on the account page,
// and a second sign-in with that passkey alone.
func TestChromiumSignIn(t *testing.T) {
	command := strings.Fields(os.Getenv(chromiumEnv))
	if len(command) == 0 {
		t.Skipf("%s is not set; set it to a Chromium or Chrome command to run the browser test", chromiumEnv)
	}

	port := freePort(t)
	cfg := config.Local()
	cfg.DataDir = t.TempDir()
	cfg.Listen.HTTP = "127.0.0.1:" + strconv.Itoa(port)
	cfg.ExternalPort = port
	cfg.Admin.UpdateCheck = false
	var stderr bytes.Buffer
	in := startWith(t, cfg, app.Options{Stderr: &stderr})

	apex := "http://kazari.localhost:" + strconv.Itoa(port)
	admin := "http://admin.kazari.localhost:" + strconv.Itoa(port)
	signInHost := "mcp.kazari.localhost:" + strconv.Itoa(port)
	link := regexp.MustCompile(regexp.QuoteMeta(apex) + `/account/setup\?token=kzl_[a-z2-7]+`).FindString(stderr.String())
	if link == "" {
		t.Fatalf("stderr holds no setup link for %s: %q", apex, stderr.String())
	}

	c := startChromium(t, command)
	var authenticator struct {
		ID string `json:"authenticatorId"`
	}
	c.must("WebAuthn.enable", nil, nil)
	c.must("WebAuthn.addVirtualAuthenticator", map[string]any{"options": map[string]any{
		"protocol": "ctap2", "transport": "internal", "hasResidentKey": true, "hasUserVerification": true,
		"isUserVerified": true, "automaticPresenceSimulation": true,
	}}, &authenticator)
	// With a virtual authenticator the autofill request of the sign-in page
	// is answered at once, before the button under test can be pressed.
	c.must("Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": `if (window.PublicKeyCredential) PublicKeyCredential.isConditionalMediationAvailable = () => Promise.resolve(false);`,
	}, nil)

	onSignIn := `location.host === ` + quote(signInHost) + ` && location.pathname === "/oauth/signin"`
	onDashboard := `location.origin === ` + quote(admin) + ` && location.pathname === "/" && ` + heading("Overview")

	c.open(link)
	c.wait("the setup page", `location.pathname === "/account/setup" && !!document.querySelector('form[action="/account/setup"]')`)
	c.shot("1-setup")
	c.fill("#handle", "peter")
	c.fill("#name", "Peter")
	c.fill("#email", "peter@example.com")
	c.fill("#password", testPassword)
	c.fill("#confirm", testPassword)
	c.click(`form[action="/account/setup"] button[type="submit"]`)

	c.wait("the account's welcome after setup", `location.origin === `+quote(apex)+` && location.pathname === "/account/" && `+
		`document.body.textContent.includes("Welcome, Peter")`)
	c.shot("2-welcome")
	c.open(admin + "/")
	c.wait("the dashboard's sign-in page", onSignIn+` && !!document.querySelector("#login")`)
	c.shot("2-sign-in")
	c.fill("#login", "peter")
	c.fill("#password", testPassword)
	c.click(`form[action="/oauth/signin"]:not([data-passkey]) button[type="submit"]`)
	c.wait("the dashboard after the password sign-in", onDashboard)
	c.shot("3-dashboard")

	c.open(apex + "/account/")
	c.wait("the account page with its passkey form shown",
		`location.origin === `+quote(apex)+` && !!document.querySelector('form[data-passkey="create"]:not([hidden])')`)
	c.fill("#passkey-name", "Virtual authenticator")
	c.fill("#passkey-current", testPassword)
	c.click(`form[data-passkey="create"] button[type="submit"]`)
	c.wait("the account page to say the passkey is added",
		`!!document.querySelector(".flash") && document.querySelector(".flash").textContent.includes("The passkey is added")`)
	c.shot("4-passkey-added")
	var held struct {
		Credentials []struct {
			Resident bool `json:"isResidentCredential"`
		} `json:"credentials"`
	}
	c.must("WebAuthn.getCredentials", map[string]any{"authenticatorId": authenticator.ID}, &held)
	if len(held.Credentials) != 1 || !held.Credentials[0].Resident {
		t.Fatalf("the authenticator holds %+v after adding a passkey, want one discoverable credential", held.Credentials)
	}

	c.click(`form[action="/account/signout"] button[type="submit"]`)
	c.wait("the signed-out page", heading("Signed out"))
	c.shot("5-signed-out")

	c.open(admin + "/")
	c.wait("the sign-in page with its passkey form shown", onSignIn+` && !!document.querySelector('form[data-passkey="get"]:not([hidden])')`)
	c.shot("6-sign-in-again")
	c.click(`form[data-passkey="get"] button[type="submit"]`)
	c.wait("the dashboard after the passkey sign-in", onDashboard)
	c.shot("7-dashboard-by-passkey")

	// The page an MCP client's authorisation opens takes the passkey too,
	// and the browser follows the approval back to the client.
	c.click(`form[action="/session/signout"] button[type="submit"]`)
	c.wait("the signed-out page of the dashboard", heading("Signed out"))
	codes := make(chan string, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		select {
		case codes <- r.URL.Query().Get("code"):
		default:
		}
		// The empty icon keeps the browser from asking for a favicon
		// this server does not have, which it would log as an error.
		fmt.Fprint(w, `<!doctype html><title>Connected</title><link rel="icon" href="data:,"><h1>Connected</h1>`)
	}))
	t.Cleanup(callback.Close)
	redirect := callback.URL + "/callback"
	registration, _ := json.Marshal(map[string]any{"client_name": "Routine", "redirect_uris": []string{redirect}})
	resp, err := client(in.app.Addrs().HTTP).Post("http://"+signInHost+"/oauth/register", "application/json", bytes.NewReader(registration))
	if err != nil {
		t.Fatalf("registering a client: %v", err)
	}
	var registered struct {
		ClientID string `json:"client_id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&registered)
	resp.Body.Close()
	if err != nil || registered.ClientID == "" {
		t.Fatalf("registering a client: status %d, client %q, err %v", resp.StatusCode, registered.ClientID, err)
	}
	challenge := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	c.open("http://" + signInHost + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {registered.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}.Encode())
	c.wait("the sign-in page of the authorisation",
		heading("Sign in to connect Routine")+` && !!document.querySelector('form[data-passkey="get"]:not([hidden])')`)
	c.shot("8-authorise-sign-in")
	c.click(`form[data-passkey="get"] button[type="submit"]`)
	c.wait("the consent page", heading("Authorise Routine"))
	c.shot("9-consent")
	c.click(`form[action="/oauth/consent"] button[value="approve"]`)
	c.wait("the client's callback page", `location.origin === `+quote(callback.URL)+` && `+heading("Connected"))
	select {
	case code := <-codes:
		if code == "" {
			t.Error("the client's callback was opened with no authorisation code")
		}
	default:
		t.Error("the browser showed the client's callback page, but the client saw no request")
	}

	local := cli.NewClient("http://kazari", "", socketClient(in.app.Addrs().Socket))
	events, err := local.Audit(t.Context(), cli.AuditParams{Actions: []string{"oauth.signin"}})
	if err != nil {
		t.Fatalf("Audit(oauth.signin): %v", err)
	}
	got := map[string]int{}
	for _, e := range events.Events {
		got[e.Actor+" via "+e.Details["via"]]++
	}
	if diff := cmp.Diff(map[string]int{"peter via password": 1, "peter via passkey": 2}, got); diff != "" {
		t.Errorf("sign-ins in the audit log (-want +got):\n%s", diff)
	}
	for _, problem := range c.problems() {
		t.Errorf("the browser reported: %s", problem)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func poll(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func heading(text string) string {
	return `!!document.querySelector("h1") && document.querySelector("h1").textContent.trim().startsWith(` + quote(text) + `)`
}

const chromiumTimeout = 20 * time.Second

// chromiumStart bounds the first reply: a browser that has not run on the
// machine before, as on a CI runner, can take most of a minute to start.
const chromiumStart = 2 * time.Minute

// chromium is one page of a headless Chromium, driven over the DevTools
// pipe: JSON messages that end with a zero byte.
type chromium struct {
	t       *testing.T
	to      io.Writer
	session string
	closed  chan struct{}

	mu       sync.Mutex
	next     int
	replies  map[int]chan cdpMessage
	reported []string
}

type cdpMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func startChromium(t *testing.T, command []string) *chromium {
	t.Helper()
	home := t.TempDir()
	toChromium, to, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	from, fromChromium, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	args := append(command[1:],
		"--headless=new", "--remote-debugging-pipe", "--user-data-dir="+filepath.Join(home, "profile"),
		"--window-size=1280,1400", "--no-first-run", "--no-default-browser-check", "--password-store=basic",
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions",
		// Nothing but the instance and loopback can be reached.
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE kazari.localhost, EXCLUDE *.kazari.localhost, EXCLUDE 127.0.0.1",
		"about:blank")
	cmd := exec.Command(command[0], args...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.ExtraFiles = []*os.File{toChromium, fromChromium}
	// Without the person's home, Chromium reads no flags file, profile or
	// keyring of theirs.
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	toChromium.Close()
	fromChromium.Close()
	if err != nil {
		to.Close()
		from.Close()
		t.Fatalf("starting %q: %v", command, err)
	}
	c := &chromium{t: t, to: to, closed: make(chan struct{}), replies: map[int]chan cdpMessage{}}
	go c.read(from)
	t.Cleanup(func() {
		exited := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
		_ = c.call("Browser.close", nil, nil)
		select {
		case <-exited:
		case <-time.After(chromiumTimeout):
			t.Errorf("Chromium did not exit within %v of Browser.close", chromiumTimeout)
		}
		// A helper process that outlives the browser would still be
		// writing to the profile when the test removes it.
		group := -cmd.Process.Pid
		_ = syscall.Kill(group, syscall.SIGKILL)
		<-exited
		if !poll(chromiumTimeout, func() bool { return errors.Is(syscall.Kill(group, 0), syscall.ESRCH) }) {
			t.Errorf("Chromium's processes were still running %v after they were killed", chromiumTimeout)
		}
		to.Close()
		from.Close()
		<-c.closed
		if t.Failed() {
			t.Logf("Chromium's output:\n%s", output.String())
		}
	})

	var targets struct {
		Infos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	began := time.Now()
	if err := c.callWithin(chromiumStart, "Target.getTargets", nil, &targets); err != nil {
		t.Fatal(err)
	}
	t.Logf("Chromium answered %v after it was started", time.Since(began).Round(time.Millisecond))
	var attached struct {
		Session string `json:"sessionId"`
	}
	for _, target := range targets.Infos {
		if target.Type == "page" {
			c.must("Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached)
			break
		}
	}
	if attached.Session == "" {
		t.Fatalf("Chromium opened no page: targets %+v", targets.Infos)
	}
	c.session = attached.Session
	c.must("Page.enable", nil, nil)
	c.must("Runtime.enable", nil, nil)
	c.must("Log.enable", nil, nil)
	return c
}

func (c *chromium) read(from io.Reader) {
	defer close(c.closed)
	r := bufio.NewReader(from)
	for {
		raw, err := r.ReadBytes(0)
		if err != nil {
			return
		}
		var m cdpMessage
		if err := json.Unmarshal(raw[:len(raw)-1], &m); err != nil {
			c.report("a message that is not JSON: " + err.Error())
			continue
		}
		c.mu.Lock()
		reply := c.replies[m.ID]
		delete(c.replies, m.ID)
		c.mu.Unlock()
		switch {
		case reply != nil:
			reply <- m
		case m.Method == "Runtime.exceptionThrown":
			c.report("an uncaught exception: " + string(m.Params))
		case m.Method == "Log.entryAdded":
			var p struct {
				Entry struct {
					Source, Level, Text, URL string
				} `json:"entry"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.Entry.Level == "error" {
				c.report(fmt.Sprintf("%s error at %s: %s", p.Entry.Source, p.Entry.URL, p.Entry.Text))
			}
		}
	}
}

func (c *chromium) report(problem string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reported = append(c.reported, problem)
}

func (c *chromium) problems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reported...)
}

func (c *chromium) call(method string, params, out any) error {
	return c.callWithin(chromiumTimeout, method, params, out)
}

func (c *chromium) callWithin(timeout time.Duration, method string, params, out any) error {
	reply := make(chan cdpMessage, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.replies[id] = reply
	c.mu.Unlock()
	message := map[string]any{"id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	if c.session != "" {
		message["sessionId"] = c.session
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := c.to.Write(append(raw, 0)); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m := <-reply:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(m.Result, out)
	case <-c.closed:
		return fmt.Errorf("%s: Chromium closed the pipe", method)
	case <-time.After(timeout):
		return fmt.Errorf("%s: no reply within %v", method, timeout)
	}
}

func (c *chromium) must(method string, params, out any) {
	c.t.Helper()
	if err := c.call(method, params, out); err != nil {
		c.t.Fatal(err)
	}
}

func (c *chromium) eval(script string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call("Runtime.evaluate", map[string]any{"expression": script, "returnByValue": true}, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return fmt.Errorf("%s: %s", script, r.Exception.Text)
	}
	return json.Unmarshal(r.Result.Value, out)
}

func (c *chromium) open(url string) {
	c.t.Helper()
	var r struct {
		ErrorText string `json:"errorText"`
	}
	c.must("Page.navigate", map[string]any{"url": url}, &r)
	if r.ErrorText != "" {
		c.t.Fatalf("opening %s: %s", url, r.ErrorText)
	}
}

func (c *chromium) wait(what, script string) {
	c.t.Helper()
	if poll(chromiumTimeout, func() bool {
		// An error is not a failure here: a page that is still loading
		// has no context to run the script in.
		var ok bool
		return c.eval(script, &ok) == nil && ok
	}) {
		return
	}
	var at struct{ URL, Text string }
	_ = c.eval(`({URL: location.href, Text: document.body ? document.body.innerText.slice(0, 600) : ""})`, &at)
	c.shot("failed")
	c.t.Fatalf("timed out after %v waiting for %s; the browser is at %s showing:\n%s\nproblems: %q", chromiumTimeout, what, at.URL, at.Text, c.problems())
}

func (c *chromium) fill(selector, value string) {
	c.t.Helper()
	var found bool
	script := `(() => { const e = document.querySelector(` + quote(selector) + `); if (!e) return false; e.value = ` + quote(value) + `; return true; })()`
	if err := c.eval(script, &found); err != nil || !found {
		c.t.Fatalf("filling %s: found %v, err %v", selector, found, err)
	}
}

func (c *chromium) click(selector string) {
	c.t.Helper()
	var found bool
	script := `(() => { const e = document.querySelector(` + quote(selector) + `); if (!e) return false; e.click(); return true; })()`
	if err := c.eval(script, &found); err != nil || !found {
		c.t.Fatalf("clicking %s: found %v, err %v", selector, found, err)
	}
}

func (c *chromium) shot(name string) {
	c.t.Helper()
	var r struct {
		Data string `json:"data"`
	}
	if err := c.call("Page.captureScreenshot", map[string]any{"format": "png"}, &r); err != nil {
		c.t.Errorf("screenshot %s: %v", name, err)
		return
	}
	png, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		c.t.Errorf("screenshot %s: %v", name, err)
		return
	}
	if err := os.WriteFile(filepath.Join(c.t.ArtifactDir(), name+".png"), png, 0o644); err != nil {
		c.t.Errorf("screenshot %s: %v", name, err)
	}
}
