package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"genroc/internal/errcode"
	"genroc/internal/numeric"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"genroc/internal/model"
)

// MaxResponseBytes caps the body a fetch reads into memory. A worker holds leases on every
// instance it claimed, so an OOM here strands all of them until those leases expire — one
// endpoint streaming an unbounded body must not be able to do that. Far above the 2 KiB at
// which a value externalizes to the object store, so it bounds the pathological case
// without capping a legitimately large result.
const MaxResponseBytes = 8 << 20

// Shared by every fetch; deliberately NO Client.Timeout — the per-attempt budget is the
// caller's context deadline, and a second ceiling would silently override declared
// timeouts. Idle limits raised: stdlib's MaxIdleConnsPerHost=2 re-dials (and re-handshakes
// TLS) nearly every call.
var client = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: t}
}()

// Identity headers genroc stamps on every fetch request so the receiving service can
// correlate a call back to the instance/task that made it — the context the request
// body used to carry as an envelope before fetch switched to a raw body.
const (
	HeaderInstanceID = "X-Genroc-Instance-Id"
	HeaderTaskID     = "X-Genroc-Task-Id"
)

// Response carries the result of a Send call.
// Body holds the decoded JSON body, on an unaccepted status as much as on an accepted one —
// whether an error body is readable is the caller's decision, not the transport's.
// BodyCode reports why Body is absent when the bytes could not be decoded ("output.parse",
// "output.too_large"); it is NOT a verdict, because a status nobody declared a schema for is
// entitled to answer with HTML.
// ErrorCode is non-empty ONLY when the status was not accepted ("http.404"); a body problem
// never lands here, so the caller can tell "the remote refused" from "the body was unreadable"
// and apply the declaration to each.
// ErrorMessage is a human-readable description of the failure (may include trimmed response body).
// Headers are the response headers, lowercased and comma-joined (see responseHeaders).
// Status is the HTTP status code for a REST call (success or failure); 0 for non-HTTP transports.
type Response struct {
	Body         any
	Headers      map[string]string
	BodyCode     errcode.Code
	ErrorCode    errcode.Code
	ErrorMessage string
	Status       int
}

// errorMessageBytes is how much of an unaccepted response is kept as human-readable text.
// The whole body is still decoded into Body; this is the operator's copy, and it stays short
// because it lands in an audit row.
const errorMessageBytes = 512

// Send dispatches a fetch HTTP request. url, method, acceptedStatus, and headers are
// pre-resolved (accepted_status is a shape evaluated by the engine); body is the raw
// payload — an object is marshaled to JSON, a string sent as-is, nil sends no body.
func Send(ctx context.Context, call *model.Action, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	switch call.Type {
	case model.ActionTypeFetch:
		return sendHTTP(ctx, client, url, method, acceptedStatus, headers, body)
	default:
		return nil, notSent{fmt.Errorf("unknown call type: %q", call.Type)}
	}
}

// notSent marks a failure that never even acquired a connection — the positive evidence
// pre.* asserts. Anything weaker than that is a guess: pre.* licenses a retry on an
// only_once task, so "we did not observe a write" is not enough, only "there was nothing
// to write to".
type notSent struct{ err error }

func (e notSent) Error() string { return e.err.Error() }
func (e notSent) Unwrap() error { return e.err }

// sendHTTP wraps doHTTP solely to apply that mark in ONE place: every failure before the
// request hits the wire is caught here, so a new early return in doHTTP cannot silently
// inherit the unknowable default.
func sendHTTP(ctx context.Context, c *http.Client, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	var mayHaveSent atomic.Bool
	// GotConn is the load-bearing half: it is delivered before the request is handed to the
	// write goroutine, so "no connection" is stable by the time Do returns, whereas
	// WroteRequest races that return. No connection, no bytes. WroteRequest is kept as the
	// belt to that braces — it can only widen the answer, never narrow it.
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { mayHaveSent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { mayHaveSent.Store(true) },
	})
	resp, err := doHTTP(ctx, c, url, method, acceptedStatus, headers, body)
	if err != nil && !mayHaveSent.Load() {
		return nil, notSent{err}
	}
	return resp, err
}

