package providerspec

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidate_NamesAndCommandsAreRequired(t *testing.T) {
	for name, bad := range map[string][]Spec{
		"missing name":    {{Command: []string{"x"}}},
		"missing command": {{Name: "p"}},
		"duplicate name":  {{Name: "p", Command: []string{"x"}}, {Name: "p", Command: []string{"y"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(bad); err == nil {
				t.Fatal("an ill-formed provider config must be a validation error")
			}
		})
	}
	if err := Validate([]Spec{{Name: "p", Command: []string{"x", "--flag"}, ExpectedVersion: "1.0.0"}}); err != nil {
		t.Fatalf("a well-formed provider config must validate, got %v", err)
	}
}

// An entry without a command is valid only when the binary carries a
// provider of that name; anything else is a config error that names the
// built-ins, not a silent skip at snapshot time.
func TestValidate_NoCommandNamesABuiltIn(t *testing.T) {
	if err := Validate([]Spec{{Name: "rubydex"}}); err != nil {
		t.Fatalf("a built-in needs no command: %v", err)
	}
	err := Validate([]Spec{{Name: "nope"}})
	if err == nil || !strings.Contains(err.Error(), "built-ins: rubydex") {
		t.Fatalf("an unknown name without a command must be refused and the built-ins named, got %v", err)
	}
}

func TestValidate_PerFileNeedsExtensionsAndACommand(t *testing.T) {
	cases := []struct {
		name string
		p    Spec
		want string
	}{
		{"unknown files value", Spec{Name: "x", Command: []string{"x"}, Files: "sometimes"}, "files must be"},
		{"per-file without extensions", Spec{Name: "x", Command: []string{"x"}, Files: FilesPerFile}, "needs the extensions"},
		{"extensions without per-file", Spec{Name: "x", Command: []string{"x"}, Extensions: []string{".rb"}}, "only mean something"},
		{"built-in with files", Spec{Name: "rubydex", Files: FilesPerFile, Extensions: []string{".rb"}}, "decides its own caching"},
	}
	for _, c := range cases {
		err := Validate([]Spec{c.p})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", c.name, err, c.want)
		}
	}
	if err := Validate([]Spec{{Name: "x", Command: []string{"x"}, Files: FilesPerFile, Extensions: []string{".rb"}}}); err != nil {
		t.Errorf("a per-file provider with extensions is valid, got %v", err)
	}
}

// The list handed out is a copy: a caller sorting or appending to it must not
// change what the next validation accepts.
func TestBuiltInNamesIsACopy(t *testing.T) {
	names := BuiltInNames()
	if len(names) == 0 {
		t.Fatal("the binary carries at least one built-in provider")
	}
	want := append([]string(nil), names...)
	names[0] = "tampered"
	if got := BuiltInNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("BuiltInNames after a caller wrote to its result = %v, want %v", got, want)
	}
}
