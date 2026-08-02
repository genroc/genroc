package transport

import (
	"context"
	"fmt"
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
	resp, err := sendHTTP(context.Background(), serveBody(t, MaxResponseBytes), http.MethodGet, nil, nil, nil)
	if err != nil {
		t.Fatalf("sendHTTP: %v", err)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("a body of exactly MaxResponseBytes must be accepted, got %q (%s) — an off-by-one here rejects "+
			"responses the limit is documented to allow", resp.ErrorCode, resp.ErrorMessage)
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
	if resp.ErrorCode != errcode.OutputTooLarge {
		t.Fatalf("got %q, want %q — an unbounded read here OOMs the worker and strands every lease it holds",
			resp.ErrorCode, errcode.OutputTooLarge)
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
	if resp.ErrorCode != errcode.OutputTooLarge {
		t.Fatalf("got %q, want %q", resp.ErrorCode, errcode.OutputTooLarge)
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

// nominal is the un-jittered delay RetryDelay is allowed to land under: plain 2^attempt
// seconds, capped. Written independently of the implementation's exponent clamp — 1<<29
// seconds is the last value that fits in a Duration, and anything past it is the cap
// regardless.
func nominal(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := maxRetryDelay
	if attempt < 30 {
		if exp := time.Duration(1<<uint(attempt)) * time.Second; exp < d {
			d = exp
		}
	}
	return d
}

func TestRetryDelay_StaysWithinTheNominalWindow(t *testing.T) {
	// Includes the attempt counts where the old shift overflowed: 63 wrapped negative and
	// 64+ shifted to zero, both of which retried with no backoff at all.
	for _, attempt := range []int{-1, 0, 1, 2, 8, 9, 10, 33, 62, 63, 64, 100, 1000} {
		want := nominal(attempt)
		for i := 0; i < 200; i++ {
			got := RetryDelay(attempt)
			if got < want/2 || got > want {
				t.Fatalf("RetryDelay(%d) = %v, want within [%v, %v]: a delay outside the window is either "+
					"a hot retry loop or a backoff past the documented ceiling", attempt, got, want/2, want)
			}
		}
	}
}

func TestRetryDelay_IsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[RetryDelay(6)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("RetryDelay(6) returned one value across 50 calls; without jitter every instance that "+
			"failed on the same outage wakes at the same instant (%v)", seen)
	}
}
