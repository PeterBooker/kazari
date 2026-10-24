package user_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/audit"
	"kazari/internal/auth"
	"kazari/internal/sqlite"
	"kazari/internal/user"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	svc   *user.Service
	store *sqlite.UserStore
	audit *audit.Memory
	clock *clock
	opts  user.Options
}

func newFixture(t *testing.T, opts user.Options) *fixture {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{store: sqlite.NewUserStore(db), audit: &audit.Memory{}, clock: &clock{t: t0}}
	opts.Now = f.clock.Now
	f.opts = opts
	f.svc = user.New(f.store, f.audit, opts)
	return f
}

// restart returns a fresh Service over the same store, as after a process
// restart.
func (f *fixture) restart() *user.Service {
	return user.New(f.store, f.audit, f.opts)
}

func (f *fixture) mustCreate(t *testing.T, handle, email string, role user.Role) *user.User {
	t.Helper()
	u, err := f.svc.CreateUser(context.Background(), user.CreateUserRequest{Handle: handle, Email: email, Role: role})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", handle, err)
	}
	return u
}

func (f *fixture) mustIssue(t *testing.T, handle string, scopes []string, expiresIn time.Duration) (*user.APIKey, string) {
	t.Helper()
	k, secret, err := f.svc.IssueKey(context.Background(), handle, "test", scopes, expiresIn)
	if err != nil {
		t.Fatalf("IssueKey(%q): %v", handle, err)
	}
	return k, secret
}

func (f *fixture) lastEvent(t *testing.T) audit.Event {
	t.Helper()
	if len(f.audit.Events) == 0 {
		t.Fatal("no audit events")
	}
	return f.audit.Events[len(f.audit.Events)-1]
}

func ptr[T any](v T) *T { return &v }

func TestValidateHandle(t *testing.T) {
	tests := []struct {
		handle string
		ok     bool
	}{
		{"ab", true},
		{"alice", true},
		{"a1_b-c", true},
		{"0day", true},
		{strings.Repeat("a", 32), true},
		{"a", false},
		{"", false},
		{strings.Repeat("a", 33), false},
		{"-alice", false},
		{"_alice", false},
		{"Alice", false},
		{"al ice", false},
		{"al.ice", false},
		{"álice", false},
	}
	for _, tt := range tests {
		err := user.ValidateHandle(tt.handle)
		if tt.ok && err != nil {
			t.Errorf("ValidateHandle(%q) = %v, want nil", tt.handle, err)
		}
		if !tt.ok && !errors.Is(err, user.ErrInvalidHandle) {
			t.Errorf("ValidateHandle(%q) = %v, want ErrInvalidHandle", tt.handle, err)
		}
	}
}

func TestCreateUser(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{DefaultQuota: 1000})

	u, err := f.svc.CreateUser(ctx, user.CreateUserRequest{
		Handle: "alice", Email: "  Alice@Example.COM ", DisplayName: " Alice ",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &user.User{ID: u.ID, Handle: "alice", Email: "alice@example.com", DisplayName: "Alice",
		Role: user.RoleMember, QuotaBytes: 1000, CreatedAt: t0}
	if diff := cmp.Diff(want, u); diff != "" {
		t.Errorf("CreateUser (-want +got):\n%s", diff)
	}
	got, err := f.svc.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetUser (-want +got):\n%s", diff)
	}
	ev := f.lastEvent(t)
	if ev.Action != audit.UserCreate || ev.Actor != "admin" || ev.Details["handle"] != "alice" || !ev.At.Equal(t0) {
		t.Errorf("audit event = %+v", ev)
	}

	// Explicit role and quota (zero is a real value, not "default"), and
	// the actor comes from the principal in the context.
	pctx := auth.ContextWithPrincipal(ctx, auth.Principal{Handle: "root", Admin: true})
	b, err := f.svc.CreateUser(pctx, user.CreateUserRequest{Handle: "bob", Role: user.RoleAdmin, QuotaBytes: ptr(int64(0))})
	if err != nil {
		t.Fatal(err)
	}
	if b.Role != user.RoleAdmin || b.QuotaBytes != 0 {
		t.Errorf("bob = %+v, want admin with quota 0", b)
	}
	if ev := f.lastEvent(t); ev.Actor != "root" {
		t.Errorf("audit actor = %q, want root", ev.Actor)
	}

	users, err := f.svc.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Handle != "alice" || users[1].Handle != "bob" {
		t.Errorf("ListUsers = %+v", users)
	}
}

