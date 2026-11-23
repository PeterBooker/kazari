package cli

// The parsers that read text from outside the process, for their fuzz
// targets, and journald, which needs a real file.
var (
	NormalizeURL   = normalizeURL
	ExtractBundle  = extractBundle
	ErrUnsafeEntry = errUnsafeEntry
	Journald       = journald
)

// IgnoreMatch reports whether .kazariignore rules name rel and, if so,
// whether they send it.
func IgnoreMatch(rules, rel string, dir bool) (matched, include bool) {
	return parseIgnore(rules).match(rel, dir)
}
