package config

import (
	"fmt"
	"maps"
	"os"
	"strings"
)

// LoadEnvFile reads a dotenv file: one KEY=VALUE per line, with blank lines
// and lines starting with "#" ignored and an optional "export " prefix. KEY
// matches [A-Za-z_][A-Za-z0-9_]*. VALUE is one of:
//
//   - single-quoted: taken literally up to the closing quote;
//   - double-quoted: with the escapes \n, \t, \r, \" and \\ (a backslash
//     before any other character is kept as it is);
//   - unquoted: trimmed of surrounding blanks, with a trailing comment (a
//     "#" preceded by a blank) removed.
//
// A quoted value may be followed only by blanks and a "# comment", and must
// close on the same line. Nothing is interpolated: "$" has no special
// meaning. A later line for the same key wins. A malformed line is an error
// naming its line number, and a missing file is an error satisfying
// errors.Is(err, fs.ErrNotExist).
func LoadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	vars := map[string]string{}
	for n, line := range strings.Split(text, "\n") {
		key, value, ok, err := parseEnvLine(line)
		if err != nil {
			return nil, fmt.Errorf("config: %s: line %d: %w", path, n+1, err)
		}
		if ok {
			vars[key] = value
		}
	}
	return vars, nil
}

// parseEnvLine parses one dotenv line; ok is false for a blank or comment line.
func parseEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	if rest, found := strings.CutPrefix(line, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		line = strings.TrimLeft(rest, " \t")
	}
	key, raw, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("want KEY=VALUE, got %q", line)
	}
	key = strings.TrimRight(key, " \t")
	if !isEnvName(key) {
		return "", "", false, fmt.Errorf("invalid variable name %q", key)
	}
	after := raw
	raw = strings.TrimLeft(raw, " \t")
	if raw == "" {
		return key, "", true, nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", "", false, fmt.Errorf("%s: unterminated single-quoted value", key)
		}
		value, raw = raw[1:1+end], raw[2+end:]
	case '"':
		value, raw, err = unquoteDouble(raw[1:])
		if err != nil {
			return "", "", false, fmt.Errorf("%s: %w", key, err)
		}
	default:
		return key, unquotedValue(after), true, nil
	}
	if rest := strings.TrimLeft(raw, " \t"); rest != "" && (rest == raw || rest[0] != '#') {
		return "", "", false, fmt.Errorf("%s: unexpected text after the closing quote: %q", key, rest)
	}
	return key, value, true, nil
}

// unquoteDouble decodes a double-quoted value whose opening quote has been
// consumed, returning it and the text after the closing quote.
func unquoteDouble(s string) (value, rest string, err error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			if i+1 == len(s) {
				b.WriteByte(c)
				continue
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("unterminated double-quoted value")
}

// unquotedValue strips a trailing comment and surrounding blanks from the
// text after "=". A "#" starts the comment when a blank precedes it, so in
// "KEY= # note" the value is empty but in "KEY=#tag" it is "#tag".
func unquotedValue(raw string) string {
	for i := 1; i < len(raw); i++ {
		if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = raw[:i]
			break
		}
	}
	return strings.TrimSpace(raw)
}

// EnvLookup returns a variable lookup for LoadWith that reads the process
// environment (os.LookupEnv) first and dotenv second, so a value from a
// .env file never overrides a variable already set in the environment.
// dotenv may be nil.
func EnvLookup(dotenv map[string]string) func(key string) (string, bool) {
	dotenv = maps.Clone(dotenv)
	return func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := dotenv[key]
		return v, ok
	}
}
