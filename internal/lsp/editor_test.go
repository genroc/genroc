package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The VS Code extension and this server have to agree on which files are a definition. They
// are written in different languages and neither imports the other, so nothing but this
// notices when one is edited and the other is not.
func TestTheVSCodeExtensionMatchesTheFilesThisServerAnswersFor(t *testing.T) {
	raw, err := os.ReadFile("../../editors/vscode/package.json")
	if err != nil {
		t.Skipf("extension not present: %v", err)
	}
	var pkg struct {
		ActivationEvents []string `json:"activationEvents"`
		Contributes      struct {
			Languages []struct {
				FilenamePatterns []string `json:"filenamePatterns"`
			} `json:"languages"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}

	patterns := []string{}
	for _, l := range pkg.Contributes.Languages {
		patterns = append(patterns, l.FilenamePatterns...)
	}
	if len(patterns) == 0 {
		t.Fatal("the extension claims no filenames, so it would never start the server")
	}
	for _, p := range patterns {
		// The glob's tail is the suffix; the server matches on exactly that.
		suffix := p[strings.LastIndexByte(p, '*')+1:]
		if !isDefinitionURI("file:///w/x" + suffix) {
			t.Errorf("the extension claims %q but the server does not answer for %q", p, suffix)
		}
	}

	for _, ev := range pkg.ActivationEvents {
		glob, ok := strings.CutPrefix(ev, "workspaceContains:")
		if !ok {
			continue
		}
		if !isDefinitionURI("file:///w/x" + glob[strings.LastIndexByte(glob, '*')+1:]) {
			t.Errorf("the extension activates on %q, which the server does not answer for", ev)
		}
	}
}

// VS Code loads a grammar only when the scopeName the extension contributes equals the one the
// grammar file declares. On a mismatch it loads neither, the language falls back, and its
// indentation rules go with it — which is how a cursor ends up at column 1 under a key.
func TestTheGrammarsScopeNameMatchesTheContribution(t *testing.T) {
	var pkg struct {
		Contributes struct {
			Grammars []struct {
				ScopeName string `json:"scopeName"`
				Path      string `json:"path"`
			} `json:"grammars"`
		} `json:"contributes"`
	}
	raw, err := os.ReadFile("../../editors/vscode/package.json")
	if err != nil {
		t.Skipf("extension not present: %v", err)
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	for _, g := range pkg.Contributes.Grammars {
		body, err := os.ReadFile(filepath.Join("../../editors/vscode", g.Path))
		if err != nil {
			t.Fatalf("%s: %v", g.Path, err)
		}
		var grammar struct {
			ScopeName string `json:"scopeName"`
		}
		if err := json.Unmarshal(body, &grammar); err != nil {
			t.Fatalf("%s: %v", g.Path, err)
		}
		if grammar.ScopeName != g.ScopeName {
			t.Errorf("package.json contributes %q for %s, which declares %q",
				g.ScopeName, g.Path, grammar.ScopeName)
		}
	}
}
