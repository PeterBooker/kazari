package artifact

import (
	"mime"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Limits that are fixed by the design rather than configured.
const (
	SlugMinLen       = 3
	SlugMaxLen       = 63
	PathMaxBytes     = 1024
	SegmentMaxBytes  = 255
	MetaKeyMaxLen    = 64
	MetaValueMaxLen  = 4096
	DefaultMaxPairs  = 64
	DefaultMaxFiles  = 2000
	VersionSeparator = "--v" // <slug>--v<N>.<base>
)

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	metaKeyRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
)

// ValidateSlug checks the slug rules: lower-case letters, digits and single
// hyphens, 3 to 63 characters, no "--" (reserved for version hosts), and not
// in the reserved list.
func ValidateSlug(slug string, reserved []string) error {
	switch {
	case len(slug) < SlugMinLen:
		return Errorf(ErrInvalidSlug, "slug %q is shorter than %d characters", slug, SlugMinLen)
	case len(slug) > SlugMaxLen:
		return Errorf(ErrInvalidSlug, "slug %q is longer than %d characters", slug, SlugMaxLen)
	case !slugRe.MatchString(slug):
		return Errorf(ErrInvalidSlug, "slug %q must match %s", slug, slugRe.String())
	case strings.Contains(slug, "--"):
		return Errorf(ErrInvalidSlug, "slug %q may not contain \"--\"", slug)
	case slices.Contains(reserved, slug):
		return Errorf(ErrInvalidSlug, "slug %q is reserved", slug)
	}
	return nil
}

// ValidatePath normalises a file path and rejects anything that could
// escape or confuse the serving path: absolute paths, "..", NUL and control
// characters, backslashes, over-long segments or totals, and a trailing
// slash (a directory, not a file). It returns the cleaned path.
func ValidatePath(p string) (string, error) {
	if p == "" {
		return "", Errorf(ErrInvalidPath, "empty path")
	}
	if len(p) > PathMaxBytes {
		return "", Errorf(ErrInvalidPath, "path longer than %d bytes", PathMaxBytes)
	}
	if strings.HasPrefix(p, "/") {
		return "", Errorf(ErrInvalidPath, "path %q is absolute", p)
	}
	if strings.HasSuffix(p, "/") {
		return "", Errorf(ErrInvalidPath, "path %q names a directory", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == 0xfffd {
			return "", Errorf(ErrInvalidPath, "path %q contains an invalid character", p)
		}
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", Errorf(ErrInvalidPath, "path %q escapes the bundle", p)
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", Errorf(ErrInvalidPath, "path %q escapes the bundle", p)
		}
		if len(seg) > SegmentMaxBytes {
			return "", Errorf(ErrInvalidPath, "path segment longer than %d bytes", SegmentMaxBytes)
		}
	}
	return clean, nil
}

// ValidateMetadata checks keys, values and pair count, and restricts
// server-interpreted "agalma." keys to the known set with valid values.
// maxPairs <= 0 means DefaultMaxPairs.
func ValidateMetadata(m map[string]string, maxPairs int) error {
	if maxPairs <= 0 {
		maxPairs = DefaultMaxPairs
	}
	if len(m) > maxPairs {
		return Errorf(ErrInvalidMetadata, "more than %d metadata pairs", maxPairs)
	}
	for k, v := range m {
		if err := ValidateMetadataPair(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ValidateMetadataPair checks one key and value.
func ValidateMetadataPair(k, v string) error {
	if !metaKeyRe.MatchString(k) {
		return Errorf(ErrInvalidMetadata, "metadata key %q must match %s", k, metaKeyRe.String())
	}
	if len(v) > MetaValueMaxLen {
		return Errorf(ErrInvalidMetadata, "metadata value for %q longer than %d bytes", k, MetaValueMaxLen)
	}
	if !strings.HasPrefix(k, "agalma.") {
		return nil
	}
	switch k {
	case MetaSPA, MetaIndexable:
		if v != "true" && v != "false" {
			return Errorf(ErrInvalidMetadata, "metadata %s must be \"true\" or \"false\"", k)
		}
	case MetaCSP:
		// The value is sent verbatim as a response header: printable ASCII
		// only, so no line breaks, NUL or bytes a proxy might mangle.
		if !printableASCII(v) {
			return Errorf(ErrInvalidMetadata, "metadata %s must be printable ASCII on one line", k)
		}
	default:
		return Errorf(ErrInvalidMetadata, "unknown server metadata key %q", k)
	}
	return nil
}

// ValidateMetadataKey checks a key alone, for list filters.
func ValidateMetadataKey(k string) error {
	if !metaKeyRe.MatchString(k) {
		return Errorf(ErrInvalidMetadata, "metadata key %q must match %s", k, metaKeyRe.String())
	}
	return nil
}

// ParseVisibility converts user input to a Visibility. Empty means Public.
func ParseVisibility(s string) (Visibility, error) {
	switch Visibility(s) {
	case "", Public:
		return Public, nil
	case Unlisted:
		return Unlisted, nil
	}
	return "", Errorf(ErrInvalidArgument, "visibility must be %q or %q", Public, Unlisted)
}

// ContentTypeMaxLen caps a stored content type, which is replayed as a
// response header.
const ContentTypeMaxLen = 255

// validateContentType checks a caller-supplied content type before it is
// stored and later sent as the Content-Type header: printable ASCII, at
// most ContentTypeMaxLen bytes, and a parseable media type. Empty is fine
// (the type is then derived).
func validateContentType(ct string) error {
	if ct == "" {
		return nil
	}
	if len(ct) > ContentTypeMaxLen || !printableASCII(ct) {
		return Errorf(ErrInvalidArgument, "content type must be at most %d printable ASCII bytes", ContentTypeMaxLen)
	}
	if _, _, err := mime.ParseMediaType(ct); err != nil {
		return Errorf(ErrInvalidArgument, "content type %q is not a media type", ct)
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
