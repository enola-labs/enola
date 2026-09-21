// Angular templates — the reason the rest of this dialect can be trusted.
//
// In Angular a component member is very often referenced ONLY from its template:
// `(click)="save()"`, `{{ total }}`, `*ngIf="loading"`. So is a child component,
// which appears as a tag and nowhere else in the class. Measured across ten public
// repositories, 4,251 external templates and 10,844 inline ones were walked past
// entirely — every one of those references invisible, and every symbol whose only
// use is in a template reading as code nothing calls. That is not a coverage gap,
// it is a false-positive generator, and it is what the dead-code and orphan
// readings would have been fed.
//
// The resolution regimes, in decreasing order of certainty — the shape Ember's
// resolver established:
//
//   - A binding identifier is an edge ONLY when it names a member the owning
//     component declares. `{{ title }}` where the class has no `title` is
//     indistinguishable from a local, an @Input alias or a global, and produces
//     nothing.
//   - A tag resolves against the selector its component DECLARED, and only when
//     exactly one component declares it. Same for an attribute against a
//     directive's selector, and a pipe name against a pipe's.
//   - Everything else is counted, never guessed: an unknown custom-element tag is
//     an unresolved selector, two components claiming one selector are ambiguous.
//
// The scan is lexical. There is no HTML grammar in this build, and an Angular
// template is not HTML anyway once `@if`/`@for`/`@defer` blocks are in it — both
// dialects are live in the same corpus and frequently in the same file.
package tsextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// angularTemplate is one template's scanned content: the raw material for the
// repo-wide join, which is where the owning component and the selector index are
// both in hand.
type angularTemplate struct {
	// file is the template's own path for an external template, or the component
	// file for an inline one.
	file string
	// idents are the identifiers every binding expression names, deduplicated and
	// with template-local names (block variables, reference variables) removed.
	idents []string
	// elements are the element occurrences the template renders, each with the
	// attributes written on it. Attributes travel WITH their element because a real
	// Angular selector is routinely compound — `tui-data-list-wrapper[labels]` — and
	// a flat list of attribute names matches such a selector on any element that
	// happens to carry the attribute.
	elements []angularElementUse
	// pipes are the pipe names its expressions apply.
	pipes []string
	// links are the literal routerLink paths it navigates to.
	links []string
}

// angularElementUse is one element occurrence: its tag and the attributes written
// on it, with binding punctuation stripped so `[disabled]`, `(click)` and
// `*ngIf` are all seen as the names they bind.
type angularElementUse struct {
	tag   string
	attrs []string
}

var (
	// angularInterpolation matches `{{ … }}`. Non-greedy so two on one line stay two.
	angularInterpolation = regexp.MustCompile(`\{\{([^}]*)\}\}`)

	// angularBinding matches the three binding forms whose VALUE is an expression:
	// [prop]="…", (event)="…", *structural="…", and the banana-in-a-box [(x)]="…".
	angularBinding = regexp.MustCompile(`(?:\[\(?[\w.$-]+\)?\]|\([\w.$-]+\)|\*[\w-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

	// angularBlock matches an Angular 17 control-flow block head. The expression is
	// inside the parentheses; `@else`, `@empty` and `@placeholder` have none.
	angularBlock = regexp.MustCompile(`@(?:if|else if|for|switch|case|defer)\s*\(([^)]*)\)`)

	// angularElement matches one opening tag with everything up to its close, so an
	// attribute can be attributed to the element it is written on.
	angularElement = regexp.MustCompile(`(?s)<([a-zA-Z][\w.-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>`)

	// angularAttrName matches an attribute name inside a tag, including the
	// bracketed and parenthesised binding forms.
	angularAttrName = regexp.MustCompile(`(?:^|\s)[*\[(]*([a-zA-Z][\w.-]*)[)\]]*`)

	// angularPipe matches a pipe application inside an expression: `| name`, but not
	// the `||` operator.
	angularPipe = regexp.MustCompile(`[^|]\|\s*([a-zA-Z_$][\w$]*)`)

	// angularRouterLink matches a literal routerLink target. The array form
	// ([routerLink]="['/a', id]") is deliberately not read: its first segment is a
	// literal but the rest is not, and half a path is not a path.
	angularRouterLink = regexp.MustCompile(`routerLink\s*=\s*"(/[^"]*)"`)

	// angularIdent matches an identifier in an expression.
	angularIdent = regexp.MustCompile(`[A-Za-z_$][\w$]*`)

	// angularLocalDecl matches the names a template binds itself: `let x` (both the
	// *ngFor microsyntax and the @for/@let forms), `as alias`, `#ref`, and a
	// `@for (item of items)` loop variable.
	angularLocalDecl = regexp.MustCompile(`(?:\blet\s+([\w$]+)|\bas\s+([\w$]+)|#([\w$]+)|@for\s*\(\s*([\w$]+)\s+of\b)`)
)

