package buildinfo

import "testing"

// Stamp sets, until the test ends, what a release build sets with -ldflags -X.
func Stamp(t *testing.T, v, c, d string) {
	t.Helper()
	oldVersion, oldCommit, oldDate := version, commit, date
	version, commit, date = v, c, d
	t.Cleanup(func() { version, commit, date = oldVersion, oldCommit, oldDate })
}
