package clientspec

import (
	"strings"
	"testing"
)

func ptr(i int) *int { return &i }

func validSpec() Spec {
	return Spec{
		Name:          "sdk-http",
		Language:      "typescript",
		ReceiverTypes: []string{"IHttpRequestService"},
		Methods: []Method{{
			Name:        "sendRequest",
			ServiceArg:  ptr(0),
			PathArg:     ptr(1),
			OptionsArg:  ptr(2),
			VerbOption:  "method",
			DefaultVerb: "GET",
		}},
	}
}

func TestValidate_AcceptsAValidSpec(t *testing.T) {
	specs := []Spec{validSpec()}
	Normalize(specs)
	if err := Validate(specs); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestValidate_RejectsEachProblem(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"missing name", func(s *Spec) { s.Name = "" }, "missing name"},
		{"unknown language", func(s *Spec) { s.Language = "cobol" }, `language "cobol" is not a language enola reads`},
		{"no receiver types", func(s *Spec) { s.ReceiverTypes = nil }, "receiver_types is empty"},
		{"receiver type not a name", func(s *Spec) { s.ReceiverTypes = []string{"a.b"} }, `receiver type "a.b" is not a type name`},
		{"no methods", func(s *Spec) { s.Methods = nil }, "methods is empty"},
		{"method not a name", func(s *Spec) { s.Methods[0].Name = "send-request" }, `name "send-request" is not a method name`},
		{"path_arg missing", func(s *Spec) { s.Methods[0].PathArg = nil }, "path_arg is required"},
		{"negative index", func(s *Spec) { s.Methods[0].ServiceArg = ptr(-1) }, "service_arg must be 0 or greater"},
		{"shared position", func(s *Spec) { s.Methods[0].ServiceArg = ptr(1) }, "path_arg and service_arg both name argument 1"},
		{"verb option without options", func(s *Spec) { s.Methods[0].OptionsArg = nil }, "verb_option only means something with options_arg"},
		{"bad default verb", func(s *Spec) { s.Methods[0].DefaultVerb = "FETCH" }, `default_verb "FETCH" is not an HTTP verb`},
		{"method declared twice", func(s *Spec) { s.Methods = append(s.Methods, s.Methods[0]) }, `method "sendRequest" is declared twice`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSpec()
			c.mutate(&s)
			err := Validate([]Spec{s})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestValidate_ReportsEveryProblemAtOnce(t *testing.T) {
	s := validSpec()
	s.Language = "cobol"
	s.Methods[0].PathArg = nil
	s.Methods[0].DefaultVerb = "FETCH"
	err := Validate([]Spec{s})
	if err == nil {
		t.Fatal("invalid spec accepted")
	}
	for _, want := range []string{"is not a language enola reads", "path_arg is required", "is not an HTTP verb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %q:\n%v", want, err)
		}
	}
}

func TestValidate_RejectsDuplicateNamesAndOverlappingCalls(t *testing.T) {
	a, b := validSpec(), validSpec()
	if err := Validate([]Spec{a, b}); err == nil || !strings.Contains(err.Error(), `name "sdk-http" is declared twice`) {
		t.Errorf("duplicate spec name accepted: %v", err)
	}

	b.Name = "other"
	err := Validate([]Spec{a, b})
	if err == nil || !strings.Contains(err.Error(), `IHttpRequestService.sendRequest is already declared by clients "sdk-http"`) {
		t.Errorf("two specs reading the same call accepted: %v", err)
	}

	b.ReceiverTypes = []string{"OtherClient"}
	if err := Validate([]Spec{a, b}); err != nil {
		t.Errorf("same method name on a different type rejected: %v", err)
	}
}

func TestNormalize_FillsDefaultsAndIsIdempotent(t *testing.T) {
	s := validSpec()
	s.Language = " TypeScript "
	s.Methods[0].VerbOption = ""
	s.Methods[0].DefaultVerb = "post"
	specs := []Spec{s}

	Normalize(specs)
	first := Fingerprint(specs)
	Normalize(specs)

	m := specs[0].Methods[0]
	if specs[0].Language != "typescript" || m.VerbOption != DefaultVerbOption || m.DefaultVerb != "POST" {
		t.Errorf("defaults not filled: language=%q verb_option=%q default_verb=%q", specs[0].Language, m.VerbOption, m.DefaultVerb)
	}
	if Fingerprint(specs) != first {
		t.Error("Normalize is not idempotent")
	}
}

func TestNormalize_LeavesVerbOptionEmptyWithoutOptions(t *testing.T) {
	s := validSpec()
	s.Methods[0].OptionsArg = nil
	s.Methods[0].VerbOption = ""
	specs := []Spec{s}
	Normalize(specs)
	if specs[0].Methods[0].VerbOption != "" {
		t.Errorf("verb_option defaulted with no options_arg: %q", specs[0].Methods[0].VerbOption)
	}
}