func TestCreateUserErrors(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "alice@example.com", "")
	events := len(f.audit.Events)
	tests := []struct {
		name string
		req  user.CreateUserRequest
		want error
	}{
		{"duplicate handle", user.CreateUserRequest{Handle: "alice"}, user.ErrHandleTaken},
		{"duplicate email", user.CreateUserRequest{Handle: "alice2", Email: "ALICE@example.com"}, user.ErrEmailTaken},
		{"short handle", user.CreateUserRequest{Handle: "a"}, user.ErrInvalidHandle},
		{"upper-case handle", user.CreateUserRequest{Handle: "Alice"}, user.ErrInvalidHandle},
		{"bad role", user.CreateUserRequest{Handle: "carol", Role: "owner"}, user.ErrInvalidRole},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.svc.CreateUser(ctx, tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("CreateUser = %v, want %v", err, tt.want)
			}
		})
	}
	if len(f.audit.Events) != events {
		t.Errorf("failed creates recorded %d audit events", len(f.audit.Events)-events)
	}
}

func TestUpdateUser(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "alice@example.com", "")
	f.mustCreate(t, "bob", "bob@example.com", "")

	u, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{
		Email: ptr(" NEW@example.com"), DisplayName: ptr("Alice A"), Role: ptr(user.RoleAdmin), QuotaBytes: ptr(int64(5)),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.GetUser(ctx, "alice")
	if diff := cmp.Diff(u, got); diff != "" {
		t.Errorf("stored user differs from returned (-returned +stored):\n%s", diff)
	}
	if got.Email != "new@example.com" || got.DisplayName != "Alice A" || got.Role != user.RoleAdmin || got.QuotaBytes != 5 {
		t.Errorf("updated user = %+v", got)
	}
	if ev := f.lastEvent(t); ev.Action != audit.UserUpdate || ev.Details["role"] != "admin" {
		t.Errorf("audit event = %+v", ev)
	}

	tests := []struct {
		name   string
		handle string
		req    user.UpdateUserRequest
		want   error
	}{
		{"unknown user", "nobody", user.UpdateUserRequest{}, user.ErrNotFound},
		{"bad role", "alice", user.UpdateUserRequest{Role: ptr(user.Role("root"))}, user.ErrInvalidRole},
		{"taken email", "alice", user.UpdateUserRequest{Email: ptr("Bob@example.com")}, user.ErrEmailTaken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.svc.UpdateUser(ctx, tt.handle, tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("UpdateUser = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDisableRevokesKeys(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	_, k1 := f.mustIssue(t, "alice", nil, 0)
	_, k2 := f.mustIssue(t, "alice", nil, 0)
	if _, err := f.svc.Authenticate(ctx, k1); err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(time.Hour)
	u, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if u.DisabledAt == nil || !u.DisabledAt.Equal(f.clock.Now()) {
		t.Errorf("DisabledAt = %v, want %v", u.DisabledAt, f.clock.Now())
	}
	keys, err := f.svc.ListKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.RevokedAt == nil {
			t.Errorf("key %s not revoked on disable", k.Prefix)
		}
	}
	for _, k := range []string{k1, k2} {
		if _, err := f.svc.Authenticate(ctx, k); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("Authenticate after disable = %v, want ErrInvalidToken", err)
		}
	}
	if _, _, err := f.svc.IssueKey(ctx, "alice", "", nil, 0); !errors.Is(err, user.ErrDisabled) {
		t.Errorf("IssueKey for disabled user = %v, want ErrDisabled", err)
	}

	// Enabling clears DisabledAt; revoked keys stay revoked, new keys work.
	u, err = f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if u.DisabledAt != nil {
		t.Errorf("DisabledAt after enable = %v", u.DisabledAt)
	}
	if _, err := f.svc.Authenticate(ctx, k1); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("old key after enable = %v, want ErrInvalidToken", err)
	}
	_, k3 := f.mustIssue(t, "alice", nil, 0)
	if _, err := f.svc.Authenticate(ctx, k3); err != nil {
		t.Errorf("new key after enable: %v", err)
	}
}

// Regression: disabling a user revoked their keys but nothing else, so
// enabling them again revived their OAuth grants. OnDisable lets the
// holder of other credentials revoke them.
func TestDisableRunsOnDisable(t *testing.T) {
	ctx := context.Background()
	var calls []string
	var fail error
	f := newFixture(t, user.Options{OnDisable: func(_ context.Context, handle string) error {
		calls = append(calls, handle)
		return fail
	}})
	f.mustCreate(t, "alice", "", "")

	if _, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{DisplayName: ptr("Alice")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"alice"}, calls); diff != "" {
		t.Errorf("OnDisable calls (-want +got):\n%s", diff)
	}

	// A failed hook fails the update; the user is disabled all the same.
	fail = errors.New("grants unavailable")
	if _, err := f.svc.UpdateUser(ctx, "alice", user.UpdateUserRequest{Disabled: ptr(true)}); !errors.Is(err, fail) {
		t.Errorf("UpdateUser with failing OnDisable = %v, want its error", err)
	}
	if u, _ := f.svc.GetUser(ctx, "alice"); u.DisabledAt == nil {
		t.Error("user not disabled after a failed OnDisable")
	}
}

