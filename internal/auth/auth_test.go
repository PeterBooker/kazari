package auth_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/auth"
)

type stub struct {
	p     auth.Principal
	err   error
	calls int
}

func (s *stub) Authenticate(context.Context, string) (auth.Principal, error) {
	s.calls++
	return s.p, s.err
}

func TestChain(t *testing.T) {
	boom := errors.New("database down")
	alice := auth.Principal{Handle: "alice"}
	bob := auth.Principal{Handle: "bob"}
	tests := []struct {
		name      string
		auths     []*stub
		want      auth.Principal
		wantErr   error
		wantCalls []int
	}{
		{"empty", nil, auth.Principal{}, auth.ErrInvalidToken, nil},
		{"first match wins", []*stub{{p: alice}, {p: bob}}, alice, nil, []int{1, 0}},
		{"falls through invalid", []*stub{{err: auth.ErrInvalidToken}, {p: bob}}, bob, nil, []int{1, 1}},
		{"falls through wrapped invalid",
			[]*stub{{err: fmt.Errorf("oauth: %w", auth.ErrInvalidToken)}, {p: bob}}, bob, nil, []int{1, 1}},
		{"all invalid", []*stub{{err: auth.ErrInvalidToken}, {err: auth.ErrInvalidToken}},
			auth.Principal{}, auth.ErrInvalidToken, []int{1, 1}},
		{"other error stops", []*stub{{err: boom}, {p: bob}}, auth.Principal{}, boom, []int{1, 0}},
		{"other error after invalid stops",
			[]*stub{{err: auth.ErrInvalidToken}, {err: boom}, {p: bob}}, auth.Principal{}, boom, []int{1, 1, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var as []auth.Authenticator
			for _, s := range tt.auths {
				as = append(as, s)
			}
			got, err := auth.Chain(as...).Authenticate(context.Background(), "tok")
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("principal (-want +got):\n%s", diff)
			}
			var calls []int
			for _, s := range tt.auths {
				calls = append(calls, s.calls)
			}
			if diff := cmp.Diff(tt.wantCalls, calls); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAuthenticatorFunc(t *testing.T) {
	var gotToken string
	f := auth.AuthenticatorFunc(func(_ context.Context, token string) (auth.Principal, error) {
		gotToken = token
		return auth.Principal{Handle: "x"}, nil
	})
	p, err := f.Authenticate(context.Background(), "secret")
	if err != nil || p.Handle != "x" || gotToken != "secret" {
		t.Errorf("Authenticate = %+v, %v (token %q)", p, err, gotToken)
	}
}

func TestContextPrincipal(t *testing.T) {
	ctx := context.Background()
	if p, ok := auth.FromContext(ctx); ok {
		t.Errorf("FromContext(empty) = %+v, true", p)
	}
	want := auth.Principal{Handle: "alice", Scopes: []string{auth.ScopeRead}, Source: auth.SourceAPIKey, KeyID: "abcd2345", QuotaBytes: 9}
	got, ok := auth.FromContext(auth.ContextWithPrincipal(ctx, want))
	if !ok {
		t.Fatal("FromContext: no principal")
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("FromContext (-want +got):\n%s", diff)
	}
}

func TestHasScope(t *testing.T) {
	tests := []struct {
		name  string
		p     auth.Principal
		scope string
		want  bool
	}{
		{"held", auth.Principal{Scopes: []string{auth.ScopeRead, auth.ScopeWrite}}, auth.ScopeWrite, true},
		{"not held", auth.Principal{Scopes: []string{auth.ScopeRead}}, auth.ScopeDelete, false},
		{"no scopes", auth.Principal{}, auth.ScopeRead, false},
		{"admin scope string without admin role", auth.Principal{Scopes: []string{auth.ScopeAdmin}}, auth.ScopeRead, false},
		{"admin holds everything", auth.Principal{Admin: true}, auth.ScopeDelete, true},
		{"admin holds admin", auth.Principal{Admin: true}, auth.ScopeAdmin, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.HasScope(tt.scope); got != tt.want {
				t.Errorf("HasScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}
