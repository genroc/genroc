package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"genroc/internal/errcode"
)

// serveBody starts a server answering every request with exactly n bytes of a valid JSON
// string value, so a body can be sized either side of MaxResponseBytes without the size
// depending on how the payload happens to encode.
func serveBody(t *testing.T, n int) string {
	t.Helper()
	if n < 2 {
		t.Fatalf("n=%d cannot hold a quoted JSON string", n)
	}
	body := `"` + strings.Repeat("x", n-2) + `"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSendHTTP_BodyAtTheLimitIsRead(t *testing.T) {
	resp, err := sendHTTP(context.Background(), client, serveBody(t, MaxResponseBytes), http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.BodyCode != "" {
		t.Fatalf("a body of exactly MaxResponseBytes must be accepted, got %q (%s) — an off-by-one here rejects "+
			"responses the limit is documented to allow", resp.BodyCode, resp.ErrorMessage)
	}
	if got := len(resp.Body.(string)); got != MaxResponseBytes-2 {
		t.Fatalf("decoded %d bytes, want %d", got, MaxResponseBytes-2)
	}
}

func TestSendHTTP_BodyPastTheLimitIsRefused(t *testing.T) {
	resp, err := sendHTTP(context.Background(), client, serveBody(t, MaxResponseBytes+1), http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.BodyCode != errcode.OutputTooLarge {
		t.Fatalf("got %q, want %q — an unbounded read here OOMs the worker and strands every lease it holds",
			resp.BodyCode, errcode.OutputTooLarge)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d, want 200: the response arrived, so on_error must still see what the endpoint answered", resp.Status)
	}
}

// An oversized body that is also malformed must report the size, not the syntax: the parse
// error is a consequence of the truncation, and "fix your JSON" sends the reader nowhere.
func TestSendHTTP_TooLargeOutranksParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"k":"`+strings.Repeat("x", MaxResponseBytes)) // never closed
	}))
	defer srv.Close()

	resp, err := sendHTTP(context.Background(), client, srv.URL, http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.BodyCode != errcode.OutputTooLarge {
		t.Fatalf("got %q, want %q", resp.BodyCode, errcode.OutputTooLarge)
	}
}

func TestSendHTTP_ShortBodyStillParses(t *testing.T) {
	resp, err := sendHTTP(context.Background(), client, serveBody(t, 16), http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("unexpected error code %q (%s)", resp.ErrorCode, resp.ErrorMessage)
	}
}

// A pin, not a behaviour test: connection reuse is only observable through timing, and the
// failure this guards against is silent and textual — someone reaching for
// http.DefaultClient again, whose per-host idle cap of 2 makes a worker re-dial and
// re-handshake TLS for nearly every call to the same endpoint.
func TestClient_PoolsConnectionsPerHost(t *testing.T) {
	if client == http.DefaultClient {
		t.Fatal("fetch is using http.DefaultClient again")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport is %T, want *http.Transport", client.Transport)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want more than the stdlib default of %d",
			tr.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
	}
	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0: the per-attempt budget is the caller's context "+
			"deadline, and a ceiling here silently overrides a task's declared timeout", client.Timeout)
	}
}

// An empty body is a value, not a failure. 204, an async 202 and a webhook ACK all answer
// with nothing, and reporting output.parse for them is what made those endpoints unwritable.
func TestSendHTTP_EmptyBodyDecodesToNull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"204 no content", http.StatusNoContent}, {"200 with nothing", http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			resp, err := sendHTTP(context.Background(), client, srv.URL, http.MethodGet, nil, nil, nil)
			if err != nil {
				t.Fatalf("sendHTTP: %v", err)
			}
			if resp.BodyCode != "" {
				t.Fatalf("got %q, want no body code — an empty body is null, not a parse failure", resp.BodyCode)
			}
			if resp.Body != nil {
				t.Fatalf("body = %#v, want nil", resp.Body)
			}
		})
	}
}

