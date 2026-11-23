package phpextractor

import (
	"slices"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// propFractalIncludes lists, on a class, the names its `$defaultIncludes` and
// `$availableIncludes` properties hold. A Fractal transformer's includeAccount runs
// because `account` is in one of those lists: Fractal builds the method name from the
// entry, so the list is the only place the method is named. The dead-code detector
// reads it, on classes that extend Fractal's TransformerAbstract only; a method the
// lists leave out is never called.
const propFractalIncludes = "fractal_includes"

var fractalIncludeProps = map[string]bool{"defaultIncludes": true, "availableIncludes": true}

// recordFractalIncludes adds the string entries of an include list property to its
// class's propFractalIncludes, sorted and each once.
func (w *phpWalker) recordFractalIncludes(el *sitter.Node) {
	idx := w.typeFactIndex()
	if idx < 0 {
		return
	}
	value := el.ChildByFieldName("default_value")
	if value == nil {
		return
	}
	if kindOf(value) == "property_initializer" && value.NamedChildCount() > 0 {
		value = value.NamedChild(0)
	}
	if kindOf(value) != "array_creation_expression" {
		return
	}
	have, _ := w.out[idx].Props[propFractalIncludes].([]string)
	for i := uint(0); i < value.NamedChildCount(); i++ {
		item := value.NamedChild(i)
		if kindOf(item) != "array_element_initializer" || item.NamedChildCount() != 1 {
			continue // a keyed entry is not a list item
		}
		if name := stringLiteral(item.NamedChild(0), w.src); name != "" && !slices.Contains(have, name) {
			have = append(have, name)
		}
	}
	if len(have) == 0 {
		return
	}
	slices.Sort(have)
	w.out[idx].Props[propFractalIncludes] = have
}