func TestKeyStatus(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	f.mustCreate(t, "bob", "", "")
	live, _ := f.mustIssue(t, "alice", nil, 0)
	expiring, _ := f.mustIssue(t, "alice", nil, time.Hour)
	revoked, _ := f.mustIssue(t, "alice", nil, 0)
	if err := f.svc.RevokeKey(ctx, "alice", revoked.Prefix); err != nil {
		t.Fatal(err)
	}
	// Disabled in the store directly, so bob's key itself stays unrevoked.
	bobs, _ := f.mustIssue(t, "bob", nil, 0)
	if err := f.store.UpdateUser(ctx, &user.User{Handle: "bob", Role: user.RoleMember, DisabledAt: ptr(t0)}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		prefix  string
		advance time.Duration
		want    user.KeyStatus
	}{
		{"live", live.Prefix, 0, user.KeyStatus{Handle: "alice", Active: true}},
		{"before expiry", expiring.Prefix, 0, user.KeyStatus{Handle: "alice", Active: true, ExpiresAt: expiring.ExpiresAt}},
		{"revoked", revoked.Prefix, 0, user.KeyStatus{Handle: "alice"}},
		{"disabled owner", bobs.Prefix, 0, user.KeyStatus{Handle: "bob"}},
		{"at expiry", expiring.Prefix, time.Hour, user.KeyStatus{Handle: "alice", ExpiresAt: expiring.ExpiresAt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.clock.Advance(tt.advance)
			defer f.clock.Advance(-tt.advance)
			got, err := f.svc.KeyStatus(ctx, tt.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("KeyStatus (-want +got):\n%s", diff)
			}
		})
	}
	if _, err := f.svc.KeyStatus(ctx, "zzzzzzzz"); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("KeyStatus(unknown) = %v, want ErrKeyNotFound", err)
	}
}

var keyRE = regexp.MustCompile(`^kzk_([a-z2-7]{8})_[a-z2-7]{52}$`)

func TestIssueKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")

	k, secret, err := f.svc.IssueKey(ctx, "alice", " laptop ", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := keyRE.FindStringSubmatch(secret)
	if m == nil {
		t.Fatalf("key %q does not match %s", secret, keyRE)
	}
	if len(secret) != user.KeyLen || user.KeyLen != 4+8+1+52 {
		t.Errorf("len(key) = %d, KeyLen = %d, want 65", len(secret), user.KeyLen)
	}
	if m[1] != k.Prefix {
		t.Errorf("key prefix %q, APIKey.Prefix %q", m[1], k.Prefix)
	}
	sum := sha256.Sum256([]byte(secret))
	if k.Hash != hex.EncodeToString(sum[:]) {
		t.Errorf("Hash = %q, want sha256 of the whole key", k.Hash)
	}
	exp := t0.Add(24 * time.Hour)
	want := &user.APIKey{ID: k.ID, UserHandle: "alice", Prefix: k.Prefix, Hash: k.Hash, Label: "laptop",
		Scopes: auth.MemberScopes, ExpiresAt: &exp, CreatedAt: t0}
	if diff := cmp.Diff(want, k); diff != "" {
		t.Errorf("IssueKey (-want +got):\n%s", diff)
	}
	keys, err := f.svc.ListKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]user.APIKey{*want}, keys); diff != "" {
		t.Errorf("ListKeys (-want +got):\n%s", diff)
	}
	ev := f.lastEvent(t)
	wantDetails := map[string]string{"handle": "alice", "prefix": k.Prefix, "label": "laptop",
		"scopes": "artifacts:read artifacts:write artifacts:delete"}
	if ev.Action != audit.KeyIssue || !cmp.Equal(ev.Details, wantDetails) {
		t.Errorf("audit event = %+v, want details %v", ev, wantDetails)
	}
	if strings.Contains(strings.Join([]string{ev.Details["prefix"], ev.Details["label"], ev.Details["scopes"]}, " "), secret) {
		t.Error("audit details contain the secret")
	}

	// No expiry, and two keys never share a prefix or secret.
	k2, secret2 := f.mustIssue(t, "alice", nil, 0)
	if k2.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil for expiresIn 0", k2.ExpiresAt)
	}
	if k2.Prefix == k.Prefix || secret2 == secret {
		t.Error("two keys collided")
	}

	if _, _, err := f.svc.IssueKey(ctx, "nobody", "", nil, 0); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("IssueKey(unknown) = %v, want ErrNotFound", err)
	}
}