// An unaccepted status carries its body through as a decoded value AND as text: error.data
// needs the value, the operator reading an audit row needs the text, and dropping either
// leaves one of them with nothing.
func TestSendHTTP_UnacceptedStatusKeepsBodyAndText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"no such order"}`)
	}))
	defer srv.Close()

	resp, err := sendHTTP(context.Background(), client, srv.URL, http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.ErrorCode != errcode.HTTP(http.StatusNotFound) {
		t.Fatalf("code = %q, want http.404", resp.ErrorCode)
	}
	body, ok := resp.Body.(map[string]any)
	if !ok {
		t.Fatalf("body = %#v, want a decoded object — error.data has nothing to type without it", resp.Body)
	}
	if body["detail"] != "no such order" {
		t.Errorf("body[detail] = %v, want %q", body["detail"], "no such order")
	}
	if !strings.Contains(resp.ErrorMessage, "no such order") {
		t.Errorf("message = %q, want the body text: an operator reads this row, not error.data", resp.ErrorMessage)
	}
}

// A body the transport could not read is reported as BodyCode, never as ErrorCode: an HTML
// 500 from a status nobody declared must stay http.500, and only the caller knows that.
func TestSendHTTP_UnreadableErrorBodyIsNotAVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "<html>gateway exploded</html>")
	}))
	defer srv.Close()

	resp, err := sendHTTP(context.Background(), client, srv.URL, http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.ErrorCode != errcode.HTTP(http.StatusInternalServerError) {
		t.Fatalf("code = %q, want http.500 — an undeclared status is not made a parse failure by its body", resp.ErrorCode)
	}
	if resp.BodyCode != errcode.OutputParse {
		t.Errorf("body code = %q, want %q", resp.BodyCode, errcode.OutputParse)
	}
	if resp.Body != nil {
		t.Errorf("body = %#v, want nil", resp.Body)
	}
}

// killAfterRead reads a whole request off the socket and then destroys the connection
// without answering. The remote demonstrably received the call — the case that separates
// http.disconnected from pre.error. rst picks whether the close emits RST or FIN; both
// reach the client as a post-write failure, so both must classify the same.
func killAfterRead(t *testing.T, rst bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Read(make([]byte, 4096)) // headers and a small body arrive together
				if rst {
					c.(*net.TCPConn).SetLinger(0) // makes Close emit RST rather than FIN
				}
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/eval"
}

// silentAfterRead reads the request and then holds the connection open forever.
func silentAfterRead(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Read(make([]byte, 4096))
				<-done
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/eval"
}

// stallsAfterHeaders reads a request's headers and then stops reading. A body larger than
// the socket buffers blocks mid-write, so a deadline fires while bytes are on the wire —
// the case where reading a write-goroutine hook after Do returns would be a guess.
func stallsAfterHeaders(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Read(make([]byte, 512))
				<-stop
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/eval"
}

func deadPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr + "/eval"
}

// TestClassifyGoError_PreOnlyWhenTheRequestNeverLeft pins the retry-safety split, not the
// diagnosis: pre.* licenses a retry on an only_once task (isRetryAllowed), so claiming it
// for a call the remote may have run is the one misclassification that can double-charge.
func TestClassifyGoError_PreOnlyWhenTheRequestNeverLeft(t *testing.T) {
	cases := []struct {
		name    string
		url     func(*testing.T) string
		body    any
		ctx     func() (context.Context, context.CancelFunc)
		want    errcode.Code
		reached bool // did the remote have the bytes?
	}{
		{
			name: "nothing listening: the dial itself failed",
			url:  deadPort, want: errcode.PreError, reached: false,
		},
		{
			name: "body could not be marshaled: no socket was ever touched",
			url:  func(*testing.T) string { return "http://127.0.0.1:1/eval" },
			body: make(chan int), want: errcode.PreError, reached: false,
		},
		{
			name: "context already canceled: nothing was written",
			url:  func(*testing.T) string { return "http://127.0.0.1:1/eval" },
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			want: errcode.PreTimeout, reached: false,
		},
		{
			name: "remote read the request, then reset the connection",
			url:  func(t *testing.T) string { return killAfterRead(t, true) },
			want: errcode.HTTPDisconnected, reached: true,
		},
		{
			name: "remote read the request, then closed the connection",
			url:  func(t *testing.T) string { return killAfterRead(t, false) },
			want: errcode.HTTPDisconnected, reached: true,
		},
		{
			name: "deadline fired while the body was still going out",
			url:  stallsAfterHeaders,
			body: strings.Repeat("x", 64<<20),
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 300*time.Millisecond)
			},
			want: errcode.HTTPTimeout, reached: true,
		},
		{
			name: "remote read the request and never answered",
			url:  silentAfterRead,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 200*time.Millisecond)
			},
			want: errcode.HTTPTimeout, reached: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()

			body := tc.body
			if body == nil {
				body = map[string]any{"code": "charge()"}
			}
			resp, err := sendHTTP(ctx, client, tc.url(t), http.MethodPost, nil, nil, body)
			if err == nil {
				t.Fatalf("expected a transport failure, got response %+v", resp)
			}

			got := ClassifyGoError(err)
			if got != tc.want {
				t.Errorf("code = %q, want %q\n  underlying error: %v", got, tc.want, err)
			}
			if tc.reached && got.IsNotReached() {
				t.Errorf("code %q asserts the remote was never reached, but it read the request; "+
					"isRetryAllowed would license a retry of a call that may already have run", got)
			}
			if tc.reached && !got.IsUnknowable() {
				t.Errorf("code %q is not in the unknowable set, so an only_once task could retry a "+
					"call whose outcome cannot be known", got)
			}
			if !tc.reached && !got.IsNotReached() {
				t.Errorf("code %q withholds a safe retry: the request provably never left", got)
			}
		})
	}
}

// TestClassifyGoError_HTTP2RequestThatReachedTheRemote covers the OTHER write path in
// net/http. h2 serialises a request as HEADERS frames rather than through Request.write, so
// if WroteRequest did not fire there every h2 failure would classify pre.* — and genroc
// speaks h2 to any real HTTPS endpoint, the shared transport keeping ForceAttemptHTTP2.
func TestClassifyGoError_HTTP2RequestThatReachedTheRemote(t *testing.T) {
	read := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		read <- struct{}{}
		panic(http.ErrAbortHandler) // received in full, then the stream dies
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	resp, err := sendHTTP(context.Background(), srv.Client(), srv.URL+"/eval",
		http.MethodPost, nil, nil, map[string]any{"code": "charge()"})
	if err == nil {
		t.Fatalf("expected the aborted stream to surface as an error, got %+v", resp)
	}
	select {
	case <-read:
	default:
		t.Fatal("handler never read the request, so this does not exercise a reached remote")
	}

	got := ClassifyGoError(err)
	if got != errcode.HTTPDisconnected {
		t.Errorf("code = %q, want %q\n  underlying error: %v", got, errcode.HTTPDisconnected, err)
	}
	if got.IsNotReached() {
		t.Errorf("code %q asserts the remote was never reached, but the handler read the whole "+
			"request body; on an only_once task isRetryAllowed would re-run a call that ran", got)
	}
}
