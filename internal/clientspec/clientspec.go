// Package clientspec is the user-declared vocabulary of in-house HTTP clients: which
// method on which injected type performs a request, and where its service name, path
// and verb sit among the call's arguments.
//
// A spec teaches an extractor to READ a call site. It never declares an edge. A call
// read through a spec becomes a client route like any other, and the cross-repo linker
// still draws an edge only when a loaded repository serves that path. A wrong spec can
// add a call that matches nothing, which coverage shows; it cannot assert a dependency.
// That is what keeps docs/EXTENDING.md's rule intact: config may not express a
// matching rule.
package clientspec

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Spec is one in-house client: the injected types that carry it, and the methods on
// them that make a request.
type Spec struct {
	// Name identifies the spec. It is stamped on every fact the spec produces, so a
	// reader can tell a route read through user config from one enola knows natively.
	Name string `yaml:"name"`

	// Language is the name of the extractor that reads the spec.
	Language string `yaml:"language"`

	// ReceiverTypes are the declared types of the member a call is made on. The type
	// decides, never the method name: a sendRequest on an unrelated type is not HTTP.
	ReceiverTypes []string `yaml:"receiver_types"`

	Methods []Method `yaml:"methods"`
}

// Method is one request-making method and the positions of its arguments.
//
// Indices are pointers because 0 is a real position: absent and "the first argument"
// must stay distinguishable.
type Method struct {
	Name string `yaml:"name"`

	// PathArg is the argument holding the request path. Required.
	PathArg *int `yaml:"path_arg"`

	// ServiceArg is the argument naming the target service, when the client addresses
	// services by name rather than by host.
	ServiceArg *int `yaml:"service_arg,omitempty"`

	// OptionsArg is the object-literal argument the verb is read from.
	OptionsArg *int `yaml:"options_arg,omitempty"`

	// VerbOption is the property inside OptionsArg holding the verb. Normalize sets it
	// to "method" when OptionsArg is given without it.
	VerbOption string `yaml:"verb_option,omitempty"`

	// DefaultVerb is used when no verb can be read from the call.
	DefaultVerb string `yaml:"default_verb,omitempty"`
}

// Consumer is an extractor that reads call sites through specs. The engine hands it
// the specs for its language when it is registered, so an extractor never reads the
// config itself.
type Consumer interface {
	SetClientSpecs(specs []Spec)
}

// Languages are the extractor names that read specs.
var Languages = map[string]bool{
	"typescript": true,
}

// WithoutReader are the extractors enola ships that could make HTTP calls but have no
// client reader yet. A spec naming one loads and is skipped: nothing hands it to an
// extractor and Fingerprint leaves it out. Loading and doing nothing is the silent
// failure validation exists to refuse, so every entry point that loads a config
// reports it through UnsupportedNotice. Any other language is a typo and fails
// validation. TestClientLanguagesAreRegisteredExtractors in pkg/bootstrap keeps both
// maps in step with the registered extractors.
var WithoutReader = map[string]bool{
	"cpp": true, "dart": true, "dotnet": true, "go": true, "java": true, "kotlin": true,
	"php": true, "python": true, "ruby": true, "rust": true, "scala": true, "swift": true,
}

// languageAliases are spellings people write for an extractor name. The typescript
// extractor reads JavaScript too, so "javascript" is typescript and not a typo.
var languageAliases = map[string]string{
	"ts": "typescript", "tsx": "typescript", "js": "typescript", "jsx": "typescript",
	"javascript": "typescript", "node": "typescript",
	"golang": "go", "c#": "dotnet", "csharp": "dotnet", "c++": "cpp",
}

// Issue, contribution and professional-services links the unsupported-language notice
// points to.
const (
	IssuesURL     = "https://github.com/enola-labs/enola/issues/new"
	ContributeURL = "https://github.com/enola-labs/enola/blob/main/docs/EXTENDING.md#teaching-enola-your-http-client"
	EnterpriseURL = "https://enola.tech/enterprise"
)

// DefaultVerbOption is the options property a verb is read from when a method names
// an options argument but no property.
const DefaultVerbOption = "method"

