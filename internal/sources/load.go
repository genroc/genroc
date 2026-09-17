package sources

// Reading definition files into documents. The file each document came from is kept: a
// directive's path resolves against it, and an error has to name it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"genroc/internal/defdoc"
	"genroc/internal/numeric"
)

// loadSourceDocs keeps the file each document came from: a directive's path resolves against
// it, and an error has to name it.
func loadSourceDocs(files []string) ([]sourceDoc, error) {
	var all []sourceDoc
	for _, path := range files {
		docs, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, docs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no process definitions found in provided files")
	}
	return all, nil
}

// readFile parses one source file. The parsed position index travels with each document so a
// failure the server reports by slot address can be printed as a line in this file.
func readFile(path string) ([]sourceDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		// No index: JSON allows tabs where YAML does not, so it keeps its own decode and
		// gives up line numbers. A .json definition is generated far more often than written.
		var doc any
		if err := numeric.Decode(data, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		if arr, ok := doc.([]any); ok {
			out := make([]sourceDoc, len(arr))
			for i, d := range arr {
				out[i] = sourceDoc{Value: d, File: path}
			}
			return out, nil
		}
		return []sourceDoc{{Value: doc, File: path}}, nil
	}

	parsed, err := defdoc.ParseAll(data)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	docs := make([]sourceDoc, len(parsed))
	for i, d := range parsed {
		docs[i] = sourceDoc{Value: d.Value, File: path, Index: d}
	}
	return docs, nil
}
