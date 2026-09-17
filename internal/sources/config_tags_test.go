package sources

import (
	"reflect"
	"strings"
	"testing"
)

// The `.genroc` schema is reflected from these structs through their json tags, and the file is
// read through their yaml tags. A name spelled differently in the two makes the editor accept a
// key the reader ignores, or underline one it honours -- silently, since each half is right on
// its own terms.
func TestConfigTagsAgree(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(projectConfig{}), reflect.TypeOf(resolverConfig{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
			jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
			if yamlName != jsonName {
				t.Errorf("%s.%s: yaml %q but json %q -- the editor and the reader would disagree about the key",
					typ.Name(), f.Name, yamlName, jsonName)
			}
			if yamlName != "-" && f.Tag.Get("description") == "" {
				t.Errorf("%s.%s: no description, so the editor has nothing to show for %q", typ.Name(), f.Name, yamlName)
			}
		}
	}
}
