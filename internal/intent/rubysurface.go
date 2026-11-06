package intent

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// The Ruby declaration surface: the same laws, written the way the team that
// lives with them would say them.
//
// A declaration is read as data and never executed. The file is parsed with
// the Ruby grammar the extractors already carry, and only the constructs
// named here are understood; anything else is a problem citing its line, so a
// sentence that looks like a law but is not one can never pass silently.
//
// It compiles to exactly the declaration the YAML loader produces, so the
// evaluator, the provenance stamping, the lint surface and the pre-edit
// contract are the same for both spellings, and a repository can hold one of
// each while a team moves.
//
//	Enola.architecture "storefront" do
//	  rails
//	  part :maintenance, files: "app/tasks/**"
//
//	  law "background jobs never invoke controller code" do
//	    jobs.must_not_call controllers
//	    why "rendering from a job goes through ApplicationController.renderer"
//	  end
//	end

// railsParts is the vocabulary a `rails` line declares: the conventional parts
// of a Rails application, each selected by the directory Rails puts it in.
//
// They are path-selected rather than predicate-selected, and the validator is
// why. A component selected by a `where` predicate cannot sit on the far side
// of an edge rule: a predicate selects the facts carrying a property, and a
// class's calls ride its member facts, so an edge-walking rule resolves it
// against nothing. Declaring these by prop read better and would have made
// every edge law in the surface unwritable, which the first compile of the
// Rails example caught.
var railsParts = []struct{ part, files string }{
	{"models", "app/models/**"},
	{"controllers", "app/controllers/**"},
	{"jobs", "app/jobs/**"},
	{"mailers", "app/mailers/**"},
	{"policies", "app/policies/**"},
	{"serializers", "app/serializers/**"},
	{"components", "app/components/**"},
	{"channels", "app/channels/**"},
	{"helpers", "app/helpers/**"},
	{"concerns", "app/models/concerns/**"},
}

// edgeVerbs are the laws about edges: each names a rule form and the role its
// object fills.
var edgeVerbs = map[string]struct{ form, role string }{
	"must_not_call":      {"forbid", "to"},
	"must_not_reach":     {"forbid_reach", "to"},
	"may_only_call":      {"allow", "only"},
	"is_reached_only_by": {"protect", "owners"},
	"must_be_reached_by": {"require_edge", "to"},
	"must_reach":         {"require_edge", "to"},
	"stays_inside":       {"private", "except"},
	"must_follow":        {"protocol", "steps"},
}

// memberVerbs are the laws about a component's own members: no counterpart,
// each carrying its own argument shape.
var memberVerbs = map[string]string{
	"must_define":              "require_defines",
	"must_define_one_of":       "require_defines",
	"must_not_reach_includers": "independent",
	"must_not_cycle_with":      "forbid_cycles",
	"names_must_match":         "require_name",
	"names_must_not_match":     "forbid_name",
	"must_be_empty":            "forbid_fact",
	"at_most":                  "cap",
	"must_carry":               "require",
	"advises":                  "guide",
	"storage_must_stay_home":   "storage_stays_home",
	"must_keep_budget":         "cap_runtime",
	"must_have_consumer":       "require_consumer",
	"must_be_unique_across":    "unique_across",
	"must_be_governed":         "require_governed",
}

// ParseRubySurface reads a Ruby declaration file into the same shape the YAML
// loader produces. Every problem it finds is returned, each citing its line,
// because a declaration file is read by a person and one error at a time is a
// poor way to fix five.
func ParseRubySurface(src []byte, path string) (ConstraintsFile, []string) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return ConstraintsFile{}, []string{path + ": the Ruby grammar could not be loaded"}
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	r := &surfaceReader{src: src, path: path, parts: map[string]bool{}}
	r.walkTop(tree.RootNode())
	sort.Strings(r.problems)
	return ConstraintsFile{Path: path, Components: r.components, Rules: r.rules, UseRecipe: r.recipes}, r.problems
}

