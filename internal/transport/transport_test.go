package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	resp, err := sendHTTP(context.Background(), serveBody(t, MaxResponseBytes), http.MethodGet, nil, nil, nil)
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
	resp, err := sendHTTP(context.Background(), serveBody(t, MaxResponseBytes+1), http.MethodGet, nil, nil, nil)
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

	resp, err := sendHTTP(context.Background(), srv.URL, http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.BodyCode != errcode.OutputTooLarge {
		t.Fatalf("got %q, want %q", resp.BodyCode, errcode.OutputTooLarge)
	}
}

func TestSendHTTP_ShortBodyStillParses(t *testing.T) {
	resp, err := sendHTTP(context.Background(), serveBody(t, 16), http.MethodGet, nil, nil, nil)
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

			resp, err := sendHTTP(context.Background(), srv.URL, http.MethodGet, nil, nil, nil)
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

	resp, err := sendHTTP(context.Background(), srv.URL, http.MethodGet, nil, nil, nil)
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

	resp, err := sendHTTP(context.Background(), srv.URL, http.MethodGet, nil, nil, nil)
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
