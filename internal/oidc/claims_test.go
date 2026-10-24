package oidc

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"agalma/internal/auth"
)

func TestIdentityMapping(t *testing.T) {
	const entra = "https://login.microsoftonline.com/0000-tenant/v2.0"
	tests := []struct {
		name   string
		issuer string
		claims string
		want   auth.Identity
	}{
		{"entra without email_verified", entra, `{"preferred_username":"bob@corp.example","name":"Bob"}`,
			auth.Identity{Issuer: entra, Subject: "s", Email: "bob@corp.example", EmailVerified: true, Name: "Bob"}},
		{"entra with email claim", entra, `{"email":"bob@corp.example"}`,
			auth.Identity{Issuer: entra, Subject: "s", Email: "bob@corp.example", EmailVerified: true}},
		{"entra explicit false wins", entra, `{"email":"bob@corp.example","email_verified":false}`,
			auth.Identity{Issuer: entra, Subject: "s", Email: "bob@corp.example"}},
		{"entra without any email", entra, `{"preferred_username":"bob"}`,
			auth.Identity{Issuer: entra, Subject: "s"}},
		{"other issuer without email_verified", "https://accounts.google.com", `{"email":"a@example.com"}`,
			auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", Email: "a@example.com"}},
		// Regression: Google's email_verified alone let a consumer account on
		// a company address (e.g. a former employee's) pass as that domain.
		{"google workspace account", "https://accounts.google.com", `{"email":"a@corp.example","email_verified":true,"hd":"corp.example"}`,
			auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", Email: "a@corp.example", EmailVerified: true}},
		{"google consumer account on a company address", "https://accounts.google.com", `{"email":"a@corp.example","email_verified":true}`,
			auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", Email: "a@corp.example"}},
		{"google hd for another domain", "https://accounts.google.com", `{"email":"a@corp.example","email_verified":true,"hd":"other.example"}`,
			auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", Email: "a@corp.example"}},
		{"google gmail account", "https://accounts.google.com", `{"email":"a@gmail.com","email_verified":true}`,
			auth.Identity{Issuer: "https://accounts.google.com", Subject: "s", Email: "a@gmail.com", EmailVerified: true}},
		{"string True", "https://idp.example", `{"email":"a@example.com","email_verified":"True"}`,
			auth.Identity{Issuer: "https://idp.example", Subject: "s", Email: "a@example.com", EmailVerified: true}},
		{"null email_verified", "https://idp.example", `{"email":"a@example.com","email_verified":null}`,
			auth.Identity{Issuer: "https://idp.example", Subject: "s", Email: "a@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c claims
			if err := json.Unmarshal([]byte(tt.claims), &c); err != nil {
				t.Fatal(err)
			}
			got := (&Provider{}).identity(tt.issuer, "s", c)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("identity (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFlexBoolRejectsNumbers(t *testing.T) {
	var c claims
	if err := json.Unmarshal([]byte(`{"email_verified":1}`), &c); err == nil {
		t.Error("email_verified 1 accepted")
	}
}
