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
	"time"

	"genroc/internal/model"
)

// Identity headers genroc stamps on every fetch request so the receiving service can
// correlate a call back to the instance/task that made it — the context the request
// body used to carry as an envelope before fetch switched to a raw body.
const (
	HeaderInstanceID = "X-Genroc-Instance-Id"
	HeaderTaskID     = "X-Genroc-Task-Id"
)

// Response carries the result of a Send call.
// ErrorCode is non-empty on failure ("http.404", "output.parse", "pre.timeout", etc.).
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

	resp, err := http.DefaultClient.Do(req)
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

	var b any
	if err := numeric.DecodeReader(resp.Body, &b); err != nil {
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

// RetryDelay returns the backoff duration for a given retry attempt (exponential, capped at 5 min).
func RetryDelay(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// MatchCode reports whether the error code s matches the pattern p. '%' is the only
// wildcard: it matches any sequence of characters (including none). Every other character
// is literal — in particular '_' and '.' match themselves, because both are ordinary
// characters in an error code (snake_case, and dotted engine codes like http.500 /
// pre.timeout). This is deliberately NOT full SQL LIKE: LIKE's '_' single-char wildcard
// is a footgun for codes that contain underscores, so `order_%` matches `order_placed` but
// not `order.placed`.
func MatchCode(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			p = p[1:]
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if MatchCode(p, s[i:]) {
					return true
				}
			}
			return false
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