type surfaceReader struct {
	src        []byte
	path       string
	components []ConstraintComponent
	rules      []ConstraintRule
	parts      map[string]bool
	recipes    []RecipeInstantiation
	problems   []string
}

func (r *surfaceReader) fail(node *sitter.Node, format string, args ...any) {
	line := 0
	if node != nil {
		line = int(node.StartPosition().Row) + 1
	}
	r.problems = append(r.problems, fmt.Sprintf("%s:%d: %s", r.path, line, fmt.Sprintf(format, args...)))
}

func (r *surfaceReader) text(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	return string(r.src[node.StartByte():node.EndByte()])
}

func (r *surfaceReader) walkTop(node *sitter.Node) {
	if node == nil {
		return
	}
	if node.Kind() == "call" && r.methodName(node) == "architecture" {
		if body := blockBody(node); body != nil {
			r.readDeclarations(body)
		}
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		r.walkTop(node.NamedChild(i))
	}
}

func (r *surfaceReader) readDeclarations(body *sitter.Node) {
	for i := uint(0); i < body.NamedChildCount(); i++ {
		stmt := body.NamedChild(i)
		// A comment is prose about the laws, which is most of why a team would
		// want to write them in a file they read.
		if stmt.Kind() == "comment" {
			continue
		}
		r.readStatement(stmt)
	}
}

func (r *surfaceReader) readStatement(stmt *sitter.Node) {
	switch r.methodName(stmt) {
	case "rails":
		r.declareRailsParts()
	case "part":
		r.readPart(stmt)
	case "law":
		r.readLaw(stmt)
	case "use_recipe":
		r.readRecipeUse(stmt)
	default:
		r.fail(stmt, "%q is not a declaration; write part, rails or law", r.text(stmt))
	}
}

func (r *surfaceReader) declareRailsParts() {
	for _, p := range railsParts {
		if r.parts[p.part] {
			continue
		}
		r.parts[p.part] = true
		r.components = append(r.components, ConstraintComponent{
			Name:  componentToken(p.part),
			Match: []string{p.files},
		})
	}
}

func (r *surfaceReader) readPart(stmt *sitter.Node) {
	args := argumentNodes(stmt)
	if len(args) == 0 {
		r.fail(stmt, "part needs a name")
		return
	}
	name := r.symbolOrString(args[0])
	if name == "" {
		r.fail(stmt, "a part's name is a symbol or a string")
		return
	}
	component := ConstraintComponent{Name: componentToken(name)}
	for _, pair := range r.keywordPairs(args) {
		key := r.symbolOrString(pair.key)
		switch key {
		case "files":
			component.Match = append(component.Match, r.stringList(pair.value)...)
		case "kind":
			component.Kind = r.symbolOrString(pair.value)
		case "service":
			component.Service = r.symbolOrString(pair.value)
		case "named":
			component.NamePattern = r.symbolOrString(pair.value)
		case "where":
			component.Where = r.hash(pair.value)
		case "owns":
			// What a predicate-selected part owns is the one thing a rule about
			// its edges cannot infer, so the surface has to be able to say it.
			component.Owns = r.symbolOrString(pair.value)
		case "ancestor":
			component.Ancestor = r.symbolOrString(pair.value)
		case "public":
			component.Public = append(component.Public, r.stringList(pair.value)...)
		case "handles":
			component.Handles = append(component.Handles, r.stringList(pair.value)...)
		case "governed_by":
			component.GovernedBy = r.symbolOrString(pair.value)
		default:
			r.fail(pair.key, "a part takes files, kind, service, named, where, owns, ancestor, public, handles or governed_by, not %q", key)
		}
	}
	if len(component.Match) == 0 && component.Where == nil && component.NamePattern == "" && component.Ancestor == "" && len(component.Handles) == 0 && component.GovernedBy == "" {
		r.fail(stmt, "part %q selects nothing: give it files, where, named or ancestor", name)
		return
	}
	r.parts[name] = true
	r.components = append(r.components, component)
}

