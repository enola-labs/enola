package intent

import (
	"reflect"
	"strings"
)

// DeclarationKeys are the top-level keys of an enola-intent.yaml, read off
// Declaration's own tags so the list cannot fall behind the schema. A reader of some
// other YAML file uses them to recognise a declaration handed to it by mistake.
func DeclarationKeys() []string {
	t := reflect.TypeOf(Declaration{})
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}
