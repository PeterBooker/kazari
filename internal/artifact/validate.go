package artifact

import (
	"mime"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

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

	// Bytes of UTF-8, kept short because every listing returns them.
	NameMaxBytes        = 256
	DescriptionMaxBytes = 4096
	LabelMaxBytes       = 256
)

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	metaKeyRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
)

// ValidateSlug allows lower-case letters, digits and single hyphens ("--"
// marks version hosts), 3 to 63 characters, and no reserved slug.
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

// ValidatePath returns p cleaned, rejecting anything that could escape or
// confuse the serving path.
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

// ValidateMetadata checks every pair and the count; maxPairs <= 0 means
// DefaultMaxPairs.
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

// ValidateMetadataPair checks one pair; "kazari." keys must be known, with
// valid values.
func ValidateMetadataPair(k, v string) error {
	if !metaKeyRe.MatchString(k) {
		return Errorf(ErrInvalidMetadata, "metadata key %q must match %s", k, metaKeyRe.String())
	}
	if len(v) > MetaValueMaxLen {
		return Errorf(ErrInvalidMetadata, "metadata value for %q longer than %d bytes", k, MetaValueMaxLen)
	}
	if !strings.HasPrefix(k, "kazari.") {
		return nil
	}
	switch k {
	case MetaSPA, MetaIndexable:
		if v != "true" && v != "false" {
			return Errorf(ErrInvalidMetadata, "metadata %s must be \"true\" or \"false\"", k)
		}
	case MetaCSP:
		// Sent verbatim as a response header: no line breaks, NUL or bytes a
		// proxy might mangle.
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

// ValidateName allows one line of valid UTF-8, at most NameMaxBytes, with no
// control or bidi control characters.
func ValidateName(s string) error { return validateText("name", s, NameMaxBytes, false) }

// ValidateDescription is ValidateName with DescriptionMaxBytes, allowing
// newline and tab.
func ValidateDescription(s string) error {
	return validateText("description", s, DescriptionMaxBytes, true)
}

// ValidateLabel is ValidateName with LabelMaxBytes.
func ValidateLabel(s string) error { return validateText("label", s, LabelMaxBytes, false) }

// validateText rejects characters that change how the output around them
// reads: C0 and C1 controls, DEL, and the bidi embedding, override and
// isolate controls.
func validateText(field, s string, maxBytes int, multiline bool) error {
	if len(s) > maxBytes {
		return Errorf(ErrInvalidArgument, "%s is longer than %d bytes", field, maxBytes)
	}
	if !utf8.ValidString(s) {
		return Errorf(ErrInvalidArgument, "%s is not valid UTF-8", field)
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return Errorf(ErrInvalidArgument, "%s contains the control character U+%04X", field, r)
		}
	}
	return nil
}

// ContentTypeMaxLen caps a stored content type, which is replayed as a
// response header.
const ContentTypeMaxLen = 255

// ValidateContentType requires a printable-ASCII media type, since it is
// sent as the Content-Type header. Empty is fine: the type is derived.
func ValidateContentType(ct string) error {
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