func doHTTP(ctx context.Context, c *http.Client, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	if method == "" {
		method = http.MethodPost
	}
	var bodyReader io.Reader
	jsonBody := false
	if body != nil && methodAllowsBody(method) {
		switch b := body.(type) {
		case string:
			bodyReader = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				return nil, fmt.Errorf("marshal body: %w", err)
			}
			bodyReader = bytes.NewReader(raw)
			jsonBody = true
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build http request: %w", err)
	}
	// Default JSON content type for an object body; a header may override it.
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, err // caller uses ClassifyGoError
	}
	defer resp.Body.Close()

	if !model.MatchAnyStatus(resp.StatusCode, acceptedStatus) {
		// Buffered rather than streamed: this exit needs the same bytes twice, as a decoded
		// value for error.data and as text for the operator.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
		msg := strings.TrimSpace(string(raw))
		if len(msg) > errorMessageBytes {
			msg = msg[:errorMessageBytes]
		}
		if msg == "" {
			msg = fmt.Sprintf("request failed with status %d without response body", resp.StatusCode)
		}
		body, code := decodeBytes(raw)
		return &Response{
			Body:         body,
			Headers:      responseHeaders(resp.Header),
			BodyCode:     code,
			ErrorCode:    errcode.HTTP(resp.StatusCode),
			ErrorMessage: msg,
			Status:       resp.StatusCode,
		}, nil
	}

	// One byte past the cap, so draining the allowance is itself the proof the body
	// exceeded it — checked on both exits, since a body over the limit may equally well
	// parse (a huge but valid value) or fail (a value truncated mid-token).
	limited := &io.LimitedReader{R: resp.Body, N: MaxResponseBytes + 1}
	var b any
	err = numeric.DecodeReader(limited, &b)
	if limited.N <= 0 {
		return &Response{
			Headers:      responseHeaders(resp.Header),
			BodyCode:     errcode.OutputTooLarge,
			ErrorMessage: fmt.Sprintf("response body exceeds the %d-byte limit a fetch will read", MaxResponseBytes),
			Status:       resp.StatusCode,
		}, nil
	}
	// An empty body is a value (null), not a parse failure: 204, an async 202 and a webhook
	// ACK all answer with nothing, and calling that malformed is what made them unwritable.
	if errors.Is(err, io.EOF) {
		return &Response{Headers: responseHeaders(resp.Header), Status: resp.StatusCode}, nil
	}
	if err != nil {
		return &Response{Headers: responseHeaders(resp.Header), BodyCode: errcode.OutputParse, Status: resp.StatusCode}, nil
	}
	return &Response{Body: b, Headers: responseHeaders(resp.Header), Status: resp.StatusCode}, nil
}

// decodeBytes decodes an already-buffered body, reporting why it could not be read rather
// than failing: the caller pairs this with the declaration to decide whether it matters.
// len(raw) is past the cap only because the reader was given MaxResponseBytes+1.
func decodeBytes(raw []byte) (any, errcode.Code) {
	if len(raw) > MaxResponseBytes {
		return nil, errcode.OutputTooLarge
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ""
	}
	var v any
	if err := numeric.DecodeReader(bytes.NewReader(raw), &v); err != nil {
		return nil, errcode.OutputParse
	}
	return v, ""
}

// responseHeaders flattens a response's headers into the flat object<string> a definition
// reads. Keys are LOWERCASED: Go canonicalises to `Retry-After`, so a canonicalised map would
// make `self.headers['retry-after']` silently null — predictability beats fidelity, and
// browsers lowercase too. Repeated headers are comma-joined so the type stays flat; Set-Cookie
// is the accepted casualty of that.
func responseHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		out[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return out
}

func methodAllowsBody(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
}

// ClassifyGoError maps a transport-level Go error (a REST call that never got an HTTP
// response) to an error code. The split is retry safety, not diagnosis: pre.* asserts the
// remote CANNOT have seen the request, so only a failure sendHTTP marked notSent earns it —
// an outcome merely believed not to have happened is unknowable and stays that way.
// Everything else is unknowable — a connection that breaks once the bytes are out cannot
// say whether the remote already acted, and a reset is indistinguishable at the client
// from a server that processed the request and died answering it.
func ClassifyGoError(err error) errcode.Code {
	var unsent notSent
	sent := !errors.As(err, &unsent)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		if sent {
			return errcode.HTTPTimeout
		}
		return errcode.PreTimeout
	case sent:
		return errcode.HTTPDisconnected
	default:
		return errcode.PreError
	}
}
