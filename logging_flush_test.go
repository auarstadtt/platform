package platform_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/auarstadtt/platform"
)

// A streamed response behind RequestLogger reaches the client as it is
// flushed, not when the handler returns: the handler here writes one event,
// flushes, then blocks until the client has read it.
func TestRequestLoggerPassesFlushThrough(t *testing.T) {
	read := make(chan struct{})
	h := platform.RequestLogger(platform.LoggingOptions{Logger: zap.NewNop(), ServiceName: "t", AccessLog: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: hello\n\n"))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush through the logger: %v", err)
			}
			select {
			case <-read:
			case <-r.Context().Done():
			}
		}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no response before the handler returned: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: hello\n" {
		t.Fatalf("first event: %q %v", line, err)
	}
	close(read)
}
