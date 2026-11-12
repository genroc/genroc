// Package extension names the keys an author may write wherever genroc's own structure is, for
// tools to keep their data in: decoders skip them, and only the stored document holds them.
// specs/extension-keys.md.
package extension

import "strings"

func Is(key string) bool { return strings.HasPrefix(key, "x-") }
