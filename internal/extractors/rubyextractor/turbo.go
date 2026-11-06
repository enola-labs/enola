package rubyextractor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// Turbo's frame identity lives in markup and model macros, not in any call an
// AST resolver can follow: a `turbo_frame_tag "comments"` declares a frame id,
// so does a hand-written `<turbo-frame id="comments">` element (the rendered
// form of the same identity, and the shape a helper-free view writes),
// a `data-turbo-frame="comments"` attribute targets it, and a model's
// `broadcasts_to :posts` names the stream its updates ride — finding 0007's
// remaining Hotwire surface. This pass records those identities as named
// facts the same way the Stimulus pass records controller bindings: declared,
// never resolved, and fail-closed on anything that is not a literal. A frame
// id built from `dom_id(@post)`, an interpolated string, an attribute
// carrying ERB — all emit nothing, because the rendered identity is not
// knowable statically. `_top` is Turbo's reserved everything-target, outside
// the literal-id shape on purpose: targeting the whole page declares no frame.

var (
	// turboFrameTagRe captures the first argument of a turbo_frame_tag helper
	// call when it is a literal symbol or a plain quoted string. Bounded on
	// purpose: one argument, no expression forms.
	turboFrameTagRe = regexp.MustCompile(`turbo_frame_tag\s*\(?\s*(?::([a-z0-9_]+)|"([^"]*)"|'([^']*)')`)
	// turboFrameAttrRe captures a data-turbo-frame attribute value, double- or
	// single-quoted, inside one attribute literal.
	turboFrameAttrRe = regexp.MustCompile(`data-turbo-frame\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	// turboFrameElementRe captures the id attribute of a hand-written
	// <turbo-frame> element — the form Turbo itself renders, and the one a view
	// that skips the tag helper writes directly. Bounded to one tag (no > may
	// intervene before id), and an id carrying ERB still fails the id gate.
	turboFrameElementRe = regexp.MustCompile(`<turbo-frame\b[^>]*?\bid\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	// turboFrameIDRe admits the ids Rails' dom_id convention and hand-written
	// frames produce: lowercase alphanumerics joined by underscores or dashes,
	// starting with an alphanumeric — which also keeps `_top` out.
	turboFrameIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// extractTurboFrames scans one view template for literal Turbo frame ids and
// emits one fact per declared id, sorted by id so the file's facts are a
// function of what it declares, not of markup order.
func extractTurboFrames(relFile string, src []byte) []facts.Fact {
	declaredBy := map[string]map[string]bool{}
	declare := func(id, via string) {
		if !turboFrameIDRe.MatchString(id) {
			return
		}
		if declaredBy[id] == nil {
			declaredBy[id] = map[string]bool{}
		}
		declaredBy[id][via] = true
	}
	for _, m := range turboFrameTagRe.FindAllStringSubmatch(string(src), -1) {
		declare(m[1]+m[2]+m[3], "turbo_frame_tag")
	}
	for _, m := range turboFrameAttrRe.FindAllStringSubmatch(string(src), -1) {
		declare(m[1]+m[2], "data-turbo-frame")
	}
	for _, m := range turboFrameElementRe.FindAllStringSubmatch(string(src), -1) {
		declare(m[1]+m[2], "<turbo-frame>")
	}

	ids := make([]string, 0, len(declaredBy))
	for id := range declaredBy {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var out []facts.Fact
	for _, id := range ids {
		vias := make([]string, 0, len(declaredBy[id]))
		for via := range declaredBy[id] {
			vias = append(vias, via)
		}
		sort.Strings(vias)
		out = append(out, facts.Fact{
			Kind: facts.KindDependency,
			Name: fmt.Sprintf("turbo-frame: %s -> %s", relFile, id),
			File: relFile,
			Props: map[string]any{
				"language":         "ruby",
				"framework":        "turbo",
				"binding":          strings.Join(vias, " "),
				"resolution_level": stimulusResolutionLevel,
			},
		})
	}
	return out
}

// extractBroadcasts scans the model files for broadcasts_to declarations whose
// stream argument is a literal symbol or plain string, and emits one fact per
// (model, stream) pair — "broadcast: <Model> -> <stream>". The common lambda
// form (`broadcasts_to ->(post) { :posts }`) computes its stream at runtime
// per record, so it emits NOTHING: a stream identity this pass cannot read is
// a counted absence, never a guess. Facts are sorted by name then file, so
// the output is a function of what the models declare.
func extractBroadcasts(repoPath string, files []string) []facts.Fact {
	var out []facts.Fact
	for _, relFile := range files {
		if !isModelFile(relFile) {
			continue
		}
		model := modelClassName(relFile)
		if model == "" {
			continue
		}
		eachCall(filepath.Join(repoPath, relFile), func(method string, args *sitter.Node, src []byte) {
			if method != "broadcasts_to" {
				return
			}
			stream, line := literalStreamArg(args, src)
			if stream == "" {
				return
			}
			out = append(out, facts.Fact{
				Kind: facts.KindDependency,
				Name: fmt.Sprintf("broadcast: %s -> %s", model, stream),
				File: relFile,
				Line: line,
				Props: map[string]any{
					"language":         "ruby",
					"framework":        "rails",
					"macro":            "broadcasts_to",
					"model":            model,
					"resolution_level": "literal-declared",
				},
			})
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].File < out[j].File
	})
	return out
}

// literalStreamArg reads the FIRST argument of a broadcasts_to call, and only
// when it is a literal the stream identity can be read from: a simple symbol,
// or a string whose sole content is one uninterpolated literal. Any other
// first argument — a lambda, a method call, an interpolation — returns
// nothing, which is the fail-closed verdict for the whole declaration.
func literalStreamArg(args *sitter.Node, src []byte) (string, int) {
	if args == nil {
		return "", 0
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		child := args.Child(i)
		switch child.Kind() {
		case "(", ")", ",", "comment":
			continue
		case "simple_symbol":
			return strings.TrimPrefix(rubyText(child, src), ":"), int(child.StartPosition().Row) + 1
		case "string":
			if child.ChildCount() == 3 && child.Child(1).Kind() == "string_content" {
				return rubyText(child.Child(1), src), int(child.StartPosition().Row) + 1
			}
			return "", 0
		default:
			return "", 0
		}
	}
	return "", 0
}
