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
	"net"
	"net/http"
	"strconv"
	"strings"

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
// ErrorCode is non-empty on failure ("http.404", "output.parse", "output.too_large",
// "pre.timeout", etc.).
// ErrorMessage is a human-readable description of the failure (may include trimmed response body).
// Body holds the raw decoded JSON body on success.
// Status is the HTTP status code for a REST call (success or failure); 0 for non-HTTP transports.
type Response struct {
	Body         any
	ErrorCode    errcode.Code
	ErrorMessage string
	Status       int
}

// Send dispatches a fetch HTTP request. url, method, acceptedStatus, and headers are
// pre-resolved (accepted_status is a shape evaluated by the engine); body is the raw
// payload — an object is marshaled to JSON, a string sent as-is, nil sends no body.
func Send(ctx context.Context, call *model.Action, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
	switch call.Type {
	case model.ActionTypeFetch:
		return sendHTTP(ctx, url, method, acceptedStatus, headers, body)
	default:
		return nil, fmt.Errorf("unknown call type: %q", call.Type)
	}
}

func sendHTTP(ctx context.Context, url, method string, acceptedStatus []string, headers map[string]string, body any) (*Response, error) {
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, err // caller uses ClassifyGoError
	}
	defer resp.Body.Close()

	if !matchAcceptedStatus(resp.StatusCode, acceptedStatus) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("request failed with status %d without response body", resp.StatusCode)
		}
		return &Response{ErrorCode: errcode.HTTP(resp.StatusCode), ErrorMessage: msg, Status: resp.StatusCode}, nil
	}

	// One byte past the cap, so draining the allowance is itself the proof the body
	// exceeded it — checked on both exits, since a body over the limit may equally well
	// parse (a huge but valid value) or fail (a value truncated mid-token).
	limited := &io.LimitedReader{R: resp.Body, N: MaxResponseBytes + 1}
	var b any
	err = numeric.DecodeReader(limited, &b)
	if limited.N <= 0 {
		return &Response{
			ErrorCode:    errcode.OutputTooLarge,
			ErrorMessage: fmt.Sprintf("response body exceeds the %d-byte limit a fetch will read", MaxResponseBytes),
			Status:       resp.StatusCode,
		}, nil
	}
	if err != nil {
		return &Response{ErrorCode: errcode.OutputParse, Status: resp.StatusCode}, nil
	}
	return &Response{Body: b, Status: resp.StatusCode}, nil
}

func methodAllowsBody(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
}

// matchAcceptedStatus reports whether code is covered by patterns — "2xx".."5xx"
// hundred-ranges or exact 3-digit strings like "404". Empty patterns accepts any 2xx.
func matchAcceptedStatus(code int, patterns []string) bool {
	if len(patterns) == 0 {
		return code >= 200 && code <= 299
	}
	for _, p := range patterns {
		if len(p) == 3 && p[1] == 'x' && p[2] == 'x' {
			hundreds := int(p[0]-'0') * 100
			if code >= hundreds && code <= hundreds+99 {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && n == code {
			return true
		}
	}
	return false
}

// ValidStatusPattern reports whether p is a well-formed accepted_status pattern — a
// hundred-range "2xx".."5xx" or an exact 3-digit code like "404" — i.e. a pattern
// matchAcceptedStatus can ever match. It is the format the registration validator applies
// to statically-known (literal) patterns; a dynamic pattern (from an expression) is only
// known at runtime, where an unrecognized value simply never matches.
func ValidStatusPattern(p string) bool {
	if len(p) == 3 && p[1] == 'x' && p[2] == 'x' && p[0] >= '1' && p[0] <= '5' {
		return true
	}
	if len(p) == 3 {
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// ClassifyGoError maps a transport-level Go error (a REST call that never got an HTTP
// response) to an error code: pre.timeout / pre.error for a failure during the dial phase
// (server never received the request), http.timeout when the connection was established
// but no response arrived in time.
func ClassifyGoError(err error) errcode.Code {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		var netErr *net.OpError
		if errors.As(err, &netErr) && netErr.Op == "dial" {
			return errcode.PreTimeout
		}
		return errcode.HTTPTimeout
	}
	return errcode.PreError
}