func TestIssueKeyScopes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "member", "", user.RoleMember)
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	tests := []struct {
		name    string
		handle  string
		scopes  []string
		want    []string
		wantErr error
	}{
		{"member default", "member", nil, auth.MemberScopes, nil},
		{"admin default", "boss", nil, auth.MemberScopes, nil},
		{"member read", "member", []string{auth.ScopeRead}, []string{auth.ScopeRead}, nil},
		{"canonical order, deduplicated", "member",
			[]string{auth.ScopeDelete, auth.ScopeRead, auth.ScopeDelete}, []string{auth.ScopeRead, auth.ScopeDelete}, nil},
		{"admin may hold admin", "boss", []string{auth.ScopeAdmin, auth.ScopeRead}, []string{auth.ScopeRead, auth.ScopeAdmin}, nil},
		{"member may not hold admin", "member", []string{auth.ScopeRead, auth.ScopeAdmin}, nil, user.ErrInvalidScope},
		{"unknown scope", "boss", []string{"artifacts:everything"}, nil, user.ErrInvalidScope},
		{"empty scope string", "member", []string{""}, nil, user.ErrInvalidScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _, err := f.svc.IssueKey(ctx, tt.handle, "", tt.scopes, 0)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("IssueKey = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, k.Scopes); diff != "" {
				t.Errorf("scopes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestScopesFor(t *testing.T) {
	if got := user.ScopesFor(&user.User{Role: user.RoleMember}); !cmp.Equal(got, auth.MemberScopes) {
		t.Errorf("member scopes = %v", got)
	}
	got := user.ScopesFor(&user.User{Role: user.RoleAdmin})
	if !cmp.Equal(got, auth.AllScopes) {
		t.Errorf("admin scopes = %v", got)
	}
	got[0] = "mutated"
	if auth.AllScopes[0] == "mutated" {
		t.Error("ScopesFor returned the shared slice")
	}
}

func TestAuthenticate(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	if _, err := f.svc.CreateUser(ctx, user.CreateUserRequest{Handle: "boss", Role: user.RoleAdmin, QuotaBytes: ptr(int64(99))}); err != nil {
		t.Fatal(err)
	}
	k, secret := f.mustIssue(t, "boss", []string{auth.ScopeRead, auth.ScopeAdmin}, 0)
	p, err := f.svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	want := auth.Principal{Handle: "boss", Admin: true, Scopes: []string{auth.ScopeRead, auth.ScopeAdmin},
		Source: auth.SourceAPIKey, KeyID: k.Prefix, QuotaBytes: 99}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("Principal (-want +got):\n%s", diff)
	}

	// Through a chain, as the app wires it.
	if _, err := auth.Chain(f.svc).Authenticate(ctx, secret); err != nil {
		t.Errorf("Chain: %v", err)
	}

	// A member key carries Admin false.
	f.mustCreate(t, "member", "", "")
	_, ms := f.mustIssue(t, "member", []string{auth.ScopeRead}, 0)
	mp, err := f.svc.Authenticate(ctx, ms)
	if err != nil {
		t.Fatal(err)
	}
	if mp.Admin || mp.HasScope(auth.ScopeWrite) || !mp.HasScope(auth.ScopeRead) {
		t.Errorf("member principal = %+v", mp)
	}
}

func TestAuthenticateDemotedAdminLosesAdminScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	_, secret := f.mustIssue(t, "boss", auth.AllScopes, 0)
	if _, err := f.svc.UpdateUser(ctx, "boss", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Admin || p.HasScope(auth.ScopeAdmin) {
		t.Errorf("demoted principal = %+v, want no admin", p)
	}
	if diff := cmp.Diff(auth.MemberScopes, p.Scopes); diff != "" {
		t.Errorf("scopes (-want +got):\n%s", diff)
	}
}

func TestAuthenticateFailures(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	f.mustCreate(t, "bob", "", "")
	live, liveKey := f.mustIssue(t, "alice", nil, 0)
	revoked, revokedKey := f.mustIssue(t, "alice", nil, 0)
	if err := f.svc.RevokeKey(ctx, "alice", revoked.Prefix); err != nil {
		t.Fatal(err)
	}
	_, expiringKey := f.mustIssue(t, "alice", nil, time.Hour)
	_, bobKey := f.mustIssue(t, "bob", nil, 0)
	if _, err := f.svc.UpdateUser(ctx, "bob", user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour) // the expiring key expires exactly now

	_, secretOfLive, _ := strings.Cut(strings.TrimPrefix(liveKey, "kzk_"), "_")
	wrongSecret := "kzk_" + live.Prefix + "_" + strings.Repeat("a", 52)
	unknownPrefix := "kzk_zzzzzzzz_" + secretOfLive

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no kzk prefix", strings.TrimPrefix(liveKey, "kzk_")},
		{"other scheme", "xyz_" + strings.TrimPrefix(liveKey, "kzk_")},
		{"upper case", strings.ToUpper(liveKey)},
		{"short prefix", "kzk_abc_" + secretOfLive},
		{"short secret", liveKey[:len(liveKey)-1]},
		{"long secret", liveKey + "a"},
		{"no separator", strings.Replace(liveKey, "_", "", 2)},
		{"bad alphabet", liveKey[:len(liveKey)-1] + "1"},
		{"unknown prefix", unknownPrefix},
		{"wrong secret", wrongSecret},
		{"revoked", revokedKey},
		{"expired", expiringKey},
		{"disabled user", bobKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := f.svc.Authenticate(ctx, tt.token)
			if !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("Authenticate = %+v, %v; want ErrInvalidToken", p, err)
			}
		})
	}
	if _, err := f.svc.Authenticate(ctx, liveKey); err != nil {
		t.Errorf("live key: %v", err)
	}
}

