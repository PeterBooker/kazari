package middleware_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/middleware"
)

// A client that stops reading ends the response, and with it the handler,
// once it has stalled; one that keeps reading gets all of it however long
// the server's write timeout is.
func TestStreaming(t *testing.T) {
	const size = 64 << 20
	done := make(chan error, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := middleware.Streaming(w, 200*time.Millisecond)
		chunk := make([]byte, 64<<10)
		var err error
		for written := 0; written < size && err == nil; written += len(chunk) {
			_, err = out.Write(chunk)
		}
		done <- err
	}))
	// Shorter than the response takes to stall: it must not be what ends it.
	srv.Config.WriteTimeout = time.Hour
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || n != size {
		t.Fatalf("a client that reads got %d bytes, %v; want %d", n, err, size)
	}
	if err := <-done; err != nil {
		t.Fatalf("the handler of a client that reads: %v", err)
	}

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// The connection stays open and unread.
	select {
	case err := <-done:
		if err == nil {
			t.Error("the handler of a stalled client wrote everything")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the handler of a stalled client is still writing")
	}
}
