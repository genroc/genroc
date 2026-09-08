package lsp

import (
	"strings"
	"unicode/utf16"

	"genroc/internal/defdoc"
)

// The protocol counts lines from 0 and columns in UTF-16 code units from 0; defdoc reports
// yaml.v3's 1-based line and 1-based BYTE column. Converting needs the source text, which is
// why it happens here and not in defdoc: an index built once outlives any one editor's idea
// of a column.
func toRange(lines []string, r defdoc.Range) textRange {
	return textRange{
		Start: toPosition(lines, r.Line, r.Col),
		End:   toPosition(lines, r.EndLine, r.EndCol),
	}
}

func toPosition(lines []string, line, col int) position {
	i := line - 1
	if i < 0 {
		return position{}
	}
	if i >= len(lines) {
		return position{Line: len(lines), Character: 0}
	}
	return position{Line: i, Character: utf16Column(lines[i], col)}
}

// utf16Column converts a 1-based byte column to the 0-based UTF-16 offset. A column past the
// end of the line clamps to its end rather than pointing into the next one.
func utf16Column(line string, col int) int {
	b := col - 1
	if b <= 0 {
		return 0
	}
	if b > len(line) {
		b = len(line)
	}
	return len(utf16.Encode([]rune(line[:b])))
}

func splitLines(text string) []string { return strings.Split(text, "\n") }