// failingStore makes GetKey fail with a storage error.
type failingStore struct {
	*sqlite.UserStore
	err error
}

func (s failingStore) GetKey(context.Context, string) (*user.APIKey, *user.User, error) {
	return nil, nil, s.err
}

func TestAuthenticateStoreError(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	_, secret := f.mustIssue(t, "alice", nil, 0)

	boom := errors.New("disk on fire")
	svc := user.New(failingStore{f.store, boom}, nil, user.Options{})
	_, err := svc.Authenticate(ctx, secret)
	if !errors.Is(err, boom) || errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate = %v, want the store error", err)
	}
	// A storage error stops a chain instead of falling through.
	called := false
	next := auth.AuthenticatorFunc(func(context.Context, string) (auth.Principal, error) {
		called = true
		return auth.Principal{}, nil
	})
	if _, err := auth.Chain(svc, next).Authenticate(ctx, secret); !errors.Is(err, boom) || called {
		t.Errorf("Chain = %v (next called %v), want the store error", err, called)
	}
}

func TestAuthenticateLastUsedThrottle(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	k, secret := f.mustIssue(t, "alice", nil, 0)

	lastUsed := func() *time.Time {
		t.Helper()
		got, _, err := f.store.GetKey(ctx, k.Prefix)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastUsedAt
	}
	use := func(svc *user.Service) {
		t.Helper()
		if _, err := svc.Authenticate(ctx, secret); err != nil {
			t.Fatal(err)
		}
	}
	if lu := lastUsed(); lu != nil {
		t.Fatalf("LastUsedAt before use = %v", lu)
	}

	steps := []struct {
		advance time.Duration
		svc     func() *user.Service
		want    time.Time
	}{
		{0, func() *user.Service { return f.svc }, t0},                            // first use writes
		{30 * time.Second, func() *user.Service { return f.svc }, t0},             // within a minute: no write
		{29 * time.Second, func() *user.Service { return f.svc }, t0},             // 59s: still no write
		{time.Second, func() *user.Service { return f.svc }, t0.Add(time.Minute)}, // 60s: write
		{10 * time.Second, f.restart, t0.Add(time.Minute)},                        // restart honours stored value
		{time.Minute, f.restart, t0.Add(2*time.Minute + 10*time.Second)},          // restart, a minute later: write
	}
	for i, st := range steps {
		f.clock.Advance(st.advance)
		use(st.svc())
		if lu := lastUsed(); lu == nil || !lu.Equal(st.want) {
			t.Errorf("step %d: LastUsedAt = %v, want %v", i, lu, st.want)
		}
	}
}

func TestRevokeKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "", "")
	f.mustCreate(t, "bob", "", "")
	k, secret := f.mustIssue(t, "alice", nil, 0)

	if err := f.svc.RevokeKey(ctx, "bob", k.Prefix); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("RevokeKey by another user = %v, want ErrKeyNotFound", err)
	}
	f.clock.Advance(time.Minute)
	if err := f.svc.RevokeKey(ctx, "alice", k.Prefix); err != nil {
		t.Fatal(err)
	}
	ev := f.lastEvent(t)
	if ev.Action != audit.KeyRevoke || ev.Details["prefix"] != k.Prefix || ev.Details["handle"] != "alice" {
		t.Errorf("audit event = %+v", ev)
	}
	keys, _ := f.svc.ListKeys(ctx, "alice")
	if len(keys) != 1 || keys[0].RevokedAt == nil || !keys[0].RevokedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("keys after revoke = %+v", keys)
	}
	if _, err := f.svc.Authenticate(ctx, secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Authenticate after revoke = %v, want ErrInvalidToken", err)
	}
}

