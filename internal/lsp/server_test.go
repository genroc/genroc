package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The server is driven the way an editor drives it: a framed session in, a framed session out.

func frame(method string, id any, params any) string {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(b), b)
}

// session runs one scripted conversation and returns every message the server sent.
func session(t *testing.T, frames ...string) ([]map[string]json.RawMessage, int) {
	t.Helper()
	var out bytes.Buffer
	code := New(strings.NewReader(strings.Join(frames, "")), &out, "test").Run()

	var msgs []map[string]json.RawMessage
	rest := out.String()
	for len(rest) > 0 {
		head, body, ok := strings.Cut(rest, "\r\n\r\n")
		if !ok {
			t.Fatalf("unframed trailing output: %q", rest)
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(head), "Content-Length: %d", &n); err != nil {
			t.Fatalf("bad header %q: %v", head, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body[:n]), &m); err != nil {
			t.Fatalf("bad body: %v", err)
		}
		msgs = append(msgs, m)
		rest = body[n:]
	}
	return msgs, code
}

func openDoc(uri, text string) string {
	return frame("textDocument/didOpen", nil, map[string]any{
		"textDocument": map[string]any{"uri": uri, "version": 1, "text": text},
	})
}

// published returns the diagnostics of the last publishDiagnostics notification.
func published(t *testing.T, msgs []map[string]json.RawMessage) publishParams {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		var method string
		if err := json.Unmarshal(msgs[i]["method"], &method); err != nil || method != "textDocument/publishDiagnostics" {
			continue
		}
		var p publishParams
		if err := json.Unmarshal(msgs[i]["params"], &p); err != nil {
			t.Fatalf("publishDiagnostics params: %v", err)
		}
		return p
	}
	t.Fatalf("the server published no diagnostics; it sent %d message(s)", len(msgs))
	return publishParams{}
}

const uri = "file:///w/demo.genroc.yaml"

// 1  name: demo
// 2  tasks:
// 3    - id: a
// 4      action:
// 5        type: fetch
// 6        url: "$: nope.x"
// 7      switch: end
const brokenURL = "name: demo\ntasks:\n  - id: a\n    action:\n      type: fetch\n      url: \"$: nope.x\"\n    switch: end\n"

const valid = "name: demo\ntasks:\n  - id: a\n    action:\n      type: fetch\n      url: \"https://example.com\"\n    switch: end\n"

func TestInitializeAdvertisesOnlyWhatIsImplemented(t *testing.T) {
	msgs, _ := session(t, frame("initialize", 1, map[string]any{}), frame("exit", nil, nil))
	if len(msgs) != 1 {
		t.Fatalf("initialize is answered once, got %d messages", len(msgs))
	}
	var res initializeResult
	if err := json.Unmarshal(msgs[0]["result"], &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Capabilities.TextDocumentSync != 1 {
		t.Errorf("the server keeps no incremental state, so it must ask for Full sync (1), got %d",
			res.Capabilities.TextDocumentSync)
	}
	if res.ServerInfo.Name != "genctl-lsp" {
		t.Errorf("serverInfo.name = %q", res.ServerInfo.Name)
	}
}

func TestOpeningABrokenDocumentPublishesItsDiagnostic(t *testing.T) {
	msgs, _ := session(t, openDoc(uri, brokenURL), frame("exit", nil, nil))
	p := published(t, msgs)
	if p.URI != uri {
		t.Errorf("published against %q, want %q", p.URI, uri)
	}
	if len(p.Diagnostics) != 1 {
		t.Fatalf("one broken slot, got %d: %+v", len(p.Diagnostics), p.Diagnostics)
	}
	d := p.Diagnostics[0]
	// The action slot, which yaml reports at its first key — line 5, 0-based 4.
	if d.Range.Start.Line != 4 {
		t.Errorf("the action starts on line 5 (0-based 4), got %d", d.Range.Start.Line)
	}
	if d.Severity != severityError {
		t.Errorf("a definition that will not register is an error, got severity %d", d.Severity)
	}
	if d.Code != "def.expression" || d.Source != "genroc" {
		t.Errorf("code/source = %q/%q", d.Code, d.Source)
	}
	if !strings.Contains(d.Message, `field "nope" not found`) {
		t.Errorf("message = %q", d.Message)
	}
}

// The editor keeps the last published set, so a fixed document must be published as empty
// rather than simply not republished.
func TestFixingADocumentPublishesAnEmptySet(t *testing.T) {
	msgs, _ := session(t,
		openDoc(uri, brokenURL),
		frame("textDocument/didChange", nil, map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": 2},
			"contentChanges": []any{map[string]any{"text": valid}},
		}),
		frame("exit", nil, nil))
	p := published(t, msgs)
	if len(p.Diagnostics) != 0 {
		t.Fatalf("a valid document must publish an empty set, got %+v", p.Diagnostics)
	}
	if p.Version == nil || *p.Version != 2 {
		t.Errorf("the publish must carry the version it describes, got %v", p.Version)
	}
}

func TestClosingADocumentClearsItsDiagnostics(t *testing.T) {
	msgs, _ := session(t,
		openDoc(uri, brokenURL),
		frame("textDocument/didClose", nil, map[string]any{
			"textDocument": map[string]any{"uri": uri},
		}),
		frame("exit", nil, nil))
	p := published(t, msgs)
	if len(p.Diagnostics) != 0 {
		t.Fatalf("a closed file's errors must not outlive it, got %+v", p.Diagnostics)
	}
}

// An editor may route every YAML file at this server; a definition is what it can answer for.
func TestAFileThatIsNotADefinitionIsNotAnalysed(t *testing.T) {
	msgs, _ := session(t, openDoc("file:///w/docker-compose.yaml", "services:\n  db: {}\n"), frame("exit", nil, nil))
	if len(msgs) != 0 {
		t.Fatalf("the server answered for a file it is not the authority on: %v", msgs)
	}
}

func TestShutdownThenExitIsACleanExit(t *testing.T) {
	_, code := session(t, frame("shutdown", 1, nil), frame("exit", nil, nil))
	if code != 0 {
		t.Errorf("exit after shutdown is 0, got %d", code)
	}
}

func TestExitWithoutShutdownIsAnError(t *testing.T) {
	_, code := session(t, frame("exit", nil, nil))
	if code != 1 {
		t.Errorf("the protocol asks for 1 when exit arrives without shutdown, got %d", code)
	}
}

func TestAnUnsupportedRequestIsRefusedRatherThanIgnored(t *testing.T) {
	msgs, _ := session(t, frame("textDocument/completion", 7, map[string]any{}), frame("exit", nil, nil))
	if len(msgs) != 1 {
		t.Fatalf("a request always gets a reply, got %d messages", len(msgs))
	}
	if _, ok := msgs[0]["error"]; !ok {
		t.Errorf("an unimplemented method must answer with an error, got %v", msgs[0])
	}
}

// A notification has no id, and replying to one is a protocol error.
func TestAnUnsupportedNotificationIsSilent(t *testing.T) {
	msgs, _ := session(t, frame("$/setTrace", nil, map[string]any{"value": "off"}), frame("exit", nil, nil))
	if len(msgs) != 0 {
		t.Fatalf("a notification must not be answered, got %v", msgs)
	}
}
