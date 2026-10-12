package phpextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// The symbol walk leaves what it sees of Eloquent on the symbols it emits, and
// eloquentFacts turns it into storage and association facts once every file is read.
const (
	// propEloquentTable is a class's `protected $table = '...'` literal.
	propEloquentTable = "eloquent_table"
	// propEloquentRelation and propEloquentTarget are the relation a method returns
	// (`return $this->hasMany(Order::class)`): its builder and the class it names.
	propEloquentRelation = "eloquent_relation"
	propEloquentTarget   = "eloquent_target"
)

// eloquentBases are the framework classes whose subclasses are Eloquent models.
var eloquentBases = map[string]bool{
	`Illuminate\Database\Eloquent\Model`:                true,
	`Illuminate\Database\Eloquent\Relations\Pivot`:      true,
	`Illuminate\Database\Eloquent\Relations\MorphPivot`: true,
	`Illuminate\Foundation\Auth\User`:                   true,
	`Illuminate\Notifications\DatabaseNotification`:     true,
}

// eloquentRelations are the relation builders whose first argument names the related
// model. morphTo names none, so it declares no association.
var eloquentRelations = map[string]bool{
	"hasOne": true, "hasMany": true, "belongsTo": true, "belongsToMany": true,
	"hasOneThrough": true, "hasManyThrough": true,
	"morphOne": true, "morphMany": true, "morphToMany": true, "morphedByMany": true,
}

// recordEloquentTable notes a `$table = 'name'` property on its class.
func (w *phpWalker) recordEloquentTable(el *sitter.Node) {
	idx := w.typeFactIndex()
	if idx < 0 {
		return
	}
	for i := uint(0); i < el.NamedChildCount(); i++ {
		c := el.NamedChild(i)
		if kindOf(c) == "property_initializer" && c.NamedChildCount() > 0 {
			c = c.NamedChild(0)
		}
		if table := stringLiteral(c, w.src); table != "" {
			w.out[idx].Props[propEloquentTable] = table
			return
		}
	}
}

// recordEloquentRelation notes `$this->hasMany(Order::class)` on the method making
// the call. Whether that method belongs to a model is decided in eloquentFacts.
func (w *phpWalker) recordEloquentRelation(call *sitter.Node, ownerIdx int, method string) {
	if !eloquentRelations[method] || ownerIdx < 0 || len(w.typeStack) == 0 {
		return
	}
	if obj := call.ChildByFieldName("object"); obj == nil || phpText(obj, w.src) != "$this" {
		return
	}
	owner := &w.out[ownerIdx]
	if owner.PropString("symbol_kind") != facts.SymbolMethod || owner.PropString(propEloquentRelation) != "" {
		return
	}
	arg := positionalArg(call.ChildByFieldName("arguments"), 0)
	if arg == nil {
		return
	}
	// A class constant is resolved as PHP resolves it; a string names its class in full.
	target := strings.TrimLeft(stringLiteral(arg, w.src), `\`)
	if kindOf(arg) == "class_constant_access_expression" {
		target = w.className(classConstClass(arg, w.src))
	}
	if target == "" {
		return
	}
	owner.Props[propEloquentRelation] = method
	owner.Props[propEloquentTarget] = target
}

// eloquentFacts emits a storage fact per Eloquent model and an association fact per
// relation method a model declares.
//
// A model is a concrete class that extends an Eloquent base class, directly or
// through base classes of this repository. Its table is the `$table` it or its
// nearest ancestor declares, else the snake-cased plural of the class name, which is
// what Eloquent derives.
func eloquentFacts(allFacts []facts.Fact) []facts.Fact {
	ix := newClassIndex(allFacts)
	classes := map[string]*facts.Fact{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolClass && classes[f.Name] == nil {
			classes[f.Name] = f
		}
	}
	models := map[string]bool{}
	for name, f := range classes {
		if f.PropBool("trait") || f.PropBool("abstract") {
			continue
		}
		if ix.extendsEloquent(name, map[string]bool{}) {
			models[name] = true
		}
	}

	var out []facts.Fact
	for name := range models {
		f := classes[name]
		table, source := ix.declaredTable(name, classes), "declared"
		if table == "" {
			table, source = pluralize(snakeCase(lastNsSegment(name))), "derived"
		}
		out = append(out, facts.Fact{
			Kind: facts.KindStorage,
			Name: name,
			File: f.File,
			Line: f.Line,
			Props: map[string]any{
				"storage_kind": "model",
				"table":        table,
				"table_source": source,
				"language":     "php",
				"framework":    "eloquent",
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: declaresTarget(f)}},
		})
	}
	for i := range allFacts {
		f := &allFacts[i]
		macro := f.PropString(propEloquentRelation)
		sep := strings.LastIndex(f.Name, "::")
		if f.Kind != facts.KindSymbol || macro == "" || sep <= 0 || !models[f.Name[:sep]] {
			continue
		}
		model, target := f.Name[:sep], f.PropString(propEloquentTarget)
		out = append(out, facts.Fact{
			Kind: facts.KindAssociation,
			Name: f.Name,
			File: f.File,
			Line: f.Line,
			Props: map[string]any{
				"language":      "php",
				"framework":     "eloquent",
				"model":         model,
				"association":   f.Name[sep+2:],
				"macro":         macro,
				"target":        target,
				"target_source": "declared",
			},
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind > out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// extendsEloquent reports whether a class reaches an Eloquent base class through its
// parents.
func (ix *classIndex) extendsEloquent(class string, seen map[string]bool) bool {
	if seen[class] {
		return false
	}
	seen[class] = true
	for _, ref := range ix.parents[class] {
		if eloquentBases[strings.TrimPrefix(ref, `\`)] {
			return true
		}
		if parent := ix.resolve(class, ref); parent != "" && ix.extendsEloquent(parent, seen) {
			return true
		}
	}
	return false
}

// declaredTable returns the `$table` a class declares, else its nearest ancestor's.
func (ix *classIndex) declaredTable(class string, classes map[string]*facts.Fact) string {
	seen := map[string]bool{}
	for class != "" && !seen[class] {
		seen[class] = true
		if table := classes[class].PropString(propEloquentTable); table != "" {
			return table
		}
		next := ""
		for _, ref := range ix.parents[class] {
			if parent := ix.resolve(class, ref); parent != "" && !classes[parent].PropBool("trait") {
				next = parent
				break
			}
		}
		class = next
	}
	return ""
}

// snakeCase converts a StudlyCase class name to snake_case.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pluralize applies the regular English plural. Eloquent's inflector also knows the
// irregular ones; a model whose table is irregular reads as table_source "derived",
// which says the name was not read from the code.
func pluralize(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "sh"), strings.HasSuffix(s, "ch"),
		strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}
