package leaktest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/leaktest"
)

// A leaked goroutine stays for the life of the process, so with -count the
// profile already holds the earlier runs': the test looks for its own
// functions in it.
func TestLeaked(t *testing.T) {
	finish()
	leak()
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got, "leaktest_test.leak") {
		if time.Now().After(deadline) {
			t.Fatalf("Leaked() = %q, want the stack of the goroutine leak started", got)
		}
		time.Sleep(time.Millisecond)
		got = leaktest.Leaked()
	}
	if strings.Contains(got, "leaktest_test.finish") {
		t.Errorf("Leaked() names a goroutine that returned:\n%s", got)
	}
}

// finish starts a goroutine and waits for it to return.
func finish() {
	done := make(chan struct{})
	go func() { close(done) }()
	<-done
}

// leak starts a goroutine that waits on a channel nothing else holds.
func leak() {
	ch := make(chan struct{})
	go func() { <-ch }()
}