func (r *surfaceReader) readLaw(stmt *sitter.Node) {
	args := argumentNodes(stmt)
	body := blockBody(stmt)
	sentence := ""
	if len(args) > 0 {
		sentence = r.symbolOrString(args[0])
	}
	if sentence == "" || body == nil {
		r.fail(stmt, "a law is a sentence and a block")
		return
	}
	rule := ConstraintRule{ID: slug(sentence), Because: sentence}
	stated := false
	for i := uint(0); i < body.NamedChildCount(); i++ {
		line := body.NamedChild(i)
		if line.Kind() == "comment" {
			continue
		}
		r.readLawLine(line, &rule, &stated)
	}
	if !stated {
		r.fail(stmt, "law %q says nothing: give it a sentence like jobs.must_not_call controllers", sentence)
		return
	}
	// The kind of edge an antecedent reads is spelled once in the sentence and
	// lands where the form expects it: the existential form's own via already
	// names the edge it demands, so its antecedent gets a second key, and every
	// other form reads the antecedent on via itself.
	if rule.WhenVia != "" && rule.RequireEdge == "" {
		rule.Via, rule.WhenVia = rule.WhenVia, ""
	}
	r.rules = append(r.rules, rule)
}

func (r *surfaceReader) readLawLine(line *sitter.Node, rule *ConstraintRule, stated *bool) {
	method := r.methodName(line)
	args := argumentNodes(line)
	switch method {
	case "why", "because":
		if text := r.symbolOrString(firstOrNil(args)); text != "" {
			rule.Because = text
			return
		}
		r.fail(line, "%s takes the reason as a string", method)
		return
	case "seen_in":
		if text := r.symbolOrString(firstOrNil(args)); text != "" {
			rule.Because = strings.TrimSpace(rule.Because) + " (" + text + ")"
			return
		}
		r.fail(line, "seen_in takes the measurement as a string")
		return
	case "mode":
		rule.Mode = r.symbolOrString(firstOrNil(args))
		return
	case "since":
		rule.Since = r.symbolOrString(firstOrNil(args))
		return
	case "growth":
		if n, err := strconv.Atoi(strings.TrimSpace(r.symbolOrString(firstOrNil(args)))); err == nil {
			rule.Growth = n
		} else {
			r.fail(line, "growth takes a whole number")
		}
		return
	case "id":
		// A law's id is derived from its sentence, which is what keeps the two
		// in step. A team that has to keep an id stable across a rewording —
		// an exemption file, a dashboard, a suppression comment — says so.
		if id := r.symbolOrString(firstOrNil(args)); id != "" {
			rule.ID = id
			return
		}
		r.fail(line, "id takes the token a finding should carry")
		return
	case "direction":
		rule.Direction = r.symbolOrString(firstOrNil(args))
		return
	case "exemplar":
		if text := r.symbolOrString(firstOrNil(args)); text != "" {
			rule.Exemplars = append(rule.Exemplars, text)
			return
		}
		r.fail(line, "exemplar takes the prior art as a string")
		return
	case "when_carrying":
		r.readPropMatch(line, args, &rule.WhenPropContains, "when_carrying")
		return
	case "when_calling":
		rule.WhenEdgeTo = append(rule.WhenEdgeTo, r.literalList(args)...)
		for _, pair := range r.keywordPairs(args) {
			if r.symbolOrString(pair.key) == "via" {
				rule.WhenVia = r.symbolOrString(pair.value)
			}
		}
		if len(rule.WhenEdgeTo) == 0 {
			r.fail(line, "when_calling names the literals a member must already reach")
		}
		return
	case "via":
		rule.Via = r.symbolOrString(firstOrNil(args))
		return
	case "exempt":
		r.readExemption(line, rule, args)
		return
	}
	if receiver := r.receiverName(line); receiver != "" {
		r.readSubjectLine(line, receiver, method, args, rule, stated)
		return
	}
	r.fail(line, "%q is not part of a law", r.text(line))
}

