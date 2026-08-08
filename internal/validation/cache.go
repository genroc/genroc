package validation

import (
	"sync"

	"genroc/internal/model"
)

// SchemaCache memoises Generate per (process, version) for readers that need a stored
// definition's inferred schemas — redaction, chiefly. A definition is immutable per
// version, so an entry never goes stale and nothing invalidates.
//
// Hold one per owner; never a package-level instance. The zero value is ready to use,
// and dropping the owner drops the cache with it.
type SchemaCache struct {
	m sync.Map // schemaKey → SchemaFile
}

type schemaKey struct {
	name    string
	version int
}

// Get returns the inferred schemas for a stored definition, inferring on first use.
// load supplies the definition only on a miss. ok is false when the definition cannot
// be loaded or does not infer — callers treat that as "no schemas", never as fatal,
// since the caller is redacting rather than validating.
func (c *SchemaCache) Get(name string, version int, load func() (*model.ProcessDefinition, error)) (SchemaFile, bool) {
	key := schemaKey{name: name, version: version}
	if cached, ok := c.m.Load(key); ok {
		return cached.(SchemaFile), true
	}
	def, err := load()
	if err != nil {
		return SchemaFile{}, false
	}
	sf, err := Generate(def)
	if err != nil {
		return SchemaFile{}, false
	}
	c.m.Store(key, sf)
	return sf, true
}
