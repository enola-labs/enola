package rubyextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Stimulus bindings live in markup, not code: `data-controller="dropdown"`
// binds an element to app/javascript/controllers/dropdown_controller.js by
// naming convention, and `data-action="click->dropdown#toggle"` invokes a
// method on it. Neither is a call site any language parser sees, so the
// controller and its handlers read as unreached from every view that uses
// them. This pass records the binding as a fact WITHOUT pretending to resolve
// it: each declared controller identifier in an .html.erb file becomes one
// dependency-style fact at resolution_level "markup-declared", linked to the
// controller file only when the conventional path actually exists — otherwise
// the fact is name-only, a declared binding the graph could not ground. Never
// a guessed edge: an identifier that is not a plain Stimulus token (an ERB
// interpolation, a helper call) is skipped, and a missing controller file
// yields no target.
//
// The method a data-action names is carried on the same fact as its handler
// set. Grounding it on the symbol the controller file declares needs the whole
// store — the controller is TypeScript and this is the Ruby pass — so it is the
// stimulus-resolver binder's job; this pass only records what the markup says.
const stimulusResolutionLevel = "markup-declared"

// stimulusControllersDir is the conventional controller root the identifier
// maps into. It is probed first and wins outright; an app that relocates its
// controllers is resolved through stimulusControllerIndex instead.
const stimulusControllersDir = "app/javascript/controllers"

// stimulusHandlersProp carries the methods one view's data-action descriptors
// invoke on one controller — sorted and space-joined, the set-valued string
// form the rest of the vocabulary uses. The binder reads it back.
const stimulusHandlersProp = "stimulus_handlers"