var verbs = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

var identifier = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)

// Normalize fills the defaults a spec may leave out. It is idempotent.
func Normalize(specs []Spec) {
	for i := range specs {
		s := &specs[i]
		s.Name = strings.TrimSpace(s.Name)
		s.Language = strings.ToLower(strings.TrimSpace(s.Language))
		if canonical, ok := languageAliases[s.Language]; ok {
			s.Language = canonical
		}
		for j := range s.ReceiverTypes {
			s.ReceiverTypes[j] = strings.TrimSpace(s.ReceiverTypes[j])
		}
		for j := range s.Methods {
			m := &s.Methods[j]
			m.Name = strings.TrimSpace(m.Name)
			m.VerbOption = strings.TrimSpace(m.VerbOption)
			m.DefaultVerb = strings.ToUpper(strings.TrimSpace(m.DefaultVerb))
			if m.OptionsArg != nil && m.VerbOption == "" {
				m.VerbOption = DefaultVerbOption
			}
		}
	}
}

// Validate reports every problem in the specs at once, so fixing one does not merely
// reveal the next.
func Validate(specs []Spec) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	names := map[string]bool{}
	// claimed maps language, receiver type and method to the spec declaring it. Two
	// specs reading the same call would each emit a route for it.
	claimed := map[string]string{}
	for i, s := range specs {
		label := fmt.Sprintf("clients[%d]", i)
		if s.Name != "" {
			label += " (" + s.Name + ")"
		}
		switch {
		case s.Name == "":
			fail("%s: missing name", label)
		case names[s.Name]:
			fail("%s: name %q is declared twice", label, s.Name)
		}
		names[s.Name] = true

		if !Languages[s.Language] && !WithoutReader[s.Language] {
			fail("%s: language %q is not a language enola reads (client readers: %s)", label, s.Language, strings.Join(sortedKeys(Languages), ", "))
		}
		if len(s.ReceiverTypes) == 0 {
			fail("%s: receiver_types is empty; a spec is keyed on the receiver's declared type", label)
		}
		for _, t := range s.ReceiverTypes {
			if !identifier.MatchString(t) {
				fail("%s: receiver type %q is not a type name", label, t)
			}
		}
		if len(s.Methods) == 0 {
			fail("%s: methods is empty", label)
		}

		methods := map[string]bool{}
		for j, m := range s.Methods {
			ml := fmt.Sprintf("%s.methods[%d]", label, j)
			switch {
			case !identifier.MatchString(m.Name):
				fail("%s: name %q is not a method name", ml, m.Name)
			case methods[m.Name]:
				fail("%s: method %q is declared twice", ml, m.Name)
			}
			methods[m.Name] = true

			if m.PathArg == nil {
				fail("%s: path_arg is required", ml)
			}
			positions := map[int]string{}
			for _, a := range []struct {
				field string
				index *int
			}{{"path_arg", m.PathArg}, {"service_arg", m.ServiceArg}, {"options_arg", m.OptionsArg}} {
				if a.index == nil {
					continue
				}
				if *a.index < 0 {
					fail("%s: %s must be 0 or greater, got %d", ml, a.field, *a.index)
					continue
				}
				if other, taken := positions[*a.index]; taken {
					fail("%s: %s and %s both name argument %d", ml, other, a.field, *a.index)
				}
				positions[*a.index] = a.field
			}

			if m.VerbOption != "" && m.OptionsArg == nil {
				fail("%s: verb_option only means something with options_arg", ml)
			}
			if m.VerbOption != "" && !identifier.MatchString(m.VerbOption) {
				fail("%s: verb_option %q is not a property name", ml, m.VerbOption)
			}
			if m.DefaultVerb != "" && !verbs[m.DefaultVerb] {
				fail("%s: default_verb %q is not an HTTP verb (allowed: %s)", ml, m.DefaultVerb, strings.Join(sortedKeys(verbs), ", "))
			}

			if s.Name == "" || m.Name == "" {
				continue
			}
			for _, t := range s.ReceiverTypes {
				key := s.Language + "\x00" + t + "\x00" + m.Name
				if other, ok := claimed[key]; ok && other != s.Name {
					fail("%s: %s.%s is already declared by clients %q", ml, t, m.Name, other)
				}
				claimed[key] = s.Name
			}
		}
	}
	return errors.Join(errs...)
}

