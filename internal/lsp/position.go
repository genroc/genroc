package lsp

import (
	"strings"
	"unicode/utf16"

	"genroc/internal/defdoc"
)

// The protocol counts from 0, columns in UTF-16 code units; defdoc reports 1-based lines and
// BYTE columns. Converting needs the source text, which is why it happens here, not in defdoc.
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

// byteColumn is the inverse of utf16Column: the protocol hands a UTF-16 offset, and defdoc
// indexes by byte column.
func byteColumn(lines []string, p position) int {
	if p.Line < 0 || p.Line >= len(lines) {
		return 1
	}
	line := lines[p.Line]
	units := 0
	for i, r := range line {
		if units >= p.Character {
			return i + 1
		}
		units += len(utf16.Encode([]rune{r}))
	}
	return len(line) + 1
}
