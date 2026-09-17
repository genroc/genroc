package lsp

import (
	"encoding/json"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"slices"
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

// Expressions live inside quoted strings, and VS Code suppresses suggestions there unless a
// language says otherwise — which is why completion only appeared on Ctrl+Space. Nothing in
// the protocol can override it; it has to be a default the extension contributes.
func TestTheExtensionEnablesSuggestionsInsideStrings(t *testing.T) {
	raw, err := os.ReadFile("../../editors/vscode/package.json")
	if err != nil {
		t.Skipf("extension not present: %v", err)
	}
	var pkg struct {
		Contributes struct {
			ConfigurationDefaults map[string]struct {
				QuickSuggestions map[string]bool `json:"editor.quickSuggestions"`
				ShowWords        *bool           `json:"editor.suggest.showWords"`
			} `json:"configurationDefaults"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	lang, ok := pkg.Contributes.ConfigurationDefaults["[genroc]"]
	if !ok {
		t.Fatal("the extension contributes no defaults for its own language")
	}
	if !lang.QuickSuggestions["strings"] {
		t.Error("suggestions inside strings are off, so a `$:` expression completes only on Ctrl+Space")
	}
	if lang.ShowWords == nil || *lang.ShowWords {
		t.Error("word-based suggestions are on, and they rank beside the vocabulary the server knows")
	}
}

// The marketplace lists an extension by its icon, and vsce only checks the path when it
// packages — which `make test` does not run. A missing or undersized icon therefore fails at
// publish time, long after the change that broke it.
func TestTheExtensionShipsTheIconItDeclares(t *testing.T) {
	raw, err := os.ReadFile("../../editors/vscode/package.json")
	if err != nil {
		t.Skipf("extension not present: %v", err)
	}
	var pkg struct {
		Icon string `json:"icon"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	if pkg.Icon == "" {
		t.Fatal("the extension declares no icon, so the marketplace lists it with a placeholder")
	}

	f, err := os.Open(filepath.Join("../../editors/vscode", pkg.Icon))
	if err != nil {
		t.Fatalf("package.json points at %q, which is not there: %v", pkg.Icon, err)
	}
	defer f.Close()

	// PNG specifically: vsce refuses an SVG, and a JPEG named .png would pass a path check.
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("%s does not decode as an image: %v", pkg.Icon, err)
	}
	if format != "png" {
		t.Errorf("%s is a %s; the marketplace takes a PNG", pkg.Icon, format)
	}
	if cfg.Width < 128 || cfg.Height < 128 {
		t.Errorf("%s is %dx%d; the marketplace minimum is 128x128", pkg.Icon, cfg.Width, cfg.Height)
	}
}

// `.genroc` is YAML, and the extension says so -- with the YAML extension present, that is also
// what attaches the schema `make extension` generates. Three things are silent when broken: an
// association on the wrong language id, a `yamlValidation` url naming a file the build does not
// write, and a `.vscodeignore` that drops it from the .vsix.
func TestTheExtensionAssociatesTheProjectFileWithYAMLAndItsSchema(t *testing.T) {
	raw, err := os.ReadFile("../../editors/vscode/package.json")
	if err != nil {
		t.Skipf("extension not present: %v", err)
	}
	var pkg struct {
		Contributes struct {
			Languages []struct {
				ID        string   `json:"id"`
				Filenames []string `json:"filenames"`
			} `json:"languages"`
			YAMLValidation []struct {
				FileMatch []string `json:"fileMatch"`
				URL       string   `json:"url"`
			} `json:"yamlValidation"`
		} `json:"contributes"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}

	asYAML := false
	for _, l := range pkg.Contributes.Languages {
		if l.ID == "yaml" && slices.Contains(l.Filenames, ".genroc") {
			asYAML = true
		}
	}
	if !asYAML {
		t.Error("`.genroc` is not contributed to the `yaml` language, so it is neither highlighted nor validated")
	}

	url := ""
	for _, v := range pkg.Contributes.YAMLValidation {
		if slices.Contains(v.FileMatch, "/.genroc") {
			url = v.URL
		}
	}
	if url == "" {
		t.Fatal("no yamlValidation entry matches /.genroc")
	}
	rel := strings.TrimPrefix(url, "./")
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "editors/vscode/"+rel) {
		t.Errorf("package.json points at %s, which no Makefile target generates -- the .vsix would ship without it", url)
	}
	ignore, _ := os.ReadFile("../../editors/vscode/.vscodeignore")
	for _, line := range strings.Split(string(ignore), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "schemas") {
			t.Errorf(".vscodeignore excludes %q, so the schema package.json points at is not in the .vsix", line)
		}
	}
}
