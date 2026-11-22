// Package leaktest fails a package's tests when they leave goroutines
// behind that can never run again.
package leaktest

import (
	"fmt"
	"os"
	"runtime/pprof"
	"strings"
	"testing"
)

// Main runs the tests of a package and then looks for leaked goroutines:
// those blocked on a channel, mutex or condition that nothing still running
// can reach, which is what the runtime's goroutineleak profile finds. A
// package calls it from TestMain:
//
//	func TestMain(m *testing.M) { leaktest.Main(m) }
func Main(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if leaked := Leaked(); leaked != "" {
			fmt.Fprintf(os.Stderr, "goroutines leaked by the tests:\n\n%s", leaked)
			code = 1
		}
	}
	os.Exit(code)
}

// Leaked returns the stacks of the goroutines leaked so far, or "" when
// there are none.
func Leaked() string {
	p := pprof.Lookup("goroutineleak")
	var b strings.Builder
	if err := p.WriteTo(&b, 1); err != nil {
		return err.Error()
	}
	if p.Count() == 0 {
		return ""
	}
	return b.String()
}