var (
	// stimulusAttrRe captures a data-controller or data-action attribute value,
	// double- or single-quoted. Bounded on purpose: values are matched inside
	// one attribute literal, never across tags.
	stimulusAttrRe = regexp.MustCompile(`data-(controller|action)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	// stimulusIdentifierRe admits exactly the tokens Stimulus generates
	// identifiers from: lowercase segments joined by single dashes or
	// underscores, with `--` separating namespace segments. Anything else —
	// interpolated Ruby, a stray helper — fails closed.
	stimulusIdentifierRe = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:--[a-z0-9]+(?:[-_][a-z0-9]+)*)*$`)
	// stimulusMethodRe admits a JavaScript method name and nothing else, so a
	// descriptor this pass misread never names a handler.
	stimulusMethodRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// stimulusControllerIndex holds the repository's controller files by basename,
// so an identifier whose conventional path does not exist can still be grounded
// when exactly one file in the tree carries its relative path. One measured
// monolith's controllers live under app/components, registered from there by
// `require.context`, and every binding in that tree was name-only without this.
type stimulusControllerIndex struct {
	byBase map[string][]string
}

// newStimulusControllerIndex indexes the repository's controller files. It reads
// the extractor's own file list rather than the filesystem, so a file the walk
// excluded cannot ground a binding.
func newStimulusControllerIndex(files []string) *stimulusControllerIndex {
	idx := &stimulusControllerIndex{byBase: map[string][]string{}}
	for _, rel := range files {
		slashed := filepath.ToSlash(rel)
		if !strings.HasSuffix(slashed, "_controller.js") && !strings.HasSuffix(slashed, "_controller.ts") {
			continue
		}
		base := slashed[strings.LastIndexByte(slashed, '/')+1:]
		idx.byBase[base] = append(idx.byBase[base], slashed)
	}
	for base := range idx.byBase {
		sort.Strings(idx.byBase[base])
	}
	return idx
}

// extractStimulusBindings scans one .html.erb file for Stimulus markup
// bindings and emits one fact per declared controller identifier, sorted by
// identifier so the file's facts are a function of what it declares, not of
// attribute order.
func extractStimulusBindings(repoPath, relFile string, src []byte, controllers *stimulusControllerIndex) []facts.Fact {
	if !strings.HasSuffix(strings.ToLower(relFile), ".html.erb") {
		return nil
	}
	declaredBy := map[string]map[string]bool{}
	handlersOf := map[string]map[string]bool{}
	declare := func(identifier, attr string) {
		if !stimulusIdentifierRe.MatchString(identifier) {
			return
		}
		if declaredBy[identifier] == nil {
			declaredBy[identifier] = map[string]bool{}
		}
		declaredBy[identifier][attr] = true
	}
	for _, m := range stimulusAttrRe.FindAllStringSubmatch(string(src), -1) {
		attr, value := "data-"+m[1], m[2]+m[3]
		// A value carrying embedded Ruby renders to something this pass cannot
		// know; the whole attribute is skipped, not just the interpolated token,
		// because the static remainder may not survive the rendering either.
		if strings.Contains(value, "<%") {
			continue
		}
		for _, token := range strings.Fields(value) {
			switch attr {
			case "data-controller":
				declare(token, attr)
			case "data-action":
				identifier, method := stimulusAction(token)
				declare(identifier, attr)
				if method != "" && declaredBy[identifier] != nil {
					if handlersOf[identifier] == nil {
						handlersOf[identifier] = map[string]bool{}
					}
					handlersOf[identifier][method] = true
				}
			}
		}
	}

	identifiers := make([]string, 0, len(declaredBy))
	for identifier := range declaredBy {
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)

	var out []facts.Fact
	for _, identifier := range identifiers {
		attrs := make([]string, 0, len(declaredBy[identifier]))
		for attr := range declaredBy[identifier] {
			attrs = append(attrs, attr)
		}
		sort.Strings(attrs)
		fact := facts.Fact{
			Kind: facts.KindDependency,
			Name: fmt.Sprintf("stimulus-binding: %s -> %s", relFile, identifier),
			File: relFile,
			Props: map[string]any{
				"language":         "ruby",
				"framework":        "stimulus",
				"binding":          strings.Join(attrs, " "),
				"resolution_level": stimulusResolutionLevel,
			},
		}
		if handlers := sortedKeys(handlersOf[identifier]); len(handlers) > 0 {
			fact.SetProp(stimulusHandlersProp, strings.Join(handlers, " "))
		}
		if target := stimulusControllerFile(repoPath, identifier, controllers); target != "" {
			fact.Relations = []facts.Relation{{Kind: facts.RelDependsOn, Target: target}}
		}
		out = append(out, fact)
	}
	return out
}

// stimulusAction splits one data-action descriptor into the controller
// identifier and the method it invokes: `click->dropdown#toggle` is dropdown
// and toggle, the event prefix is optional (`dropdown#toggle` is legal
// Stimulus), and the action options a descriptor may carry (`#submit:prevent`)
// are not part of the method name. The identifier still passes through the
// identifier gate and the method through its own, so a descriptor this cannot
// parse declares nothing.
func stimulusAction(descriptor string) (identifier, method string) {
	rest := descriptor
	if idx := strings.LastIndex(rest, "->"); idx >= 0 {
		rest = rest[idx+2:]
	}
	identifier, method, found := strings.Cut(rest, "#")
	if !found {
		return "", ""
	}
	method, _, _ = strings.Cut(method, ":")
	if !stimulusMethodRe.MatchString(method) {
		method = ""
	}
	return identifier, method
}

// stimulusControllerFile maps an identifier to a controller file and returns it
// only when exactly one is found: `--` separates directory segments and dashes
// become underscores, so `users--date-picker` maps to
// users/date_picker_controller.(js|ts). The conventional root is probed first
// and wins outright — it is Stimulus' own default and a repository using it is
// not ambiguous with anything. Failing that, the file is the single one in the
// tree whose path ends with that relative path; two candidates are a genuine
// ambiguity and yield no target, which is what `dropdown` naming both a root
// controller and a nested one has to mean.
func stimulusControllerFile(repoPath, identifier string, controllers *stimulusControllerIndex) string {
	base := stimulusControllerPath(identifier)
	for _, ext := range []string{".js", ".ts"} {
		rel := filepath.ToSlash(filepath.Join(stimulusControllersDir, base+ext))
		if _, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(rel))); err == nil {
			return rel
		}
	}
	if controllers == nil {
		return ""
	}
	for _, ext := range []string{".js", ".ts"} {
		want := base + ext
		found := ""
		for _, file := range controllers.byBase[want[strings.LastIndexByte(want, '/')+1:]] {
			if file != want && !strings.HasSuffix(file, "/"+want) {
				continue
			}
			if found != "" && found != file {
				return ""
			}
			found = file
		}
		if found != "" {
			return found
		}
	}
	return ""
}

// stimulusControllerPath renders an identifier as the extension-less relative
// path Stimulus derives its file from.
func stimulusControllerPath(identifier string) string {
	segments := strings.Split(identifier, "--")
	for i, segment := range segments {
		segments[i] = strings.ReplaceAll(segment, "-", "_")
	}
	return strings.Join(segments, "/") + "_controller"
}
