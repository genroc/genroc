package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestServer runs ListenHTTP on an ephemeral port with the connection limits shrunk
// to durations a test can wait on, and returns the address plus a func that cancels the
// context and reports how long ListenHTTP took to return.
func startTestServer(t *testing.T, tune func(*Server)) (string, func() time.Duration) {
	t.Helper()
	h, cleanup := newTestHandlers(t)
	t.Cleanup(cleanup)

	s := NewServer(h, slog.New(slog.DiscardHandler))
	s.readHeaderTimeout = 300 * time.Millisecond
	s.readTimeout = 30 * time.Second
	s.shutdownTimeout = 500 * time.Millisecond
	if tune != nil {
		tune(s)
	}

	// Bind first on :0 to learn the port, then hand the address to ListenHTTP. The gap
	// between close and re-bind is why the dial below retries.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenHTTP(ctx, addr) }()

	waitListening(t, addr)

	stopped := false
	stop := func() time.Duration {
		if stopped {
			return 0
		}
		stopped = true
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ListenHTTP: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("ListenHTTP never returned after the context was cancelled")
		}
		return time.Since(start)
	}
	t.Cleanup(func() { stop() })
	return addr, stop
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never came up on %s", addr)
}

// A connection that opens and then sends nothing is the slowloris shape: before
// ReadHeaderTimeout was set it could hold a goroutine and a socket indefinitely, and
// enough of them exhaust the listener without a single valid request.
func TestListenHTTP_ClosesAConnectionThatSendsNoHeaders(t *testing.T) {
	addr, _ := startTestServer(t, nil)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("server answered a connection that sent no request")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("connection held for %v; the read deadline fired before the server hung up, so "+
			"ReadHeaderTimeout is not being applied", elapsed)
	}
}

// The drain has to be bounded *and* awaited: unbounded, a stuck request holds the shutdown
// goroutine forever; un-awaited, ListenHTTP returns immediately and process exit severs
// requests that were about to finish.
func TestListenHTTP_ShutdownWaitsForTheDrainButNotForever(t *testing.T) {
	addr, stop := startTestServer(t, nil)

	// Headers complete, body short of its declared length: the handler is parked reading
	// it, so this counts as an active connection for Shutdown.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /definitions HTTP/1.1\r\nHost: x\r\nContent-Length: 4096\r\n\r\n{")

	// Give the handler a moment to reach the body read before cancelling.
	time.Sleep(100 * time.Millisecond)

	elapsed := stop()
	if elapsed < 300*time.Millisecond {
		t.Errorf("ListenHTTP returned after %v with a request still in flight; it is not waiting for "+
			"the drain, so in-flight work dies at process exit", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("ListenHTTP took %v to return; the drain is not bounded by shutdownTimeout and a single "+
			"stuck request can outlast the supervisor's patience", elapsed)
	}
}

func TestListenHTTP_ShutsDownImmediatelyWhenIdle(t *testing.T) {
	_, stop := startTestServer(t, nil)
	if elapsed := stop(); elapsed > 2*time.Second {
		t.Errorf("an idle server took %v to shut down; Shutdown should close idle connections at once", elapsed)
	}
}

// putDefinition submits body to PUT /definitions and returns the status and reply.
func putDefinition(t *testing.T, addr, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, "http://"+addr+"/api/definitions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// oversizedDefinition builds a definition whose padding puts the encoded body n bytes over
// maxRequestBytes. Valid apart from its size, so a rejection can only be the cap: an
// invalid one would be refused for its shape whether the cap existed or not, which is what
// made the first version of this test pass with MaxBytesReader removed.
func oversizedDefinition(pad int) string {
	return fmt.Sprintf(
		`{"name":"size_probe","tasks":[{"id":"t","action":{"type":"fetch","url":"http://127.0.0.1:1/%s"},"switch":"end"}]}`,
		strings.Repeat("x", pad))
}

func TestListenHTTP_RejectsAnOversizedRequestBody(t *testing.T) {
	addr, _ := startTestServer(t, nil)

	status, body := putDefinition(t, addr, oversizedDefinition(maxRequestBytes))

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %.200s", status, body)
	}
	// The specific message matters: without the cap this same request is *also* a 400
	// (it decodes, then fails definition validation), so only the transport-level
	// complaint distinguishes a body that was refused from one that was buffered whole.
	if !strings.Contains(body, "request body too large") {
		t.Fatalf("body = %.200s\nwant the transport's oversize refusal — a 400 for any other reason means "+
			"the %d-byte body was read into memory before anything rejected it", body, maxRequestBytes)
	}
}

// The mirror: a body under the cap must still be read in full and acted on, or the limit
// is a regression rather than a guard.
func TestListenHTTP_AcceptsABodyUnderTheCap(t *testing.T) {
	addr, _ := startTestServer(t, nil)

	status, body := putDefinition(t, addr, oversizedDefinition(maxRequestBytes/2))

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %.200s", status, body)
	}
	if !strings.Contains(body, `"saved":true`) {
		t.Errorf("body = %.200s, want the definition saved", body)
	}
}
