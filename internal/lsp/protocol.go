package lsp

// The slice of LSP this server speaks. Written out rather than imported: the protocol is
// large and almost none of it is reachable from a server that publishes diagnostics.

type position struct {
	Line      int `json:"line"`      // 0-based
	Character int `json:"character"` // 0-based, UTF-16 code units
}

type textRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type diagnostic struct {
	Range    textRange `json:"range"`
	Severity int       `json:"severity"`
	Code     string    `json:"code,omitempty"`
	Source   string    `json:"source"`
	Message  string    `json:"message"`
}

const severityError = 1

type publishParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []diagnostic `json:"diagnostics"`
}

type textDocumentItem struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
	Text    string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type didChangeParams struct {
	TextDocument struct {
		URI     string `json:"uri"`
		Version int    `json:"version"`
	} `json:"textDocument"`
	// Full text only: the server advertises TextDocumentSyncKind.Full, so each change
	// carries the whole document and there is no incremental state to keep aligned.
	ContentChanges []struct {
		Text string `json:"text"`
	} `json:"contentChanges"`
}

type didCloseParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
}

// initializeResult advertises only what is implemented. textDocumentSync 1 is Full.
type initializeResult struct {
	Capabilities struct {
		TextDocumentSync   int             `json:"textDocumentSync"`
		HoverProvider      bool            `json:"hoverProvider"`
		CompletionProvider *completionOpts `json:"completionProvider,omitempty"`
		DefinitionProvider bool            `json:"definitionProvider"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type hoverParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position position `json:"position"`
}

type hoverResult struct {
	Contents markupContent `json:"contents"`
	Range    *textRange    `json:"range,omitempty"`
}

type markupContent struct {
	Kind  string `json:"kind"` // "markdown"
	Value string `json:"value"`
}

type completionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position position `json:"position"`
}

type completionItem struct {
	Label         string    `json:"label"`
	Kind          int       `json:"kind,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	Documentation string    `json:"documentation,omitempty"`
	SortText      string    `json:"sortText,omitempty"`
	TextEdit      *textEdit `json:"textEdit,omitempty"`

	// replaceFrom is the 1-based BYTE column the item overwrites from, which the server turns
	// into TextEdit once it has the line to convert against. Unexported: it never goes out.
	replaceFrom int
}

type textEdit struct {
	Range   textRange `json:"range"`
	NewText string    `json:"newText"`
}

const (
	kindField    = 5
	kindProperty = 10
	kindValue    = 12
)

// completionOpts asks the editor to re-request after a `.`, which is where a member list is
// wanted and where the client has nothing cached to filter.
type completionOpts struct {
	TriggerCharacters []string `json:"triggerCharacters"`
}

// location is the protocol's "here it is": a document and a range inside it.
type location struct {
	URI   string    `json:"uri"`
	Range textRange `json:"range"`
}

// initializeParams is the slice of the handshake this server reads: where the workspace is,
// so a reference to another process can be resolved to the file that defines it.
type initializeParams struct {
	RootURI          string `json:"rootUri"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
}
