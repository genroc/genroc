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

// MaxResponseBytes caps the body a fetch reads into memory: an OOM here strands every lease
// the worker holds. specs/resource-limits.md.
const MaxResponseBytes = 8 << 20

// Deliberately NO Client.Timeout: the budget is the caller's context deadline. Idle limits
// raised: stdlib's 2 per host re-handshakes TLS nearly every call. CLAUDE.md.
var client = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64
	return &http.Client{Transport: t}
}()

// Identity headers stamped on every fetch, so the receiver can correlate a call to its
// instance and task.
const (
	HeaderInstanceID = "X-Genroc-Instance-Id"
	HeaderTaskID     = "X-Genroc-Task-Id"
)

// Response decodes Body on any status. BodyCode says why Body is absent and is NOT a verdict
// (an undeclared status may answer with HTML); ErrorCode is set ONLY for an unaccepted status.
type Response struct {
	Body         any
	Headers      map[string]string
	BodyCode     errcode.Code
	ErrorCode    errcode.Code
	ErrorMessage string
	Status       int
}

// errorMessageBytes is the operator's copy of an unaccepted body, short because it lands in
// an audit row.
const errorMessageBytes = 512

// Send takes every slot pre-resolved. An object body is marshaled to JSON, a string sent
// as-is, and nil sends no body.
func Send(ctx context.Context, call *model.Action, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	switch call.Type {
	case model.ActionTypeFetch:
		return sendHTTP(ctx, client, url, method, acceptedStatus, headers, body)
	default:
		return nil, notSent{fmt.Errorf("unknown call type: %q", call.Type)}
	}
}

// notSent marks a failure that never acquired a connection, the positive evidence pre.*
// asserts: "we did not observe a write" is not enough. CLAUDE.md.
type notSent struct{ err error }

func (e notSent) Error() string { return e.err.Error() }
func (e notSent) Unwrap() error { return e.err }

// sendHTTP wraps doHTTP solely to apply that mark in ONE place, so a new early return in
// doHTTP cannot silently inherit the unknowable default.
func sendHTTP(ctx context.Context, c *http.Client, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	var mayHaveSent atomic.Bool
	// GotConn is the load-bearing half; WroteRequest races Do's return and can only widen the
	// answer.
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
	// net/http reads an empty method as GET. Callers resolve the verb (it is required on a
	// fetch); refusing here keeps the one unnamed case from becoming a silent read.
	if method == "" {
		return nil, fmt.Errorf("http method is empty")
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

	// One byte past the cap: draining the allowance is the proof, checked before the decode
	// error. CLAUDE.md.
	limited := &io.LimitedReader{R: resp.Body, N: MaxResponseBytes + 1}
	var b any
	err = numeric.DecodeReader(limited, &b)
	if limited.N <= 0 {
		return &Response{
			Headers:      responseHeaders(resp.Header),
			BodyCode:     errcode.ResultTooLarge,
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
		return &Response{Headers: responseHeaders(resp.Header), BodyCode: errcode.ResultParse, Status: resp.StatusCode}, nil
	}
	return &Response{Body: b, Headers: responseHeaders(resp.Header), Status: resp.StatusCode}, nil
}

// decodeBytes reports rather than fails: the caller pairs it with the declaration. len(raw)
// is past the cap only because the reader was given MaxResponseBytes+1.
func decodeBytes(raw []byte) (any, errcode.Code) {
	if len(raw) > MaxResponseBytes {
		return nil, errcode.ResultTooLarge
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ""
	}
	var v any
	if err := numeric.DecodeReader(bytes.NewReader(raw), &v); err != nil {
		return nil, errcode.ResultParse
	}
	return v, ""
}

// responseHeaders LOWERCASES keys, or `self.headers['retry-after']` reads a silent null.
// Repeats are comma-joined to keep the type flat; Set-Cookie is the accepted casualty.
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

// ClassifyGoError splits by retry safety, not diagnosis: pre.* asserts the remote CANNOT have
// seen the request, so only a failure sendHTTP marked notSent earns it.
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
