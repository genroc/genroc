package lsp

// The server loop. One goroutine, one document store, no shared state — a language server's
// work is per-document and fast enough here (parse plus inference on one file) that
// concurrency would buy latency nobody would notice and a class of races nobody wants.

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Server holds the open documents. Their text arrives from the editor and is authoritative:
// the file on disk may be older, so nothing here reads it.
type Server struct {
	conn *conn
	docs map[string]string // uri → the editor's current text
	// shutdown is set by the `shutdown` request, so a later `exit` reports success and any
	// other request is refused, as the protocol requires.
	shutdown bool
	version  string
}

func New(in io.Reader, out io.Writer, version string) *Server {
	return &Server{conn: newConn(in, out), docs: map[string]string{}, version: version}
}

// Run serves until the peer closes the stream or sends `exit`. It returns the exit code the
// protocol asks for: 0 after a `shutdown`, 1 otherwise.
func (s *Server) Run() int {
	for {
		req, err := s.conn.read()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return s.exitCode()
			}
			if errors.Is(err, errParse) {
				_ = s.conn.replyErr(nil, codeParseError, "%s", err)
				continue
			}
			return 1
		}
		if req.Method == "exit" {
			return s.exitCode()
		}
		s.handle(req)
	}
}

func (s *Server) exitCode() int {
	if s.shutdown {
		return 0
	}
	return 1
}

func (s *Server) handle(req *request) {
	switch req.Method {
	case "initialize":
		var res initializeResult
		res.Capabilities.TextDocumentSync = 1 // Full: each change carries the whole document
		res.ServerInfo.Name = "genctl-lsp"
		res.ServerInfo.Version = s.version
		_ = s.conn.reply(req.ID, res)

	case "initialized":
		// A notification with nothing to do; answering it would be a protocol error.

	case "shutdown":
		s.shutdown = true
		_ = s.conn.reply(req.ID, nil)

	case "textDocument/didOpen":
		var p didOpenParams
		if s.decode(req, &p) {
			s.set(p.TextDocument.URI, p.TextDocument.Text, &p.TextDocument.Version)
		}

	case "textDocument/didChange":
		var p didChangeParams
		if s.decode(req, &p) && len(p.ContentChanges) > 0 {
			// Full sync: the last change is the whole document.
			s.set(p.TextDocument.URI, p.ContentChanges[len(p.ContentChanges)-1].Text, &p.TextDocument.Version)
		}

	case "textDocument/didSave":
		// The text is already current; a save changes nothing this server knows.

	case "textDocument/didClose":
		var p didCloseParams
		if s.decode(req, &p) {
			delete(s.docs, p.TextDocument.URI)
			// Clearing is required: an editor keeps the last published set otherwise, and a
			// closed file's errors would outlive the file being open.
			_ = s.conn.notify("textDocument/publishDiagnostics",
				publishParams{URI: p.TextDocument.URI, Diagnostics: []diagnostic{}})
		}

	default:
		if !req.isNotification() {
			_ = s.conn.replyErr(req.ID, codeMethodNotFound, "unsupported method %q", req.Method)
		}
	}
}

// set stores the text and republishes. Analysis is synchronous: it is one file's parse and
// inference, and a debounce would only add a way for the editor to show a stale underline.
func (s *Server) set(uri, text string, version *int) {
	s.docs[uri] = text
	if !isDefinitionURI(uri) {
		return
	}
	_ = s.conn.notify("textDocument/publishDiagnostics", publishParams{
		URI:         uri,
		Version:     version,
		Diagnostics: analyse(text),
	})
}

// isDefinitionURI keeps the server to the files it is the authority on. An editor may route
// every YAML file here; `*.genroc.yaml` is what a definition is called.
func isDefinitionURI(uri string) bool {
	return strings.HasSuffix(uri, ".genroc.yaml") || strings.HasSuffix(uri, ".genroc.yml")
}

func (s *Server) decode(req *request, into any) bool {
	if err := json.Unmarshal(req.Params, into); err != nil {
		if !req.isNotification() {
			_ = s.conn.replyErr(req.ID, codeInternalError, "bad params for %s: %s", req.Method, err)
		}
		return false
	}
	return true
}
