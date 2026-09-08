package lsp

import (
	"encoding/json"
	"os"
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