func (r *surfaceReader) readSubjectLine(line *sitter.Node, subject, verb string, args []*sitter.Node, rule *ConstraintRule, stated *bool) {
	if *stated {
		r.fail(line, "a law states one thing; write a second law for the rest")
		return
	}
	if !r.parts[subject] {
		r.fail(line, "%q is not a part; declare it with part or rails first", subject)
		return
	}
	if edge, ok := edgeVerbs[verb]; ok {
		// A far end is either a part this declaration knows or a literal the
		// graph recorded: a bare name is the part, a string is the literal.
		// That is the difference the vocabulary draws between to and to_name,
		// and it is the difference between naming something we declared and
		// naming something we merely measured, so the surface keeps it visible
		// rather than guessing from whether the name happens to resolve.
		if literals := r.literalList(args); len(literals) > 0 {
			if edge.role != "to" {
				r.fail(line, "%s takes parts, not literals; only the forms with a single far end read a literal", verb)
				return
			}
			setForm(rule, edge.form, componentToken(subject))
			rule.ToName = append(rule.ToName, literals...)
			for _, pair := range r.keywordPairs(args) {
				if r.symbolOrString(pair.key) == "receiver" {
					rule.Receiver = r.symbolOrString(pair.value)
				}
			}
			if rule.Via == "" && formNeedsVia(edge.form) {
				rule.Via = "calls"
			}
			*stated = true
			return
		}
		targets := r.partList(args)
		// A role may also be named by the keyword that names it, which is how
		// the visibility form reads: stays_inside except: handlers.
		targets = append(targets, r.partsNamedBy(args, edge.role)...)
		if len(targets) == 0 && edge.form != "private" {
			r.fail(line, "%s names the part on the other side", verb)
			return
		}
		setForm(rule, edge.form, componentToken(subject))
		for i, target := range targets {
			targets[i] = componentToken(target)
		}
		switch edge.role {
		case "to":
			rule.To = firstString(targets)
		case "only":
			rule.Only = targets
		case "owners":
			rule.Owners = targets
		case "except":
			rule.Except = targets
		case "steps":
			rule.Steps = targets
		}
		if rule.Via == "" && formNeedsVia(edge.form) {
			rule.Via = "calls"
		}
		// The existential form demands an edge in a direction, and the two
		// verbs are the two directions: being reached is inbound, reaching is
		// outbound.
		if edge.form == "require_edge" {
			if verb == "must_reach" {
				rule.Direction = "outbound"
			} else {
				rule.Direction = "inbound"
			}
		}
		*stated = true
		return
	}
	form, ok := memberVerbs[verb]
	if !ok {
		r.fail(line, "%q is not something a part can be told to do", verb)
		return
	}
	setForm(rule, form, componentToken(subject))
	r.readMemberArguments(line, form, args, rule)
	*stated = true
}

func (r *surfaceReader) readMemberArguments(line *sitter.Node, form string, args []*sitter.Node, rule *ConstraintRule) {
	first := r.symbolOrString(firstOrNil(args))
	switch form {
	case "require_defines":
		if len(args) > 1 {
			rule.AnyOf = r.symbolList(args)
		} else {
			rule.Method = first
		}
	case "forbid_cycles":
		rule.Among = r.symbolList(args)
		for i := range rule.Among {
			rule.Among[i] = componentToken(rule.Among[i])
		}
	case "require_name", "forbid_name":
		rule.Pattern = first
		for _, pair := range r.keywordPairs(args) {
			switch r.symbolOrString(pair.key) {
			case "surface":
				rule.Surface = r.symbolOrString(pair.value)
			case "requires":
				rule.Requires = r.symbolOrString(pair.value)
			}
		}
	case "cap_runtime":
		for _, pair := range r.keywordPairs(args) {
			switch key := r.symbolOrString(pair.key); key {
			case "metric":
				rule.Metric = r.symbolOrString(pair.value)
			case "max":
				if n, err := strconv.Atoi(strings.TrimSpace(r.symbolOrString(pair.value))); err == nil {
					rule.Max = n
				} else {
					r.fail(pair.value, "max takes a whole number")
				}
			default:
				r.fail(pair.key, "must_keep_budget takes metric: and max:, not %q", key)
			}
		}
	case "unique_across":
		for _, pair := range r.keywordPairs(args) {
			if key := r.symbolOrString(pair.key); key == "by" {
				rule.By = r.symbolOrString(pair.value)
			} else {
				r.fail(pair.key, "must_be_unique_across takes by:, not %q", key)
			}
		}
	case "cap":
		if n, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			rule.MaxMembers = n
		} else {
			r.fail(line, "at_most takes a number")
		}
	case "guide":
		rule.Message = first
	case "require":
		r.readPropMatch(line, args, &rule.MustPropContain, "must_carry")
	}
}

