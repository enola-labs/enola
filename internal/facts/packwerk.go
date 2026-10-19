package facts

// Packwerk usage props, set on a packwerk package's module fact by the Ruby
// extractor once every reference in the repository is resolved. They compare what
// package.yml declares with what the package's code actually references, at
// package granularity: an edge between two directories of one package is no
// dependency, and an edge between two packages is one whichever directory it left.
//
// Package names are the module fact names: the package path, or "root" for the
// root package.
const (
	// PropPackwerkObserved lists the packages this package's code references.
	PropPackwerkObserved = "packwerk_observed_dependencies"
	// PropPackwerkUnused lists the declared dependencies nothing in the package
	// references. A use the graph does not see as an edge (a constant named in a
	// string, a class looked up by name) leaves a dependency here too.
	PropPackwerkUnused = "packwerk_unused_dependencies"
	// PropPackwerkUndeclared lists the packages referenced without being declared.
	// Set only on a package that enforces dependencies: without enforcement packwerk
	// does not ask for the declaration, so its absence is no finding.
	PropPackwerkUndeclared = "packwerk_undeclared_dependencies"
	// PropPackwerkUndeclaredEvidence maps each undeclared dependency to the first
	// reference behind it: {"file", "symbol", "line"}.
	PropPackwerkUndeclaredEvidence = "packwerk_undeclared_evidence"
	// PropPackwerkTodo lists the packages package_todo.yml records a dependency
	// violation against: undeclared, but already acknowledged.
	PropPackwerkTodo = "packwerk_todo_dependencies"
)