// angularExprKeywords are the words an expression may contain that never name a
// component member.
var angularExprKeywords = map[string]bool{
	"let": true, "of": true, "as": true, "in": true, "track": true, "when": true,
	"on": true, "true": true, "false": true, "null": true, "undefined": true,
	"this": true, "typeof": true, "new": true, "else": true, "if": true, "for": true,
	"switch": true, "case": true, "default": true, "void": true, "await": true,
	"$event": true, "$implicit": true, "$any": true, "$index": true, "$first": true,
	"$last": true, "$even": true, "$odd": true, "$count": true,
}

// scanAngularTemplate reads one template's markup into the references it makes.
func scanAngularTemplate(src []byte, file string) *angularTemplate {
	text := string(src)
	t := &angularTemplate{file: file}

	locals := map[string]bool{}
	for _, m := range angularLocalDecl.FindAllStringSubmatch(text, -1) {
		for _, g := range m[1:] {
			if g != "" {
				locals[g] = true
			}
		}
	}

	var exprs []string
	for _, m := range angularInterpolation.FindAllStringSubmatch(text, -1) {
		exprs = append(exprs, m[1])
	}
	for _, m := range angularBinding.FindAllStringSubmatch(text, -1) {
		exprs = append(exprs, m[1]+m[2])
	}
	for _, m := range angularBlock.FindAllStringSubmatch(text, -1) {
		exprs = append(exprs, m[1])
	}

	seenIdent := map[string]bool{}
	seenPipe := map[string]bool{}
	for _, expr := range exprs {
		for _, name := range angularExprIdents(expr, locals) {
			if !seenIdent[name] {
				seenIdent[name] = true
				t.idents = append(t.idents, name)
			}
		}
		for _, m := range angularPipe.FindAllStringSubmatch(" "+expr, -1) {
			if name := m[1]; !seenPipe[name] {
				seenPipe[name] = true
				t.pipes = append(t.pipes, name)
			}
		}
	}

	t.elements = angularElementUses(text)
	t.links = angularUniqueMatches(angularRouterLink, text)

	if len(t.idents) == 0 && len(t.elements) == 0 && len(t.pipes) == 0 && len(t.links) == 0 {
		return nil
	}
	return t
}