func TestDeriveHandle(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"alice@example.com", "alice"},
		{"Alice.Smith@Example.com", "alice-smith"},
		{"a_b-c@x.io", "a_b-c"},
		{"..john..@x.io", "john"},
		{"_x_@x.io", "x_"},
		{"jo+tag@x.io", "jo-tag"},
		{"zoë@x.io", "zo"},
		{"x@x.io", "user"},
		{"@x.io", "user"},
		{"---@x.io", "user"},
		{strings.Repeat("a", 40) + "@x.io", strings.Repeat("a", 32)},
		{strings.Repeat("a", 31) + ".b@x.io", strings.Repeat("a", 31)}, // trailing "-" after truncation is trimmed
	}
	for _, tt := range tests {
		got := user.DeriveHandle(tt.email)
		if got != tt.want {
			t.Errorf("DeriveHandle(%q) = %q, want %q", tt.email, got, tt.want)
		}
		if err := user.ValidateHandle(got); err != nil {
			t.Errorf("DeriveHandle(%q) = %q is not a valid handle: %v", tt.email, got, err)
		}
	}
}

const issuer = "https://idp.example"

func identity(subject, email string, verified bool) auth.Identity {
	return auth.Identity{Issuer: issuer, Subject: subject, Email: email, EmailVerified: verified, Name: "Person " + subject}
}

