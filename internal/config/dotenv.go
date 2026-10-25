package config

import (
	"fmt"
	"maps"
	"os"
	"strings"
)

// LoadEnvFile reads a dotenv file of KEY=VALUE lines, ignoring blank and "#"
// lines and an "export " prefix. A value is single-quoted (literal),
// double-quoted (escapes \n \t \r \" \\) or unquoted (trimmed; " #" starts a
// comment); nothing is interpolated. A later line for a key wins. A missing
// file satisfies fs.ErrNotExist.
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

// parseEnvLine returns ok false for a blank or comment line.
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

// unquoteDouble expects s to start after the opening quote.
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

// unquotedValue starts a comment only at a "#" after a blank: "KEY= # note"
// is empty but "KEY=#tag" is "#tag".
func unquotedValue(raw string) string {
	for i := 1; i < len(raw); i++ {
		if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = raw[:i]
			break
		}
	}
	return strings.TrimSpace(raw)
}

// EnvLookup returns a lookup for LoadWith that prefers the process
// environment over dotenv, which may be nil.
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