func TestFingerprint_EmptyForNoSpecs(t *testing.T) {
	if got := Fingerprint(nil); got != "" {
		t.Errorf("no specs must fingerprint to \"\", got %q", got)
	}
	if got := AliasFingerprint(nil); got != "" {
		t.Errorf("no aliases must fingerprint to \"\", got %q", got)
	}
}

func TestFingerprint_IgnoresOrderAndSeesEveryField(t *testing.T) {
	a := validSpec()
	b := validSpec()
	b.Name = "b"
	b.ReceiverTypes = []string{"Z", "Y"}
	base := Fingerprint([]Spec{a, b})

	reordered := b
	reordered.ReceiverTypes = []string{"Y", "Z"}
	if Fingerprint([]Spec{reordered, a}) != base {
		t.Error("fingerprint depends on declaration order")
	}

	mutations := map[string]func(*Spec){
		"name":          func(s *Spec) { s.Name = "renamed" },
		"language":      func(s *Spec) { s.Language = "go" },
		"receiver type": func(s *Spec) { s.ReceiverTypes = []string{"Other"} },
		"method name":   func(s *Spec) { s.Methods[0].Name = "call" },
		"path_arg":      func(s *Spec) { s.Methods[0].PathArg = ptr(3) },
		"service_arg":   func(s *Spec) { s.Methods[0].ServiceArg = nil },
		"options_arg":   func(s *Spec) { s.Methods[0].OptionsArg = ptr(4) },
		"verb_option":   func(s *Spec) { s.Methods[0].VerbOption = "verb" },
		"default_verb":  func(s *Spec) { s.Methods[0].DefaultVerb = "POST" },
	}
	for field, mutate := range mutations {
		c := validSpec()
		c.Methods = append([]Method(nil), c.Methods...)
		mutate(&c)
		if Fingerprint([]Spec{c, b}) == base {
			t.Errorf("fingerprint ignores %s", field)
		}
	}
}

func TestValidateAliases(t *testing.T) {
	if err := ValidateAliases(map[string]string{"resource-api": "gateway"}); err != nil {
		t.Errorf("valid aliases rejected: %v", err)
	}
	err := ValidateAliases(map[string]string{"": "gateway", "resource-api": " "})
	if err == nil || !strings.Contains(err.Error(), "empty service name") || !strings.Contains(err.Error(), "empty repository label") {
		t.Errorf("want both problems reported, got %v", err)
	}
}

// A language enola reads without a client reader loads, is skipped, and is named in a
// notice that points to an issue, a pull request and professional services. It does not
// reach an extractor or the fingerprint.
func TestUnsupportedLanguage_LoadsSkipsAndIsReported(t *testing.T) {
	ts, py, py2 := validSpec(), validSpec(), validSpec()
	py.Name, py.Language, py.ReceiverTypes = "py-http", "Python", []string{"HttpClient"}
	py2.Name, py2.Language, py2.ReceiverTypes = "py-other", "python", []string{"OtherClient"}
	specs := []Spec{ts, py, py2}
	Normalize(specs)

	if err := Validate(specs); err != nil {
		t.Fatalf("a spec for a language without a reader must load: %v", err)
	}
	if got := Unsupported(specs); len(got) != 2 || got[0].Name != "py-http" || got[1].Name != "py-other" {
		t.Errorf("Unsupported = %+v, want both python specs", got)
	}
	if got := ForLanguage(specs, "typescript"); len(got) != 1 {
		t.Errorf("ForLanguage(typescript) = %+v, want only the typescript spec", got)
	}
	if Fingerprint(specs) != Fingerprint([]Spec{ts}) {
		t.Error("a spec nothing reads changed the fingerprint")
	}

	notice := UnsupportedNotice(specs)
	for _, want := range []string{
		"not implemented for python yet",
		`clients "py-http", "py-other" were skipped`,
		"client readers exist for: typescript",
		IssuesURL + "?title=Custom+clients+for+python",
		ContributeURL,
		EnterpriseURL,
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice omits %q:\n%s", want, notice)
		}
	}
	if UnsupportedNotice([]Spec{ts}) != "" {
		t.Error("a config whose specs are all read must produce no notice")
	}
}

func TestNormalize_ResolvesLanguageAliases(t *testing.T) {
	for alias, want := range map[string]string{"JavaScript": "typescript", "ts": "typescript", "golang": "go", "C#": "dotnet"} {
		s := validSpec()
		s.Language = alias
		specs := []Spec{s}
		Normalize(specs)
		if specs[0].Language != want {
			t.Errorf("language %q normalized to %q, want %q", alias, specs[0].Language, want)
		}
	}
}

func TestForLanguage_KeepsOrderAndFilters(t *testing.T) {
	a, b, c := validSpec(), validSpec(), validSpec()
	a.Name, b.Name, c.Name = "a", "b", "c"
	b.Language = "go"
	got := ForLanguage([]Spec{c, b, a}, "typescript")
	if len(got) != 2 || got[0].Name != "c" || got[1].Name != "a" {
		t.Errorf("want [c a], got %+v", got)
	}
}