func TestResolveIdentity(t *testing.T) {
	type setup func(t *testing.T, f *fixture)
	existing := func(handle, email string) setup {
		return func(t *testing.T, f *fixture) { f.mustCreate(t, handle, email, "") }
	}
	disabled := func(handle, email string) setup {
		return func(t *testing.T, f *fixture) {
			f.mustCreate(t, handle, email, "")
			if _, err := f.svc.UpdateUser(context.Background(), handle, user.UpdateUserRequest{Disabled: ptr(true)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	linked := func(handle, subject string) setup {
		return func(t *testing.T, f *fixture) {
			f.mustCreate(t, handle, "", "")
			if err := f.store.LinkIdentity(context.Background(), handle,
				user.Identity{Issuer: issuer, Subject: subject, LastLoginAt: t0}); err != nil {
				t.Fatal(err)
			}
		}
	}
	both := func(a, b setup) setup {
		return func(t *testing.T, f *fixture) { a(t, f); b(t, f) }
	}
	auto := user.Options{AutoProvision: true, AllowedDomains: []string{"example.com"}, DefaultQuota: 77}

	tests := []struct {
		name     string
		opts     user.Options
		setup    setup
		id       auth.Identity
		wantUser string
		wantRole user.Role
		wantErr  error
	}{
		{"linked identity", user.Options{}, linked("alice", "s1"),
			identity("s1", "", false), "alice", user.RoleMember, nil},
		{"linked identity wins over email", user.Options{},
			both(linked("alice", "s1"), existing("bob", "bob@example.com")),
			identity("s1", "bob@example.com", true), "alice", user.RoleMember, nil},
		{"linked but disabled", user.Options{},
			both(disabled("alice", ""), func(t *testing.T, f *fixture) {
				if err := f.store.LinkIdentity(context.Background(), "alice",
					user.Identity{Issuer: issuer, Subject: "s1", LastLoginAt: t0}); err != nil {
					t.Fatal(err)
				}
			}),
			identity("s1", "", false), "", "", user.ErrDisabled},
		{"email link, allowed domain", user.Options{AllowedDomains: []string{"Example.COM"}},
			existing("alice", "alice@example.com"),
			identity("s1", "ALICE@example.com", true), "alice", user.RoleMember, nil},
		{"email link, no domain restriction", user.Options{},
			existing("alice", "alice@elsewhere.org"),
			identity("s1", "alice@elsewhere.org", true), "alice", user.RoleMember, nil},
		{"email link, unverified", user.Options{},
			existing("alice", "alice@example.com"),
			identity("s1", "alice@example.com", false), "", "", user.ErrNotProvisioned},
		{"email link, domain not allowed", user.Options{AllowedDomains: []string{"corp.example"}},
			existing("alice", "alice@example.com"),
			identity("s1", "alice@example.com", true), "", "", user.ErrNotProvisioned},
		{"email link, disabled user", user.Options{},
			disabled("alice", "alice@example.com"),
			identity("s1", "alice@example.com", true), "", "", user.ErrDisabled},
		{"email link preferred over provisioning", auto,
			existing("alice", "alice@example.com"),
			identity("s1", "alice@example.com", true), "alice", user.RoleMember, nil},
		{"provision member", auto, nil,
			identity("s1", "New.Person@Example.com", true), "new-person", user.RoleMember, nil},
		{"provision admin email, case-insensitive",
			user.Options{AutoProvision: true, AllowedDomains: []string{"example.com"}, AdminEmails: []string{" Boss@EXAMPLE.com"}},
			nil, identity("s1", "boss@example.com", true), "boss", user.RoleAdmin, nil},
		{"provision off", user.Options{AllowedDomains: []string{"example.com"}}, nil,
			identity("s1", "new@example.com", true), "", "", user.ErrNotProvisioned},
		{"provision, no allowed domains", user.Options{AutoProvision: true}, nil,
			identity("s1", "new@example.com", true), "", "", user.ErrNotProvisioned},
		{"provision, domain not allowed", auto, nil,
			identity("s1", "new@evil.example", true), "", "", user.ErrNotProvisioned},
		{"provision, subdomain not allowed", auto, nil,
			identity("s1", "new@sub.example.com", true), "", "", user.ErrNotProvisioned},
		{"provision, unverified", auto, nil,
			identity("s1", "new@example.com", false), "", "", user.ErrNotProvisioned},
		{"provision, no email", auto, nil,
			identity("s1", "", true), "", "", user.ErrNotProvisioned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, tt.opts)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			f.clock.Advance(time.Hour)
			users, _ := f.svc.ListUsers(ctx)
			before := len(users)

			u, err := f.svc.ResolveIdentity(ctx, tt.id)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ResolveIdentity = %+v, %v; want %v", u, err, tt.wantErr)
				}
				if _, err := f.store.GetIdentity(ctx, tt.id.Issuer, tt.id.Subject); tt.setup == nil && !errors.Is(err, user.ErrNotFound) {
					t.Errorf("refused identity was linked (GetIdentity err = %v)", err)
				}
				if users, _ := f.svc.ListUsers(ctx); len(users) != before {
					t.Errorf("refused identity created a user")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.Handle != tt.wantUser || u.Role != tt.wantRole {
				t.Errorf("user = %s (%s), want %s (%s)", u.Handle, u.Role, tt.wantUser, tt.wantRole)
			}
			// The link exists and was refreshed now.
			ids, err := f.store.ListIdentities(ctx, u.Handle)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, id := range ids {
				if id.Issuer == tt.id.Issuer && id.Subject == tt.id.Subject {
					found = true
					if !id.LastLoginAt.Equal(f.clock.Now()) || id.EmailAtLogin != tt.id.Email {
						t.Errorf("identity link = %+v, want last login %v and email %q", id, f.clock.Now(), tt.id.Email)
					}
				}
			}
			if !found {
				t.Errorf("identity not linked to %s: %+v", u.Handle, ids)
			}
		})
	}
}

func TestResolveIdentityProvisioned(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{AutoProvision: true, AllowedDomains: []string{"example.com"}, DefaultQuota: 77})
	id := identity("s1", " Carol@Example.com", true)
	u, err := f.svc.ResolveIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := &user.User{ID: u.ID, Handle: "carol", Email: "carol@example.com", DisplayName: "Person s1",
		Role: user.RoleMember, QuotaBytes: 77, CreatedAt: t0}
	if diff := cmp.Diff(want, u); diff != "" {
		t.Errorf("provisioned user (-want +got):\n%s", diff)
	}
	ev := f.lastEvent(t)
	if ev.Action != audit.UserCreate || ev.Actor != "carol" || ev.Details["via"] != "identity" {
		t.Errorf("audit event = %+v", ev)
	}

	// The second sign-in matches the link and creates nothing.
	f.clock.Advance(time.Hour)
	again, err := f.svc.ResolveIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID {
		t.Errorf("second sign-in resolved to %s, want %s", again.Handle, u.Handle)
	}
	users, _ := f.svc.ListUsers(ctx)
	if len(users) != 1 {
		t.Errorf("%d users after second sign-in, want 1", len(users))
	}
	ids, _ := f.store.ListIdentities(ctx, "carol")
	if len(ids) != 1 || !ids[0].LastLoginAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("identities = %+v, want last login moved to %v", ids, t0.Add(time.Hour))
	}
}

