package config

// Expand expands the references in one scalar value of a config file.
func Expand(s string, lookup func(string) (string, bool)) (string, []error) {
	e := expander{path: "kazari.yaml", lookup: lookup, unset: map[string]bool{}}
	out, _ := e.expand(s, 1)
	return out, e.errs
}