// ValidateAliases reports every empty service name or repository label at once.
func ValidateAliases(aliases map[string]string) error {
	var errs []error
	for _, service := range sortedKeys(aliases) {
		if strings.TrimSpace(service) == "" {
			errs = append(errs, errors.New("service_aliases: empty service name"))
			continue
		}
		if strings.TrimSpace(aliases[service]) == "" {
			errs = append(errs, fmt.Errorf("service_aliases: %q maps to an empty repository label", service))
		}
	}
	return errors.Join(errs...)
}

// ForLanguage returns the specs a named extractor reads, in declaration order.
func ForLanguage(specs []Spec, language string) []Spec {
	var out []Spec
	for _, s := range specs {
		if s.Language == language {
			out = append(out, s)
		}
	}
	return out
}

// Unsupported returns the specs naming a language that has no client reader, in
// declaration order.
func Unsupported(specs []Spec) []Spec {
	var out []Spec
	for _, s := range specs {
		if WithoutReader[s.Language] {
			out = append(out, s)
		}
	}
	return out
}

// UnsupportedNotice tells the user which declared clients were skipped because their
// language has no client reader, and where to go for one. One paragraph per language,
// "" when every spec is read.
func UnsupportedNotice(specs []Spec) string {
	byLanguage := map[string][]string{}
	for _, s := range Unsupported(specs) {
		byLanguage[s.Language] = append(byLanguage[s.Language], s.Name)
	}
	var sb strings.Builder
	for _, language := range sortedKeys(byLanguage) {
		names := byLanguage[language]
		fmt.Fprintf(&sb, "Custom clients are not implemented for %s yet, so %s %s skipped (client readers exist for: %s).\n",
			language, quotedList(names), pluralVerb(len(names)), strings.Join(sortedKeys(Languages), ", "))
		fmt.Fprintf(&sb, "If you need it: open an issue at %s?title=%s, send a pull request (see %s), "+
			"or contact us at %s for professional services.\n",
			IssuesURL, url.QueryEscape("Custom clients for "+language), ContributeURL, EnterpriseURL)
	}
	return sb.String()
}

func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return "clients " + strings.Join(quoted, ", ")
}

func pluralVerb(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// Fingerprint renders specs as a stable string, independent of declaration order. It
// is "" for no specs, so folding it into a hash or a cache key changes nothing for a
// configuration that declares none. Specs nothing reads are left out, so declaring one
// does not change a snapshot it cannot affect.
func Fingerprint(specs []Spec) string {
	var read []Spec
	for _, s := range specs {
		if !WithoutReader[s.Language] {
			read = append(read, s)
		}
	}
	if len(read) == 0 {
		return ""
	}
	sorted := read
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var sb strings.Builder
	for _, s := range sorted {
		types := append([]string(nil), s.ReceiverTypes...)
		sort.Strings(types)
		fmt.Fprintf(&sb, "spec:%s\x00%s\x00%s\n", s.Name, s.Language, strings.Join(types, ","))

		methods := append([]Method(nil), s.Methods...)
		sort.SliceStable(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
		for _, m := range methods {
			fmt.Fprintf(&sb, "method:%s\x00%s\x00%s\x00%s\x00%s\x00%s\n",
				m.Name, index(m.PathArg), index(m.ServiceArg), index(m.OptionsArg), m.VerbOption, m.DefaultVerb)
		}
	}
	return sb.String()
}

// AliasFingerprint renders aliases as a stable string, "" for none.
func AliasFingerprint(aliases map[string]string) string {
	var sb strings.Builder
	for _, service := range sortedKeys(aliases) {
		sb.WriteString(service + "\x00" + aliases[service] + "\n")
	}
	return sb.String()
}

func index(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
