package artifact_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"kazari/internal/artifact"
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
		{"unknown kazari key", map[string]string{"kazari.theme": "dark"}, 0, false},
		{"kazari prefix lookalike", map[string]string{"kazarix.theme": "dark"}, 0, true},
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

// Names, descriptions and labels are returned by every listing and shown
// in terminals and pages: short, UTF-8, and without characters that
// change how the text around them reads.
func TestValidateText(t *testing.T) {
	tests := []struct {
		name     string
		validate func(string) error
		in       string
		msg      string // part of the error; "" means accepted
	}{
		{"name at limit", artifact.ValidateName, strings.Repeat("n", artifact.NameMaxBytes), ""},
		{"name over limit", artifact.ValidateName, strings.Repeat("n", artifact.NameMaxBytes+1), "name is longer than 256 bytes"},
		{"name multi-byte", artifact.ValidateName, "Démo ✓ 日本語", ""},
		{"name empty", artifact.ValidateName, "", ""},
		{"name newline", artifact.ValidateName, "a\nb", "name contains the control character U+000A"},
		{"name tab", artifact.ValidateName, "a\tb", "U+0009"},
		{"name NUL", artifact.ValidateName, "a\x00b", "U+0000"},
		{"name DEL", artifact.ValidateName, "a\x7fb", "U+007F"},
		{"name C1", artifact.ValidateName, "a\u0085b", "U+0085"},
		{"name bidi override", artifact.ValidateName, "a\u202eb", "U+202E"},
		{"name bidi embedding", artifact.ValidateName, "a\u202ab", "U+202A"},
		{"name bidi isolate", artifact.ValidateName, "a\u2066b", "U+2066"},
		{"name pop isolate", artifact.ValidateName, "a\u2069b", "U+2069"},
		{"name invalid UTF-8", artifact.ValidateName, "a\xffb", "name is not valid UTF-8"},
		{"description at limit", artifact.ValidateDescription, strings.Repeat("d", artifact.DescriptionMaxBytes), ""},
		{"description over limit", artifact.ValidateDescription, strings.Repeat("d", artifact.DescriptionMaxBytes+1), "description is longer than 4096 bytes"},
		{"description lines and tabs", artifact.ValidateDescription, "line one\n\tline two\n", ""},
		{"description carriage return", artifact.ValidateDescription, "a\r\nb", "U+000D"},
		{"description escape", artifact.ValidateDescription, "\x1b[31mred", "U+001B"},
		{"description bidi", artifact.ValidateDescription, "x\u202ey", "U+202E"},
		{"label at limit", artifact.ValidateLabel, strings.Repeat("l", artifact.LabelMaxBytes), ""},
		{"label over limit", artifact.ValidateLabel, strings.Repeat("l", artifact.LabelMaxBytes+1), "label is longer than 256 bytes"},
		{"label newline", artifact.ValidateLabel, "v1\n", "label contains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.validate(tt.in)
			if tt.msg == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, artifact.ErrInvalidArgument) || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want invalid_argument containing %q", err, tt.msg)
			}
		})
	}
}

func TestValidateMetadataKey(t *testing.T) {
	for k, ok := range map[string]bool{"team": true, "kazari.spa": true, "a'b": false, "": false} {
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

// kazari.csp is sent verbatim as a header: only printable ASCII is allowed.
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
