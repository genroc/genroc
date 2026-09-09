package lsp

// The server loop. One goroutine, one document store, no shared state — a language server's
// work is per-document and fast enough here (parse plus inference on one file) that
// concurrency would buy latency nobody would notice and a class of races nobody wants.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	// folders are the workspace roots from `initialize`, searched for a process a child
	// action names. Empty means single-file mode: only open documents are reachable.
	folders []string
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
			// stderr, never stdout: stdout is the protocol. A session that ends on a read
			// error said nothing at all, which reads as a server that simply stopped — and is
			// how a non-blocking stdin cost an afternoon to find.
			fmt.Fprintf(os.Stderr, "genroc: reading from the editor: %v\n", err)
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
		var p initializeParams
		if s.decode(req, &p) {
			s.folders = workspaceRoots(p)
		}
		var res initializeResult
		res.Capabilities.TextDocumentSync = 1 // Full: each change carries the whole document
		res.Capabilities.HoverProvider = true
		res.Capabilities.CompletionProvider = &completionOpts{TriggerCharacters: []string{".", "$"}}
		res.Capabilities.DefinitionProvider = true
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

	case "textDocument/hover":
		var p hoverParams
		if !s.decode(req, &p) {
			return
		}
		_ = s.conn.reply(req.ID, s.hover(p))

	case "textDocument/completion":
		var p completionParams
		if !s.decode(req, &p) {
			return
		}
		_ = s.conn.reply(req.ID, s.complete(p))

	case "textDocument/definition":
		var p hoverParams // same shape: a document and a position
		if !s.decode(req, &p) {
			return
		}
		_ = s.conn.reply(req.ID, s.definition(p))

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

// hover answers for one position, or with null — which is the protocol's "nothing to say" and
// the answer for most of a document.
func (s *Server) hover(p hoverParams) any {
	text, open := s.docs[p.TextDocument.URI]
	if !open || !isDefinitionURI(p.TextDocument.URI) {
		return nil
	}
	lines := splitLines(text)
	md, r, ok := hoverAt(text, p.Position.Line+1, byteColumn(lines, p.Position))
	if !ok {
		return nil
	}
	rng := toRange(lines, r)
	return hoverResult{Contents: markupContent{Kind: "markdown", Value: md}, Range: &rng}
}

// complete answers with a list, never with null: an empty list means "nothing here", where
// null makes some clients fall back to guessing from the buffer's words.
func (s *Server) complete(p completionParams) []completionItem {
	text, open := s.docs[p.TextDocument.URI]
	if !open || !isDefinitionURI(p.TextDocument.URI) {
		return []completionItem{}
	}
	lines := splitLines(text)
	line := p.Position.Line + 1
	items := completeAt(text, line, byteColumn(lines, p.Position))
	if items == nil {
		return []completionItem{}
	}
	for i, it := range items {
		if it.replaceFrom == 0 {
			continue
		}
		start := toPosition(lines, line, it.replaceFrom)
		end := p.Position
		if it.replaceTo > 0 {
			end = toPosition(lines, line, it.replaceTo)
		}
		text := it.Label
		if it.insert != "" {
			text = it.insert
		}
		items[i].TextEdit = &textEdit{
			Range:   textRange{Start: start, End: end},
			NewText: text,
		}
	}
	return items
}

// definition answers with the one location a reference points at, or null. A list would be the
// protocol's other option, but a `goto` names exactly one task or no task at all.
func (s *Server) definition(p hoverParams) any {
	text, open := s.docs[p.TextDocument.URI]
	if !open || !isDefinitionURI(p.TextDocument.URI) {
		return nil
	}
	lines := splitLines(text)
	ref, doc, ok := referenceAt(text, p.Position.Line+1, byteColumn(lines, p.Position))
	if !ok {
		return nil
	}
	if ref.taskPath != "" {
		span, found := doc.Span(ref.taskPath)
		if !found {
			return nil
		}
		return location{URI: p.TextDocument.URI, Range: toRange(lines, span.Value)}
	}

	uri, r, found := s.findProcess(ref.process)
	if !found {
		return nil
	}
	// The target's own text decides its columns, not this document's.
	target, open := s.docs[uri]
	if !open {
		if path, ok := uriToPath(uri); ok {
			if data, err := os.ReadFile(path); err == nil {
				target = string(data)
			}
		}
	}
	return location{URI: uri, Range: toRange(splitLines(target), r)}
}

// workspaceRoots reads the roots out of the handshake. workspaceFolders is the current field
// and rootUri the one before it; an editor sends one or the other and older ones send only
// rootUri, so both are read and duplicates collapse.
func workspaceRoots(p initializeParams) []string {
	seen := map[string]bool{}
	var out []string
	add := func(uri string) {
		path, ok := uriToPath(uri)
		if !ok || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, f := range p.WorkspaceFolders {
		add(f.URI)
	}
	add(p.RootURI)
	return out
}