// angularExprIdents returns the identifiers an expression names that could be a
// component member: the head of each member chain, plus what follows `this.`.
// Property accesses after the head are the member's own shape, not the component's.
func angularExprIdents(expr string, locals map[string]bool) []string {
	var out []string
	locs := angularIdent.FindAllStringIndex(expr, -1)
	for i, loc := range locs {
		name := expr[loc[0]:loc[1]]
		afterDot := loc[0] > 0 && expr[loc[0]-1] == '.'
		if afterDot {
			// `this.total` names the member `total`; `user.name` names `user`, whose
			// `name` is a property of whatever the member holds.
			if i == 0 || expr[locs[i-1][0]:locs[i-1][1]] != "this" {
				continue
			}
		}
		if angularExprKeywords[name] || locals[name] {
			continue
		}
		// A pipe name is not a member reference; it is resolved separately.
		if k := strings.LastIndexByte(strings.TrimSpace(expr[:loc[0]]), '|'); k >= 0 &&
			strings.TrimSpace(expr[:loc[0]])[k:] == "|" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// angularElementUses parses every opening tag into its name and attribute names.
func angularElementUses(text string) []angularElementUse {
	var out []angularElementUse
	seen := map[string]bool{}
	for _, m := range angularElement.FindAllStringSubmatch(text, -1) {
		use := angularElementUse{tag: m[1]}
		attrSeen := map[string]bool{}
		for _, a := range angularAttrName.FindAllStringSubmatch(m[2], -1) {
			name := a[1]
			if name == "" || attrSeen[name] {
				continue
			}
			attrSeen[name] = true
			use.attrs = append(use.attrs, name)
		}
		// One element written twice with the same attributes is one reference.
		key := use.tag + "\x00" + strings.Join(use.attrs, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, use)
	}
	return out
}

// angularUniqueMatches returns the deduplicated first capture group of every match.
func angularUniqueMatches(re *regexp.Regexp, text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if v := m[1]; v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// --- the repo-wide join ------------------------------------------------------

// angularSelector is one declared selector, kept whole so it can be matched whole:
// `tui-data-list-wrapper[labels]` selects that element carrying that attribute, and
// matching either half alone attaches the component to templates that never render
// it. Measured: that was the one wrong edge in a 200-edge audit, and the shape it
// took was an attribute name common enough to appear elsewhere.
type angularSelector struct {
	elem   string
	attrs  []string
	target string
}

// angularSelectorIndex answers which declarations a rendered element matches. A
// selector claimed by two different classes is dropped rather than resolved
// arbitrarily: one candidate required, as everywhere else in this dialect.
type angularSelectorIndex struct {
	byElem    map[string][]angularSelector
	byAttr    map[string][]angularSelector
	pipes     map[string]string
	ambiguous map[string]bool
}

func buildAngularSelectorIndex(all []facts.Fact) *angularSelectorIndex {
	idx := &angularSelectorIndex{
		byElem:    map[string][]angularSelector{},
		byAttr:    map[string][]angularSelector{},
		pipes:     map[string]string{},
		ambiguous: map[string]bool{},
	}
	claimed := map[string]string{} // selector text -> declaring symbol
	add := func(key string, m map[string][]angularSelector, sel angularSelector) {
		m[key] = append(m[key], sel)
	}
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropString(facts.PropFramework) != AngularFramework {
			continue
		}
		switch f.PropString("web_component") {
		case "component", "directive":
			for _, raw := range strings.Split(f.PropString("angular_selector"), ",") {
				elem, attrs := angularSelectorParts(raw)
				if elem == "" && len(attrs) == 0 {
					continue
				}
				key := elem + "|" + strings.Join(attrs, "|")
				if prev, ok := claimed[key]; ok && prev != f.Name {
					idx.ambiguous[key] = true
					continue
				}
				claimed[key] = f.Name
				// Filed under whichever half narrows it most: an element when it has
				// one, else its first attribute. `match` then verifies the rest, so a
				// compound selector needs BOTH halves and `<button>` is not ambiguous
				// between the dozen directives that select `button[…]`.
				sel := angularSelector{elem: elem, attrs: attrs, target: f.Name}
				if elem != "" {
					add(elem, idx.byElem, sel)
				} else {
					add(attrs[0], idx.byAttr, sel)
				}
			}
		case "pipe":
			name := f.PropString("angular_pipe_name")
			if name == "" {
				continue
			}
			if prev, ok := idx.pipes[name]; ok && prev != f.Name {
				idx.ambiguous[name] = true
				delete(idx.pipes, name)
				continue
			}
			if !idx.ambiguous[name] {
				idx.pipes[name] = f.Name
			}
		}
	}
	// A selector two classes claim resolves to neither.
	for key := range idx.ambiguous {
		for _, m := range []map[string][]angularSelector{idx.byElem, idx.byAttr} {
			for k, sels := range m {
				kept := sels[:0]
				for _, sel := range sels {
					if sel.elem+"|"+strings.Join(sel.attrs, "|") != key {
						kept = append(kept, sel)
					}
				}
				m[k] = kept
			}
		}
	}
	return idx
}

// match returns every declaration one rendered element matches. Angular applies all
// matching directives to an element at once, so this is deliberately not
// one-candidate: a component and two directives on the same tag are three real
// references. What must be unique is a SELECTOR's owner, which the index enforces.
func (idx *angularSelectorIndex) match(use angularElementUse) []string {
	have := make(map[string]bool, len(use.attrs))
	for _, a := range use.attrs {
		have[a] = true
	}
	seen := map[string]bool{}
	var out []string
	consider := func(sels []angularSelector) {
		for _, sel := range sels {
			if sel.elem != "" && sel.elem != use.tag {
				continue
			}
			ok := true
			for _, a := range sel.attrs {
				if !have[a] {
					ok = false
					break
				}
			}
			if !ok || seen[sel.target] {
				continue
			}
			seen[sel.target] = true
			out = append(out, sel.target)
		}
	}
	consider(idx.byElem[use.tag])
	for _, a := range use.attrs {
		consider(idx.byAttr[a])
	}
	return out
}

// angularSelectorParts splits one selector into the element it selects and the
// attributes that select it.
//
// A real selector is routinely compound — `input[type="checkbox"][tuiCheckbox]` —
// and two rules keep such a selector from matching everything:
//
//   - A VALUE-CONSTRAINED group is dropped. `[type="checkbox"]` selects one kind of
//     input, and indexing it under `type` would attach the checkbox component to
//     every template that writes a `type` attribute at all.
//   - A group naming a standard HTML attribute is dropped for the same reason. What
//     is left is the distinctive attribute the directive is really named by, which
//     is the one a template author types on purpose.
func angularSelectorParts(sel string) (elem string, attrs []string) {
	sel = angularStripPseudo(strings.TrimSpace(sel))
	if sel == "" || strings.HasPrefix(sel, ".") {
		return "", nil // a class selector names nothing a tag scan sees
	}
	rest := sel
	if i := strings.IndexByte(sel, '['); i >= 0 {
		if i > 0 {
			elem = strings.TrimSpace(sel[:i])
		}
		rest = sel[i:]
	} else {
		elem = sel
		rest = ""
	}
	for len(rest) > 0 {
		open := strings.IndexByte(rest, '[')
		if open < 0 {
			break
		}
		close := strings.IndexByte(rest[open:], ']')
		if close < 0 {
			break
		}
		group := rest[open+1 : open+close]
		rest = rest[open+close+1:]
		if strings.ContainsRune(group, '=') {
			continue // value-constrained: it selects a value, not a directive name
		}
		if angularStandardAttrs[strings.ToLower(group)] {
			continue
		}
		attrs = append(attrs, group)
	}
	if elem != "" && (strings.ContainsAny(elem, ".:*>,") || strings.Contains(elem, " ")) {
		elem = ""
	}
	return elem, attrs
}

// angularStripPseudo removes a `:not(…)` clause and anything after a stray colon.
//
// `selector: 'tui-icon:not([tuiBadge])'` is an ordinary element selector with an
// exclusion attached, and refusing to read it left 1,095 of one library's 1,331
// components with no indexed selector at all — so every tag rendering them counted
// as an unknown element. The exclusion itself is not modelled: it narrows which
// elements match, and this index only answers which component a tag NAMES.
func angularStripPseudo(sel string) string {
	for {
		i := strings.Index(sel, ":not(")
		if i < 0 {
			break
		}
		depth, j := 0, i+4
		for ; j < len(sel); j++ {
			switch sel[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if j >= len(sel) {
			return strings.TrimSpace(sel[:i])
		}
		sel = sel[:i] + sel[j+1:]
	}
	if i := strings.IndexByte(sel, ':'); i >= 0 {
		sel = sel[:i]
	}
	return strings.TrimSpace(sel)
}

// angularBuiltInTags are the elements Angular itself defines. They render no
// component of the repository's own, so counting them as unresolved selectors would
// report the framework as a gap.
var angularBuiltInTags = map[string]bool{
	"ng-template": true, "ng-container": true, "ng-content": true,
	"router-outlet": true, "ng-component": true,
}

// angularStandardAttrs are attribute names HTML already gives meaning to. A
// directive selected on one of them is real, but the attribute is written for its
// HTML meaning far more often than for the directive's, so indexing it would turn
// every ordinary template into a consumer.
var angularStandardAttrs = map[string]bool{
	"class": true, "id": true, "style": true, "type": true, "name": true, "value": true,
	"disabled": true, "href": true, "src": true, "title": true, "role": true, "for": true,
	"placeholder": true, "label": true, "color": true, "size": true, "width": true,
	"height": true, "align": true, "target": true, "alt": true, "checked": true,
	"required": true, "readonly": true, "min": true, "max": true, "step": true,
	"rows": true, "cols": true, "tabindex": true, "hidden": true, "open": true,
	"selected": true, "multiple": true, "pattern": true, "autocomplete": true,
	"form": true, "action": true, "method": true, "slot": true, "part": true,
	"content": true, "data": true, "list": true, "loading": true, "rel": true,
}

// attachAngularTemplates joins each scanned template to the component that owns it
// and writes the references it makes as edges on that component.
//
// It runs repo-wide because both halves of every resolution live outside the
// template: the members belong to the owning class, and a tag names a selector some
// other file declared.
func attachAngularTemplates(all []facts.Fact, templates map[string]*angularTemplate, inline map[string]*angularTemplate) angularCounts {
	var counts angularCounts
	if len(templates) == 0 && len(inline) == 0 {
		return counts
	}
	idx := buildAngularSelectorIndex(all)

	// Members, by owning class. A template's binding may name a method, a field, a
	// getter or an input; they are all symbols whose name is `<class>.<member>`.
	members := map[string]map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if i := strings.LastIndexByte(f.Name, '.'); i > 0 {
			owner := f.Name[:i]
			if members[owner] == nil {
				members[owner] = map[string]bool{}
			}
			members[owner][f.Name[i+1:]] = true
		}
	}

	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || f.PropString(facts.PropFramework) != AngularFramework {
			continue
		}
		var t *angularTemplate
		switch {
		case f.PropString("angular_template_url") != "":
			t = templates[f.PropString("angular_template_url")]
			if t == nil {
				// The component names a template this snapshot does not hold.
				counts.miss("template_not_found")
				continue
			}
		case inline[f.Name] != nil:
			t = inline[f.Name]
		default:
			continue
		}
		counts.merge(angularTemplateEdges(f, t, idx, members[f.Name]))
	}
	return counts
}

// angularTemplateEdges writes one template's references onto its component.
func angularTemplateEdges(f *facts.Fact, t *angularTemplate, idx *angularSelectorIndex, own map[string]bool) angularCounts {
	var counts angularCounts
	add := func(target string) {
		if target == "" || target == f.Name || f.HasRelation(facts.RelCalls, target) {
			return
		}
		counts.resolved++
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
	}

	for _, name := range t.idents {
		// The rule the whole pass rests on: a binding is an edge only when it names
		// a member this component declares. Anything else is a local, an alias or a
		// global, and an edge for it would be invented rather than read.
		if own[name] {
			add(f.Name + "." + name)
		}
	}
	for _, use := range t.elements {
		targets := idx.match(use)
		for _, target := range targets {
			add(target)
		}
		if len(targets) > 0 || angularBuiltInTags[use.tag] {
			continue
		}
		if strings.ContainsRune(use.tag, '-') {
			// A hyphen is what makes an element name custom; a plain `div` is HTML
			// and names nothing this repository declares.
			counts.miss("unknown_selector")
		}
	}
	for _, pipe := range t.pipes {
		switch {
		case idx.pipes[pipe] != "":
			add(idx.pipes[pipe])
		case idx.ambiguous[pipe]:
			counts.miss("ambiguous_pipe")
		default:
			counts.miss("unknown_pipe")
		}
	}
	if len(t.links) > 0 {
		f.SetProp("angular_router_links", strings.Join(t.links, ","))
	}
	return counts
}
