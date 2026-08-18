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
// Body holds the decoded JSON body, on an unaccepted status as much as on an accepted one —
// whether an error body is readable is the caller's decision, not the transport's.
// BodyCode reports why Body is absent when the bytes could not be decoded ("output.parse",
// "output.too_large"); it is NOT a verdict, because a status nobody declared a schema for is
// entitled to answer with HTML.
// ErrorCode is non-empty ONLY when the status was not accepted ("http.404"); a body problem
// never lands here, so the caller can tell "the remote refused" from "the body was unreadable"
// and apply the declaration to each.
// ErrorMessage is a human-readable description of the failure (may include trimmed response body).
// Status is the HTTP status code for a REST call (success or failure); 0 for non-HTTP transports.
type Response struct {
	Body         any
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
			BodyCode:     errcode.OutputTooLarge,
			ErrorMessage: fmt.Sprintf("response body exceeds the %d-byte limit a fetch will read", MaxResponseBytes),
			Status:       resp.StatusCode,
		}, nil
	}
	// An empty body is a value (null), not a parse failure: 204, an async 202 and a webhook
	// ACK all answer with nothing, and calling that malformed is what made them unwritable.
	if errors.Is(err, io.EOF) {
		return &Response{Status: resp.StatusCode}, nil
	}
	if err != nil {
		return &Response{BodyCode: errcode.OutputParse, Status: resp.StatusCode}, nil
	}
	return &Response{Body: b, Status: resp.StatusCode}, nil
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

func methodAllowsBody(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
		return false
	}
	return true
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
