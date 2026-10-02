package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeStructural registers `gen` as a structural resolver answering reply verbatim, beside a code
// resolver `imp` and the built-ins.
func fakeStructural(t *testing.T, reply string) projectConfig {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "reply.json"), []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectConfig{Root: root, Resolvers: append([]resolverConfig{
		{Name: "gen", Phase: phaseStructural, Command: []string{"sh", "-c", "cat >/dev/null; cat reply.json"}},
		{Name: "imp", Phase: phaseCode, Ext: []string{".ts"}, Command: []string{"false"}},
	}, builtins()...)}
}

func taskWithAction(action map[string]any) []sourceDoc {
	return []sourceDoc{{File: "proc.genroc.yaml", Value: map[string]any{
		"name":  "p",
		"tasks": []any{map[string]any{"id": "a", "action": action}},
	}}}
}

func TestStructuralPass_ADirectiveInAReturnedValueIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action map[string]any
		reply  string
		path   string
	}{
		{
			name:   "slot site",
			action: map[string]any{"type": "fetch", "url": "https://x", "body": "$gen: body"},
			reply:  `{"values": [{"inner": ["$gen: again"]}]}`,
			path:   "tasks.a.action.body.inner[0]",
		},
		{
			name:   "spread site",
			action: map[string]any{"<<": "$gen: action"},
			reply:  `{"values": [{"type": "child", "spec": "$process: ./c.genroc.yaml"}]}`,
			path:   "tasks.a.action.spec",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := taskWithAction(tc.action)
			_, err := resolveStructuralPass(docs, fakeStructural(t, tc.reply), nil)
			if err == nil {
				t.Fatalf("a structural directive inside a returned value was accepted; it would reach the server as a literal string: %v", docs[0].Value)
			}
			if !strings.Contains(err.Error(), `"gen"`) || !strings.Contains(err.Error(), tc.path) {
				t.Errorf("the refusal must name the resolver that returned it and the path it landed at (%s), got: %v", tc.path, err)
			}
		})
	}
}

func TestStructuralPass_AReturnedCodeDirectiveOrEscapeIsKept(t *testing.T) {
	docs := taskWithAction(map[string]any{"type": "fetch", "url": "https://x", "body": "$gen: body"})
	reply := `{"values": [{"code": "$imp: ./s.ts", "doc": "$$gen: literal"}]}`
	if _, err := resolveStructuralPass(docs, fakeStructural(t, reply), nil); err != nil {
		t.Fatalf("a code directive is the code phase's to resolve and an escaped one is a literal; neither may be refused: %v", err)
	}
}
