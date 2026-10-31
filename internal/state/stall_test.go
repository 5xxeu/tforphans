package state

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A server that stops sending fails the read; one that keeps sending, if
// slowly, is read to the end however long that takes.
func TestFetchStall(t *testing.T) {
	old, oldTransport := stallTimeout, transport
	stallTimeout = 200 * time.Millisecond
	defer func() { stallTimeout, transport = old, oldTransport }()
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/headers":
			<-release
		case "/body":
			fmt.Fprint(w, `{"version":`)
			w.(http.Flusher).Flush()
			<-release
		case "/slow":
			fmt.Fprint(w, `{"version":4`)
			for range 6 {
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
				fmt.Fprint(w, ` `)
			}
			fmt.Fprint(w, `}`)
		}
	}))
	// Handlers wait for release, which has to come before Close.
	defer srv.Close()
	defer close(release)
	transport = srv.Client().Transport
	for _, path := range []string{"/headers", "/body"} {
		_, err := Fetch(context.Background(), srv.URL+path, false, DefaultMaxSize)
		if err == nil || !strings.Contains(err.Error(), "no data from the server for 200ms") {
			t.Errorf("%s: %v", path, err)
		}
	}
	if data, err := Fetch(context.Background(), srv.URL+"/slow", false, DefaultMaxSize); err != nil || !strings.HasPrefix(string(data), `{"version":4`) {
		t.Errorf("slow: %q %v", data, err)
	}
}
