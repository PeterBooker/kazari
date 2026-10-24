package artifact_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"agalma/internal/artifact"
)

func TestValidateSlug(t *testing.T) {
	reserved := []string{"api", "admin", "www"}
	tests := []struct {
		slug string
		ok   bool
	}{
		{"abc", true},
		{"my-demo", true},
		{"a1-b2-c3", true},
		{"123", true},
		{strings.Repeat("a", artifact.SlugMaxLen), true},
		{"", false},
		{"ab", false},
		{strings.Repeat("a", artifact.SlugMaxLen+1), false},
		{"Demo", false},
		{"my_demo", false},
		{"my.demo", false},
		{"my demo", false},
		{"démo", false},
		{"-abc", false},
		{"abc-", false},
		{"my--demo", false},
		{"demo--v2", false},
		{"api", false},
		{"admin", false},
		{"apis", true},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			err := artifact.ValidateSlug(tt.slug, reserved)
			if tt.ok && err != nil {
				t.Fatalf("ValidateSlug(%q) = %v, want nil", tt.slug, err)
			}
			if !tt.ok && !errors.Is(err, artifact.ErrInvalidSlug) {
				t.Fatalf("ValidateSlug(%q) = %v, want ErrInvalidSlug", tt.slug, err)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	long := strings.Repeat("a", artifact.SegmentMaxBytes)
	tests := []struct {
		name, in, want string // want "" means rejected
	}{
		{"simple", "index.html", "index.html"},
		{"nested", "css/site.css", "css/site.css"},
		{"dot prefix and double slash", "./a//b", "a/b"},
		{"inner dot", "a/./b", "a/b"},
		{"inner dotdot staying inside", "a/b/../c", "a/c"},
		{"dotfile", ".well-known/x", ".well-known/x"},
		{"unicode", "café/menu.html", "café/menu.html"},
		{"segment at limit", long, long},
		{"empty", "", ""},
		{"dot", ".", ""},
		{"dotdot", "..", ""},
		{"traversal", "../x", ""},
		{"deep traversal", "a/../../b", ""},
		{"collapses to nothing", "a/..", ""},
		{"absolute", "/etc/passwd", ""},
		{"trailing slash", "dir/", ""},
		{"NUL", "a\x00b", ""},
		{"newline", "a\nb", ""},
		{"tab", "a\tb", ""},
		{"DEL", "a\x7fb", ""},
		{"backslash", `a\b`, ""},
		{"backslash traversal", `..\x`, ""},
		{"invalid utf-8", "a\xffb", ""},
		{"segment too long", strings.Repeat("a", artifact.SegmentMaxBytes+1), ""},
		{"total too long", strings.Repeat("abcdefgh/", artifact.PathMaxBytes/9+1) + "x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := artifact.ValidatePath(tt.in)
			if tt.want == "" {
				if !errors.Is(err, artifact.ErrInvalidPath) {
					t.Fatalf("ValidatePath(%q) = %q, %v; want ErrInvalidPath", tt.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePath(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ValidatePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateMetadata(t *testing.T) {
	many := func(n int) map[string]string {
		m := map[string]string{}
		for i := range n {
			m[fmt.Sprintf("k%d", i)] = "v"
		}
		return m
	}
	tests := []struct {
		name     string
		m        map[string]string
		maxPairs int
		ok       bool
	}{
		{"nil", nil, 0, true},
		{"plain", map[string]string{"team": "core", "Owner_2": "x", "a.b-c": ""}, 0, true},
		{"key at limit", map[string]string{strings.Repeat("k", artifact.MetaKeyMaxLen): "v"}, 0, true},
		{"key too long", map[string]string{strings.Repeat("k", artifact.MetaKeyMaxLen+1): "v"}, 0, false},
		{"empty key", map[string]string{"": "v"}, 0, false},
		{"key with space", map[string]string{"a b": "v"}, 0, false},
		{"key with quote", map[string]string{`a"b`: "v"}, 0, false},
		{"key with slash", map[string]string{"a/b": "v"}, 0, false},
		{"key with dollar", map[string]string{"$a": "v"}, 0, false},
		{"value at limit", map[string]string{"k": strings.Repeat("v", artifact.MetaValueMaxLen)}, 0, true},
		{"value too long", map[string]string{"k": strings.Repeat("v", artifact.MetaValueMaxLen+1)}, 0, false},
		{"default pair cap", many(artifact.DefaultMaxPairs), 0, true},
		{"over default pair cap", many(artifact.DefaultMaxPairs + 1), 0, false},
		{"custom pair cap", many(3), 2, false},
		{"spa true", map[string]string{artifact.MetaSPA: "true"}, 0, true},
		{"spa false", map[string]string{artifact.MetaSPA: "false"}, 0, true},
		{"spa yes", map[string]string{artifact.MetaSPA: "yes"}, 0, false},
		{"indexable true", map[string]string{artifact.MetaIndexable: "true"}, 0, true},
		{"indexable TRUE", map[string]string{artifact.MetaIndexable: "TRUE"}, 0, false},
		{"csp", map[string]string{artifact.MetaCSP: "default-src 'self'"}, 0, true},
		{"csp with newline", map[string]string{artifact.MetaCSP: "default-src 'self'\r\nX-Evil: 1"}, 0, false},
		{"unknown agalma key", map[string]string{"agalma.theme": "dark"}, 0, false},
		{"agalma prefix lookalike", map[string]string{"agalmax.theme": "dark"}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := artifact.ValidateMetadata(tt.m, tt.maxPairs)
			if tt.ok && err != nil {
				t.Fatalf("ValidateMetadata = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, artifact.ErrInvalidMetadata) {
				t.Fatalf("ValidateMetadata = %v, want ErrInvalidMetadata", err)
			}
		})
	}
}

func TestValidateMetadataKey(t *testing.T) {
	for k, ok := range map[string]bool{"team": true, "agalma.spa": true, "a'b": false, "": false} {
		if err := artifact.ValidateMetadataKey(k); (err == nil) != ok {
			t.Errorf("ValidateMetadataKey(%q) = %v, want ok=%v", k, err, ok)
		}
	}
}

func TestParseVisibility(t *testing.T) {
	tests := []struct {
		in   string
		want artifact.Visibility
		ok   bool
	}{
		{"", artifact.Public, true},
		{"public", artifact.Public, true},
		{"unlisted", artifact.Unlisted, true},
		{"unpublished", "", false},
		{"Public", "", false},
		{"private", "", false},
	}
	for _, tt := range tests {
		got, err := artifact.ParseVisibility(tt.in)
		if !tt.ok {
			if !errors.Is(err, artifact.ErrInvalidArgument) {
				t.Errorf("ParseVisibility(%q) = %q, %v; want ErrInvalidArgument", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseVisibility(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestErrorIs(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", artifact.Errorf(artifact.ErrNotFound, "no %s", "demo"))
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Fatal("Errorf result does not match its sentinel")
	}
	if errors.Is(err, artifact.ErrGone) {
		t.Fatal("Errorf result matches another sentinel")
	}
	if got := artifact.CodeOf(err); got != "not_found" {
		t.Fatalf("CodeOf = %q", got)
	}
}

// agalma.csp is sent verbatim as a header: only printable ASCII is allowed.
func TestValidateMetadataCSP(t *testing.T) {
	tests := []struct {
		name string
		v    string
		ok   bool
	}{
		{"policy", "default-src 'self'; frame-ancestors 'none'", true},
		{"line break", "default-src 'self'\r\nX-Evil: 1", false},
		{"NUL", "default-src 'self'\x00", false},
		{"tab", "default-src\t'self'", false},
		{"non-ASCII", "default-src https://\u00e9.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := artifact.ValidateMetadataPair(artifact.MetaCSP, tt.v)
			if (err == nil) != tt.ok {
				t.Errorf("ValidateMetadataPair(%q) = %v, ok want %v", tt.v, err, tt.ok)
			}
		})
	}
}
