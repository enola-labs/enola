package rubyextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// How a target model's name was arrived at. These are different confidences and
// a consumer deserves to tell them apart: `class_name: "User"` is a statement in
// the source, an inferred name is a convention holding, and a source: is a hop.
const (
	targetDeclared = "declared"
	targetDerived  = "derived"
	targetThrough  = "through_source"
)

// modelAssociation is one declared relationship, with the target it names and
// how that name was arrived at. An empty target means the declaration was read
// and its target could not be named — polymorphic, or a through: whose source
// nothing states — which is a counted miss rather than an edge.
type modelAssociation struct {
	name        string
	macro       string
	target      string
	via         string
	through     string
	source      string
	polymorphic bool
}

// modelIndex is every ActiveRecord model this repository declares, keyed by its
// lowercased class name. It is what turns a derived target from an inference
// into a checked one: `has_many :candidates` naming `Candidate` is a convention
// holding, and the way to know whether it held is to find the class.
type modelIndex struct {
	byName map[string]string
	// parents maps a model to the model it descends from, for the STI chains.
	parents map[string]string
}

func buildModelIndex(repoPath string, files []string) *modelIndex {
	index := &modelIndex{byName: map[string]string{}, parents: map[string]string{}}
	superclasses := map[string]string{}
	for _, relFile := range files {
		if !isModelFile(relFile) {
			continue
		}
		name := modelClassName(relFile)
		if name == "" {
			continue
		}
		superclass, declared := declaredSuperclass(filepath.Join(repoPath, relFile), name)
		if !declared {
			continue
		}
		if isARBaseClass(strings.TrimPrefix(superclass, "::")) {
			index.byName[strings.ToLower(name)] = name
			continue
		}
		superclasses[name] = superclass
	}

	// Single-table inheritance: a class descending from a model is a model. The
	// chain is followed by repeated passes rather than recursion, because the
	// declaration order in a file listing says nothing about the hierarchy.
	for changed := true; changed; {
		changed = false
		for name, superclass := range superclasses {
			parent := index.resolve(superclass, enclosingScope(name))
			if parent == "" {
				continue
			}
			index.byName[strings.ToLower(name)] = name
			index.parents[name] = parent
			delete(superclasses, name)
			changed = true
		}
	}
	return index
}

// resolve returns the model a name refers to from inside a namespace, following
// Ruby's constant lookup: innermost enclosing module first, then the top level.
// A leading :: forces the top level and is not part of the name.
func (m *modelIndex) resolve(name, within string) string {
	if name == "" {
		return ""
	}
	if forced, absolute := strings.CutPrefix(name, "::"); absolute {
		return m.byName[strings.ToLower(forced)]
	}
	for scope := within; scope != ""; scope = enclosingScope(scope) {
		if found, ok := m.byName[strings.ToLower(scope+"::"+name)]; ok {
			return found
		}
	}
	return m.byName[strings.ToLower(name)]
}

func enclosingScope(name string) string {
	if idx := strings.LastIndex(name, "::"); idx >= 0 {
		return name[:idx]
	}
	return ""
}

// declaredSuperclass returns what a model file says its class descends from.
// The class may be written short inside a module or fully qualified at the top
// level, and both spellings name the same class.
//
// A concern is a module rather than a class: it may declare associations, but
// they belong to whatever includes it, and emitting them against the concern's
// own name produces edges the runtime has no counterpart for.
func declaredSuperclass(path, name string) (string, bool) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	short := name
	if idx := strings.LastIndex(short, "::"); idx >= 0 {
		short = short[idx+2:]
	}
	for _, match := range classDeclaration.FindAllStringSubmatch(string(src), -1) {
		if match[1] == short || match[1] == name {
			return match[2], true
		}
	}
	return "", false
}

var classDeclaration = regexp.MustCompile(`(?m)^\s*class\s+([A-Za-z0-9_:]+)\s*<\s*([A-Za-z0-9_:]+)`)

// parseModelFile reads every association a model file declares. It is the
// promoted form of the reader the route resolver has been using: same macros,
// same class_name:/source: handling, now returning enough for both callers.
func parseModelFile(path string) []modelAssociation {
	var out []modelAssociation
	eachCallWithOptions(path, func(method string, args *sitter.Node, shared []*sitter.Node, src []byte) {
		if !associationMacros[method] {
			return
		}
		className := pairString(args, "class_name", src)
		// An enclosing `with_options class_name: …` names the class of every
		// association in its block that does not name its own. Without it the
		// class was derived from the association's name (BlockRelationship),
		// which no model has, and the association was dropped as unresolved.
		for i := 0; className == "" && i < len(shared); i++ {
			className = pairString(shared[i], "class_name", src)
		}
		through := pairSymbol(args, "through", src)
		polymorphic := pairBool(args, "polymorphic", src)
		source := pairSymbol(args, "source", src)
		sourceType := pairString(args, "source_type", src)

		for _, name := range positionalSymbols(args, src) {
			assoc := modelAssociation{
				name: name, macro: method, through: through, polymorphic: polymorphic,
			}
			switch {
			case polymorphic:
				// Points at whatever implements it, decided at runtime from a type
				// column. No single target exists to name.
			case className != "":
				assoc.target, assoc.via = className, targetDeclared
			case through != "" && sourceType != "":
				// `source_type:` states the class as literally as `class_name:` does.
				// The hop it would be reached through is polymorphic in every case
				// this appears — walking it finds a target that cannot be named and
				// refuses, discarding an answer the declaration already gave.
				assoc.target, assoc.via = sourceType, targetDeclared
			case through != "":
				// `source:` names an ASSOCIATION on the intermediate model, not a
				// class. Camelizing it produced targets like Owner and Section that
				// no model has — right often enough to look fine and wrong often
				// enough to matter. Following it properly needs the intermediate,
				// which the caller resolves; unresolved here means unresolved.
				assoc.via = targetThrough
				assoc.source = source
			default:
				base := name
				if toMany := relationshipMacros[method]; toMany || method == "has_and_belongs_to_many" {
					base = singularize(base)
				}
				assoc.target, assoc.via = camelizeClass(base), targetDerived
			}
			out = append(out, assoc)
		}
	})
	return out
}

