package phpextractor

import (
	"strconv"
	"strings"
)

// anonymousClassName names an anonymous class declared outside any function: the
// `return new class extends Migration { ... };` a Laravel migration file is.
//
// The class has no name of its own, and its members were named after the empty
// owner, `::up`. The graph is keyed by name, so every such file's `up` merged into
// one symbol. The name follows PHP's own, `class@anonymous` and the file that
// declares it, without the extension, so it carries no `.` that a reader of the
// name would take for a member separator. A second anonymous class in the same file
// takes PHP's `$1` suffix.
func (w *phpWalker) anonymousClassName() string {
	name := "class@anonymous:" + strings.TrimSuffix(w.relFile, ".php")
	if w.anonymous > 0 {
		name += "$" + strconv.Itoa(w.anonymous)
	}
	w.anonymous++
	return name
}