func (r *surfaceReader) readExemption(line *sitter.Node, rule *ConstraintRule, args []*sitter.Node) {
	exemption := ConstraintExemption{Witness: r.symbolOrString(firstOrNil(args))}
	for _, pair := range r.keywordPairs(args) {
		switch r.symbolOrString(pair.key) {
		case "because":
			exemption.Because = r.symbolOrString(pair.value)
		case "owner":
			exemption.Owner = r.symbolOrString(pair.value)
		case "since":
			exemption.Since = r.symbolOrString(pair.value)
		}
	}
	if exemption.Witness == "" || exemption.Because == "" {
		r.fail(line, "an exemption names what it carves out and why")
		return
	}
	rule.Exempt = append(rule.Exempt, exemption)
}

func setForm(rule *ConstraintRule, form, subject string) {
	switch form {
	case "forbid":
		rule.Forbid = subject
	case "forbid_reach":
		rule.ForbidReach = subject
	case "allow":
		rule.Allow = subject
	case "protect":
		rule.Protect = subject
	case "private":
		rule.Private = subject
	case "forbid_fact":
		rule.ForbidFact = subject
	case "cap":
		rule.Cap = subject
	case "require":
		rule.Require = subject
	case "require_edge":
		rule.RequireEdge = subject
	case "require_defines":
		rule.RequireDefines = subject
	case "forbid_cycles":
		rule.ForbidCycles = subject
	case "independent":
		rule.Independent = subject
	case "require_name":
		rule.RequireName = subject
	case "forbid_name":
		rule.ForbidName = subject
	case "storage_stays_home":
		rule.StorageStaysHome = subject
	case "cap_runtime":
		rule.CapRuntime = subject
	case "require_consumer":
		rule.RequireConsumer = subject
	case "unique_across":
		rule.UniqueAcross = subject
	case "require_governed":
		rule.RequireGoverned = subject
	case "protocol":
		rule.Protocol = subject
	case "guide":
		rule.Guide = subject
	}
}

func formNeedsVia(form string) bool {
	switch form {
	case "forbid", "forbid_reach", "allow", "protect", "require_edge", "protocol":
		return true
	}
	return false
}

// componentToken is a part's name as the declaration vocabulary spells it.
// Ruby names a part in snake_case because that is what a Ruby file reads
// like; a component name is a lowercase token, so the underscore becomes a
// dash on the way through and the sentence and the compiled law stay the
// same thing said twice.
func componentToken(part string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(part)), "_", "-")
}