func TestResolveIdentityHandleSuffix(t *testing.T) {
	long := strings.Repeat("x", 31) + "y" // exactly 32 characters
	tests := []struct {
		name     string
		existing []string
		email    string
		want     string
	}{
		{"free", nil, "alice@example.com", "alice"},
		{"first collision", []string{"alice"}, "alice@example.com", "alice-2"},
		{"several collisions", []string{"alice", "alice-2", "alice-3"}, "Alice@example.com", "alice-4"},
		{"truncated for suffix", []string{long}, long + "zz@example.com", strings.Repeat("x", 30) + "-2"},
		{"short local part", []string{"user"}, "x@example.com", "user-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, user.Options{AutoProvision: true, AllowedDomains: []string{"example.com"}})
			for _, h := range tt.existing {
				f.mustCreate(t, h, "", "")
			}
			u, err := f.svc.ResolveIdentity(ctx, identity("s1", tt.email, true))
			if err != nil {
				t.Fatal(err)
			}
			if u.Handle != tt.want {
				t.Errorf("handle = %q, want %q", u.Handle, tt.want)
			}
			if err := user.ValidateHandle(u.Handle); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestIdentityLinks(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "alice", "alice@example.com", "")
	f.mustCreate(t, "bob", "", "")

	if err := f.svc.LinkIdentity(ctx, "alice", " https://idp.example.com ", "sub-1", "Alice@Example.com"); err != nil {
		t.Fatalf("LinkIdentity: %v", err)
	}
	ev := f.lastEvent(t)
	if ev.Action != audit.UserUpdate || ev.Actor != "admin" || ev.Details["identity"] != "link" ||
		ev.Details["handle"] != "alice" || ev.Details["issuer"] != "https://idp.example.com" {
		t.Errorf("link audit event = %+v", ev)
	}
	ids, err := f.svc.ListIdentities(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := []user.Identity{{Issuer: "https://idp.example.com", Subject: "sub-1", EmailAtLogin: "alice@example.com", LastLoginAt: t0}}
	if diff := cmp.Diff(want, ids); diff != "" {
		t.Errorf("identities (-want +got):\n%s", diff)
	}

	// The linked identity now resolves to alice without any email match.
	u, err := f.svc.ResolveIdentity(ctx, auth.Identity{Issuer: "https://idp.example.com", Subject: "sub-1"})
	if err != nil || u.Handle != "alice" {
		t.Fatalf("ResolveIdentity = %v, %v; want alice", u, err)
	}

	errTests := []struct {
		name         string
		handle, i, s string
		wantError    error
	}{
		{name: "empty issuer", handle: "alice", i: "", s: "x", wantError: user.ErrInvalidIdentity},
		{name: "blank subject", handle: "alice", i: "iss", s: "  ", wantError: user.ErrInvalidIdentity},
		{name: "unknown user", handle: "nobody", i: "iss", s: "x", wantError: user.ErrNotFound},
	}
	for _, tt := range errTests {
		if err := f.svc.LinkIdentity(ctx, tt.handle, tt.i, tt.s, ""); !errors.Is(err, tt.wantError) {
			t.Errorf("LinkIdentity %s: err = %v, want %v", tt.name, err, tt.wantError)
		}
	}
	if _, err := f.svc.ListIdentities(ctx, "nobody"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("ListIdentities(nobody): err = %v, want ErrNotFound", err)
	}

	// Unlinking from the wrong user finds nothing; from the right one works.
	if err := f.svc.UnlinkIdentity(ctx, "bob", "https://idp.example.com", "sub-1"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("UnlinkIdentity(bob): err = %v, want ErrNotFound", err)
	}
	if err := f.svc.UnlinkIdentity(ctx, "alice", "", "sub-1"); !errors.Is(err, user.ErrInvalidIdentity) {
		t.Errorf("UnlinkIdentity(empty issuer): err = %v, want ErrInvalidIdentity", err)
	}
	if err := f.svc.UnlinkIdentity(ctx, "alice", "https://idp.example.com", "sub-1"); err != nil {
		t.Fatalf("UnlinkIdentity: %v", err)
	}
	if ev := f.lastEvent(t); ev.Details["identity"] != "unlink" {
		t.Errorf("unlink audit event = %+v", ev)
	}
	ids, err = f.svc.ListIdentities(ctx, "alice")
	if err != nil || len(ids) != 0 {
		t.Errorf("identities after unlink = %+v, %v", ids, err)
	}
}

// An admin's key issued with member scopes must act as a member: before the
// fix Admin followed the role alone, so HasScope granted every scope and the
// key passed the admin host.
func TestAuthenticateAdminKeyWithoutAdminScope(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	tests := []struct {
		name      string
		scopes    []string
		wantAdmin bool
	}{
		{"read only", []string{auth.ScopeRead}, false},
		{"default member scopes", nil, false},
		{"admin scope", []string{auth.ScopeRead, auth.ScopeAdmin}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, secret := f.mustIssue(t, "boss", tt.scopes, 0)
			p, err := f.svc.Authenticate(ctx, secret)
			if err != nil {
				t.Fatal(err)
			}
			if p.Admin != tt.wantAdmin {
				t.Errorf("Admin = %v, want %v", p.Admin, tt.wantAdmin)
			}
			if !tt.wantAdmin && (p.HasScope(auth.ScopeDelete) && !slices.Contains(p.Scopes, auth.ScopeDelete) || p.HasScope(auth.ScopeAdmin)) {
				t.Errorf("narrow key principal = %+v holds scopes it was not issued", p)
			}
		})
	}
}
