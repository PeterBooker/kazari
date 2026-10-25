package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandNode replaces references in the scalar values of n through lookup:
// ${NAME} (must be set, may be empty), ${NAME:-default} (default when unset
// or empty) and $$ for "$"; any other "$" is kept. Keys and comments are not
// expanded. Substituting after parsing keeps a value from changing the
// document's structure. A changed plain scalar has its tag cleared so YAML
// resolves its type again (${PORT:-8080} is an integer); quoted ones stay
// strings. A reference in a flow collection must be quoted (["${A}", b]), as
// YAML forbids "{" in a plain scalar there. Each unset variable is reported once.
func expandNode(n *yaml.Node, path string, lookup func(string) (string, bool)) error {
	e := expander{path: path, lookup: lookup, unset: map[string]bool{}}
	e.walk(n)
	return errors.Join(e.errs...)
}

type expander struct {
	path   string
	lookup func(string) (string, bool)
	unset  map[string]bool
	errs   []error
}

func (e *expander) walk(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		v, changed := e.expand(n.Value, n.Line)
		if !changed {
			return
		}
		n.Value = v
		const fixed = yaml.TaggedStyle | yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
		if n.Style&fixed == 0 {
			n.Tag = ""
		}
	case yaml.MappingNode:
		// Keys, at even positions, are not expanded.
		for i := 1; i < len(n.Content); i += 2 {
			e.walk(n.Content[i])
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			e.walk(c)
		}
	}
	// An AliasNode's target is expanded where it is defined.
}

// expand reports whether s changed.
func (e *expander) expand(s string, line int) (string, bool) {
	if !strings.Contains(s, "$") {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				e.errs = append(e.errs, fmt.Errorf("config: %s: line %d: unterminated variable reference %q", e.path, line, s[i:]))
				return s, false
			}
			ref := s[i+2 : i+2+end]
			i += 2 + end
			name, def, hasDef := strings.Cut(ref, ":-")
			if !isEnvName(name) {
				e.errs = append(e.errs, fmt.Errorf("config: %s: line %d: invalid variable reference ${%s}: want ${NAME} or ${NAME:-default}", e.path, line, ref))
				continue
			}
			v, ok := e.lookup(name)
			switch {
			case hasDef && v == "":
				b.WriteString(def)
			case ok:
				b.WriteString(v)
			case !e.unset[name]:
				e.unset[name] = true
				e.errs = append(e.errs, fmt.Errorf("config: %s: ${%s} is not set", e.path, name))
			}
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), b.String() != s
}

// isEnvName reports whether s matches [A-Za-z_][A-Za-z0-9_]*.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// checkKeys reports every unknown key with its line in the file, which the
// strict decoder cannot: it runs on re-encoded YAML.
func checkKeys(n *yaml.Node, path string) error {
	var errs []error
	checkKeysIn(n, reflect.TypeFor[Config](), "", map[*yaml.Node]bool{}, &errs)
	for i, err := range errs {
		errs[i] = fmt.Errorf("config: %s: %w", path, err)
	}
	return errors.Join(errs...)
}

func checkKeysIn(n *yaml.Node, t reflect.Type, prefix string, merged map[*yaml.Node]bool, errs *[]error) {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			checkKeysIn(c, t, prefix, merged, errs)
		}
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || n.Kind != yaml.MappingNode {
		return // a type mismatch, left to the decoder
	}
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		if name := keyName(t.Field(i)); name != "" {
			fields[name] = t.Field(i).Type
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.ShortTag() == "!!merge" {
			targets := []*yaml.Node{v}
			if v.Kind == yaml.SequenceNode {
				targets = v.Content
			}
			for _, m := range targets {
				if !merged[m] {
					merged[m] = true
					checkKeysIn(m, t, prefix, merged, errs)
				}
			}
			continue
		}
		key := k.Value
		if prefix != "" {
			key = prefix + "." + key
		}
		ft, ok := fields[k.Value]
		if !ok {
			*errs = append(*errs, fmt.Errorf("line %d: unknown key %s", k.Line, key))
			continue
		}
		checkKeysIn(v, ft, key, merged, errs)
	}
}
