package httpapi

// UserRoutes returns the pattern of every route of the user API behind the
// bearer check, so a test can tell when one is added.
func UserRoutes() []string {
	var out []string
	for _, rt := range (&userAPI{}).routes() {
		out = append(out, rt.pattern)
	}
	return out
}
