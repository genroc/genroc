package sources

import (
	"reflect"
	"strings"
	"testing"
)

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
