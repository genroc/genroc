package lsp

// JSON-RPC 2.0 over stdio, in the framing LSP uses: a `Content-Length` header, a blank line,
// then the body. Hand-written rather than taken from a library — it is this much code, and a
// language server acquires dependencies fast enough without starting with one.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the peer expects no reply. Answering one is a protocol
// error, and the difference is only the presence of an id.
func (r *request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// conn is one stdio peer. The write mutex is a field rather than a package var because it
// guards this connection's writer and nothing else: two goroutines interleaving a header and
// a body would produce a frame neither of them wrote.
type conn struct {
	in  *bufio.Reader
	out io.Writer
	mu  sync.Mutex
}

func newConn(in io.Reader, out io.Writer) *conn {
	return &conn{in: bufio.NewReader(in), out: out}
}

// read returns the next message, or io.EOF when the peer closed. A malformed header is fatal
// to the stream: the framing is how message boundaries are found, so there is no resyncing.
func (c *conn) read() (*request, error) {
	length := -1
	for {
		line, err := c.in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "content-length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message with no Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.in, body); err != nil {
		return nil, err
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("%w: %s", errParse, err)
	}
	return &req, nil
}

var errParse = fmt.Errorf("parse error")

func (c *conn) reply(id json.RawMessage, result any) error {
	return c.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *conn) replyErr(id json.RawMessage, code int, format string, a ...any) error {
	return c.send(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: fmt.Sprintf(format, a...)}})
}

// notify sends a server-initiated message, which is a request with no id.
func (c *conn) notify(method string, params any) error {
	return c.send(request{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
}

func (c *conn) send(msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.out, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.out.Write(body)
	return err
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