// slug turns a law's sentence into the lowercase-token id every finding
// carries, so the id is derived from the sentence rather than repeated beside
// it.
func slug(sentence string) string {
	var b strings.Builder
	lastDash := true
	for _, c := range strings.ToLower(sentence) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// readPropMatch reads a prop-and-value pair, which is how both the demand and
// the antecedent of the require form are written.
func (r *surfaceReader) readPropMatch(line *sitter.Node, args []*sitter.Node, into **PropMatch, verb string) {
	match := &PropMatch{}
	for _, pair := range r.keywordPairs(args) {
		switch r.symbolOrString(pair.key) {
		case "prop":
			match.Prop = r.symbolOrString(pair.value)
		case "value":
			match.Value = r.symbolOrString(pair.value)
		}
	}
	if match.Prop == "" || match.Value == "" {
		r.fail(line, "%s names the prop and the value it must contain", verb)
		return
	}
	*into = match
}

// readRecipeUse instantiates a named recipe: the bundle of laws somebody else
// wrote, bound to this repository's own parts. It is the one line a team
// writes to adopt a convention set it did not author.
//
//	use_recipe :ember_conventions, as: :app do
//	  bind :components, files: "app/components/**"
//	  bind :fetchers, files: "app/services/**", kind: :symbol
//	end
func (r *surfaceReader) readRecipeUse(stmt *sitter.Node) {
	args := argumentNodes(stmt)
	name := r.symbolOrString(firstOrNil(args))
	if name == "" {
		r.fail(stmt, "use_recipe names the recipe to instantiate")
		return
	}
	instance := RecipeInstantiation{Recipe: name, Bind: map[string]RecipeBinding{}}
	for _, pair := range r.keywordPairs(args) {
		switch r.symbolOrString(pair.key) {
		case "as":
			instance.As = r.symbolOrString(pair.value)
		case "mode":
			instance.Mode = r.symbolOrString(pair.value)
		}
	}
	if instance.As == "" {
		r.fail(stmt, "use_recipe needs an as: naming this instantiation, since a recipe may be instantiated more than once")
		return
	}
	body := blockBody(stmt)
	if body == nil {
		r.fail(stmt, "use_recipe binds each role the recipe declares, in a block")
		return
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		bindLine := body.NamedChild(i)
		if bindLine.Kind() == "comment" {
			continue
		}
		if r.methodName(bindLine) != "bind" {
			r.fail(bindLine, "only bind lines belong in a use_recipe block")
			continue
		}
		bindArgs := argumentNodes(bindLine)
		role := r.symbolOrString(firstOrNil(bindArgs))
		if role == "" {
			r.fail(bindLine, "bind names the recipe role it fills")
			continue
		}
		binding := RecipeBinding{}
		for _, pair := range r.keywordPairs(bindArgs) {
			switch key := r.symbolOrString(pair.key); key {
			case "files":
				binding.Match = append(binding.Match, r.stringList(pair.value)...)
			case "kind":
				binding.Kind = r.symbolOrString(pair.value)
			case "service":
				binding.Service = r.symbolOrString(pair.value)
			case "named":
				binding.NamePattern = r.symbolOrString(pair.value)
			case "where":
				binding.Where = r.hash(pair.value)
			case "ancestor":
				binding.Ancestor = r.symbolOrString(pair.value)
			default:
				r.fail(pair.key, "a bind takes files, kind, service, named, where or ancestor, not %q", key)
			}
		}
		instance.Bind[role] = binding
	}
	if len(instance.Bind) == 0 {
		r.fail(stmt, "use_recipe %q binds no role, so it selects nothing", name)
		return
	}
	r.recipes = append(r.recipes, instance)
}

// partsNamedBy reads the parts a law names through the keyword that names the
// role, so a sentence may put its far end after the verb or after the word for
// the role, whichever reads better.
func (r *surfaceReader) partsNamedBy(args []*sitter.Node, role string) []string {
	var out []string
	for _, pair := range r.keywordPairs(args) {
		if r.symbolOrString(pair.key) != role {
			continue
		}
		if pair.value != nil && pair.value.Kind() == "array" {
			for i := uint(0); i < pair.value.NamedChildCount(); i++ {
				if name := r.symbolOrString(pair.value.NamedChild(i)); name != "" {
					out = append(out, name)
				}
			}
			continue
		}
		if name := r.symbolOrString(pair.value); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// symbolList reads every positional symbol or string argument, in order.
func (r *surfaceReader) symbolList(args []*sitter.Node) []string {
	var out []string
	for _, a := range args {
		if a.Kind() == "pair" {
			continue
		}
		if v := r.symbolOrString(a); v != "" {
			out = append(out, v)
		}
	}
	return out
}