// isModelFile reports whether a path holds Ruby model declarations. Concerns
// count: the associations written there are read against the concern's own
// name, which is where they are declared — attributing them to every includer
// is a second resolver this pass does not build.
func isModelFile(relFile string) bool {
	slash := filepath.ToSlash(relFile)
	if !strings.HasSuffix(slash, ".rb") {
		return false
	}
	return strings.HasPrefix(slash, "app/models/") ||
		strings.Contains(slash, "/app/models/")
}

// modelClassName turns app/models/custom_field/select.rb into
// CustomField::Select — the name the runtime reports and the name another
// association's class_name: would use.
func modelClassName(relFile string) string {
	slash := filepath.ToSlash(relFile)
	idx := strings.Index(slash, "app/models/")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSuffix(slash[idx+len("app/models/"):], ".rb")
	parts := strings.Split(rest, "/")
	for i, part := range parts {
		parts[i] = camelizeClass(part)
	}
	return strings.Join(parts, "::")
}

// extractAssociations emits one fact per association whose target can be named,
// and reports the rest as unresolved. The split is the decision: an edge names
// its target or it is not an edge.
func extractAssociations(repoPath string, files []string) ([]facts.Fact, map[string]int) {
	var out []facts.Fact
	unresolved := map[string]int{}
	index := buildModelIndex(repoPath, files)
	declared := map[string][]modelAssociation{}
	for _, relFile := range files {
		if !isModelFile(relFile) {
			continue
		}
		model := modelClassName(relFile)
		if model == "" || index.byName[strings.ToLower(model)] == "" {
			continue
		}
		declared[model] = parseModelFile(filepath.Join(repoPath, relFile))
	}

	// A concern's associations belong to every class that includes it. The
	// runtime attributes them that way, so an extractor scored against the
	// runtime must too: eleven concerns on the monolith declare 68 associations
	// and are included 330 times across app/models, and until now every one of
	// those was a counted miss.
	//
	// This is a resolver following an edge that already exists rather than a new
	// inference — `include` has been emitted as an `implements` relation
	// carrying `mixin_kind` all along, 7,396 of them for Ruby. The concern's own
	// file is still not treated as a model, so the associations are attributed
	// to includers and to nobody else, which is the correction the association
	// ADR's build notes recorded when reading a concern against its own name
	// scored as wrong.
	for model, includes := range includedModules(repoPath, files) {
		if index.byName[strings.ToLower(model)] == "" {
			continue
		}
		for _, module := range includes {
			for _, assoc := range concernAssociations(repoPath, files, module) {
				if findAssociation(declared[model], assoc.name) != nil {
					// The class declares it itself; its own declaration wins.
					continue
				}
				declared[model] = append(declared[model], assoc)
			}
		}
	}

	for model, associations := range declared {
		for _, assoc := range associations {
			target := index.resolve(assoc.target, model)
			reason := ""
			if assoc.via == targetThrough {
				target, reason = resolveThrough(index, declared, model, assoc)
			}
			if target == "" {
				if reason == "" {
					reason = unresolvedReason(assoc)
				}
				unresolved[reason]++
				continue
			}
			assoc.target = target
			props := map[string]any{
				"language":      "ruby",
				"framework":     "rails",
				"model":         model,
				"association":   assoc.name,
				"macro":         assoc.macro,
				"target":        assoc.target,
				"target_source": assoc.via,
			}
			if assoc.through != "" {
				props["through"] = assoc.through
			}
			out = append(out, facts.Fact{
				Kind:  facts.KindAssociation,
				Name:  model + "#" + assoc.name,
				File:  modelFileOf(model),
				Props: props,
			})
		}
	}
	// Publish the STI chains alongside, as extraction facts, so a consumer can
	// resolve an inherited association without every subclass restating it.
	for child, parent := range index.parents {
		out = append(out, facts.Fact{
			Kind: facts.KindAssociation,
			Name: child + "<" + parent,
			File: modelFileOf(child),
			Props: map[string]any{
				"language":      "ruby",
				"framework":     "rails",
				"model":         child,
				"macro":         "inherits",
				"target":        parent,
				"target_source": targetDeclared,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, unresolved
}

// resolveThrough takes the one extra hop a `through:` needs: find the
// intermediate association on this model, find the model it points at, and read
// the association `source:` names there. Anything that does not resolve to a
// known model resolves to nothing — the chain is walked once, never recursively.
func resolveThrough(index *modelIndex, declared map[string][]modelAssociation, model string, assoc modelAssociation) (string, string) {
	intermediate := inheritedAssociation(index, declared, model, assoc.through)
	if intermediate == nil {
		return "", reasonIntermediateUnresolved
	}
	via := index.resolve(intermediate.target, model)
	if via == "" {
		return "", reasonIntermediateUnresolved
	}
	final := sourceAssociation(index, declared, via, assoc)
	if final == nil {
		return "", "through_association"
	}
	target := index.resolve(final.target, via)
	if target == "" {
		return "", "through_association"
	}
	return target, ""
}

// sourceAssociation finds the association a `through:` reads its target from.
// An explicit `source:` names it outright. Without one Rails tries the
// association's own name and then its singular, which is what a `has_many
// :users, through: :user_access_levels` relies on when UserAccessLevel declares
// `belongs_to :user` — 77 of the monolith's 301 missing chains are that shape.
// The singular is only ever accepted when it matches an association the
// intermediate actually declares, so a wrong guess finds nothing and refuses
// rather than naming a class that does not exist.
func sourceAssociation(index *modelIndex, declared map[string][]modelAssociation, via string, assoc modelAssociation) *modelAssociation {
	if assoc.source != "" {
		return inheritedAssociation(index, declared, via, assoc.source)
	}
	if found := inheritedAssociation(index, declared, via, assoc.name); found != nil {
		return found
	}
	if singular := singularize(assoc.name); singular != assoc.name {
		return inheritedAssociation(index, declared, via, singular)
	}
	return nil
}

// inheritedAssociation finds an association on a model or on any model it
// descends from. Single-table inheritance means a subclass answers for every
// association its parent declares, and a `through:` chain routed via a subclass
// reads the association off whichever ancestor declared it — 89 of the
// monolith's unresolved chains failed for exactly this, reported as "the source
// association is absent" when it was present one class up.
func inheritedAssociation(index *modelIndex, declared map[string][]modelAssociation, model, name string) *modelAssociation {
	for depth := 0; depth < 8 && model != ""; depth++ {
		if found := findAssociation(declared[model], name); found != nil {
			return found
		}
		model = index.parents[model]
	}
	return nil
}

func findAssociation(associations []modelAssociation, name string) *modelAssociation {
	for i := range associations {
		if associations[i].name == name {
			return &associations[i]
		}
	}
	return nil
}

// reasonIntermediateUnresolved separates a chain that failed on its own terms
// from one that failed because the association it travels through never
// resolved. 165 of the monolith's 301 missing chains are the second kind, and
// reporting them as through-chain misses points every reader at the wrong file:
// no amount of `through:` logic reaches them, and fixing the intermediate fixes
// the chain for free.
const reasonIntermediateUnresolved = "through_intermediate_unresolved"

// unresolvedReason names why a declaration produced no edge, so the counter
// reports a task rather than a total.
func unresolvedReason(assoc modelAssociation) string {
	switch {
	case assoc.polymorphic:
		return "polymorphic_association"
	case assoc.via == targetThrough:
		return "through_association"
	}
	// A name that no model in this repository answers to. The convention did not
	// hold, and the honest report is that it did not.
	return "unknown_model"
}

// modelFileOf is the conventional path for a model class name.
func modelFileOf(model string) string {
	parts := strings.Split(model, "::")
	for i, part := range parts {
		parts[i] = underscoreClass(part)
	}
	return filepath.Join("app", "models", filepath.Join(parts...)) + ".rb"
}

// includedModules maps each class to the modules it includes, read from the
// same `include` statements the extractor already emits as relations.
func includedModules(repoPath string, files []string) map[string][]string {
	out := map[string][]string{}
	for _, relFile := range files {
		if !isModelFile(relFile) {
			continue
		}
		model := modelClassName(relFile)
		if model == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		for _, match := range includeStatement.FindAllStringSubmatch(string(raw), -1) {
			out[model] = append(out[model], match[1])
		}
	}
	return out
}

// includeStatement matches `include Foo` at statement position. Anchored to the
// line start so `# include Foo` in prose and `included do` blocks do not match.
var includeStatement = regexp.MustCompile(`(?m)^\s*include\s+([A-Z][\w:]*)\s*$`)

// concernAssociations reads the associations a module declares, including
// inside an `included do` block, which is where ActiveSupport::Concern puts
// them and therefore where most of them are.
func concernAssociations(repoPath string, files []string, module string) []modelAssociation {
	for _, relFile := range files {
		if !strings.HasSuffix(relFile, "/"+underscoreClass(module)+".rb") {
			continue
		}
		return parseModelFile(filepath.Join(repoPath, relFile))
	}
	return nil
}
