package swiftextractor

import "testing"

// Swift's default access is internal: only public and open declarations, in types
// that are themselves public or open, can be named outside the module. A member
// stating no access in a `public extension` is public; a setter's access
// (`private(set)`) is not the declaration's.
func TestSwiftExportedFollowsAccessControl(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Access.swift": `
public struct Api {
    public func open() {}
    func helper() {}
    public private(set) var count = 0
    public struct Nested {}
}

struct Internal {
    public func reachable() {}
}

open class Base {
    open func override() {}
}

package func shared() {}
func plain() {}
private func hidden() {}
public typealias Handler = () -> Void

public extension Api {
    func viaExtension() {}
    internal func stated() {}
}

extension Api {
    func internalExtension() {}
    public func publicInExtension() {}
}

public protocol Service {}
protocol InternalService {}
`,
	})
	want := map[string]bool{
		"Sources/Lib.Api":                   true,
		"Sources/Lib.Api.open":              true,
		"Sources/Lib.Api.helper":            false,
		"Sources/Lib.Api.count":             true,
		"Sources/Lib.Api.Nested":            true,
		"Sources/Lib.Internal":              false,
		"Sources/Lib.Internal.reachable":    false,
		"Sources/Lib.Base":                  true,
		"Sources/Lib.Base.override":         true,
		"Sources/Lib.shared":                false,
		"Sources/Lib.plain":                 false,
		"Sources/Lib.hidden":                false,
		"Sources/Lib.Handler":               true,
		"Sources/Lib.Api.viaExtension":      true,
		"Sources/Lib.Api.stated":            false,
		"Sources/Lib.Api.internalExtension": false,
		"Sources/Lib.Api.publicInExtension": true,
		"Sources/Lib.Service":               true,
		"Sources/Lib.InternalService":       false,
	}
	seen := map[string]bool{}
	for _, f := range ff {
		w, ok := want[f.Name]
		if !ok {
			continue
		}
		seen[f.Name] = true
		if got := f.PropBool("exported"); got != w {
			t.Errorf("%s: exported=%v, want %v", f.Name, got, w)
		}
	}
	for n := range want {
		if !seen[n] {
			t.Errorf("no fact %s", n)
		}
	}
}
