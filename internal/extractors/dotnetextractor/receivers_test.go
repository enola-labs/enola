package dotnetextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func csSym(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	t.Fatalf("no symbol ending in %s", suffix)
	return facts.Fact{}
}

func csCalls(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	return out
}

func csCallsSuffix(f facts.Fact, suffix string) bool {
	return slices.ContainsFunc(csCalls(f), func(s string) bool { return strings.HasSuffix(s, suffix) })
}

func csPerformsIO(f facts.Fact) bool {
	b, _ := f.PropAny("performs_io").(bool)
	return b
}

const csLibraryFixture = `namespace Lib.Core;

public interface ILibraryManager
{
    void DeleteItem(Item item);
    string Describe(Item item);
}
`

const csLibraryImplFixture = `namespace Lib.Impl;

using System.IO;
using Lib.Core;

public class LibraryManager : ILibraryManager
{
    public void DeleteItem(Item item)
    {
        File.Delete(item.Path);
    }

    public string Describe(Item item)
    {
        return item.Name;
    }
}
`

// The injected dependency: the provider holds an interface, the I/O is in the
// class behind it. The call on the field is an edge to the interface's method,
// the in-loop list names that method, and the closure carries the I/O back.
func TestCSharpCallOnAnInjectedFieldIsAnEdge(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Lib.Core/ILibraryManager.cs": csLibraryFixture,
		"Lib.Impl/LibraryManager.cs":  csLibraryImplFixture,
		"Lib.Tv/Provider.cs": `namespace Lib.Tv;

using System.Collections.Generic;
using Lib.Core;

public class Provider
{
    private readonly ILibraryManager _libraryManager;

    public Provider(ILibraryManager libraryManager)
    {
        _libraryManager = libraryManager;
    }

    public void Prune(IEnumerable<Item> episodes)
    {
        foreach (var episode in episodes)
        {
            _libraryManager.DeleteItem(episode);
        }
    }

    public void Titles(IEnumerable<Item> episodes)
    {
        foreach (var episode in episodes)
        {
            _libraryManager.Describe(episode);
        }
    }
}
`,
	})
	prune := csSym(t, ff, "Provider.Prune")
	if !csCallsSuffix(prune, "ILibraryManager.DeleteItem") {
		t.Errorf("Prune calls %v, want the interface's DeleteItem", csCalls(prune))
	}
	if inLoop, _ := prune.PropAny("calls_in_loop").([]string); len(inLoop) != 1 || !strings.HasSuffix(inLoop[0], "ILibraryManager.DeleteItem") {
		t.Errorf("Prune calls_in_loop = %v, want the resolved method", inLoop)
	}
	if !csPerformsIO(prune) {
		t.Errorf("Prune is not flagged: provider, interface, implementation, File.Delete")
	}
	if csPerformsIO(csSym(t, ff, "Provider.Titles")) {
		t.Errorf("Titles is flagged: Describe returns a name")
	}
	if _, kept := prune.Prop("typed_calls"); kept {
		t.Errorf("the walker's note leaked into the facts")
	}
}

// Every way C# declares what a receiver is: a property (named for its type, as
// they are), a primary-constructor parameter, a method parameter, a typed local,
// `var x = new T()`.
func TestCSharpDeclaredReceiverForms(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Lib.Core/ILibraryManager.cs": csLibraryFixture,
		"Lib.Impl/LibraryManager.cs":  csLibraryImplFixture,
		"Lib.Tv/Forms.cs": `namespace Lib.Tv;

using Lib.Core;
using Lib.Impl;

public class ByProperty
{
    public ILibraryManager LibraryManager { get; set; }

    public void Run(Item item) { LibraryManager.DeleteItem(item); }
}

public class ByPrimary(ILibraryManager library)
{
    public void Run(Item item) { library.DeleteItem(item); }
}

public class ByLocals
{
    public void Param(ILibraryManager library, Item item) { library.DeleteItem(item); }

    public void Typed(Item item)
    {
        LibraryManager manager = Create();
        manager.DeleteItem(item);
    }

    public void Created(Item item)
    {
        var manager = new LibraryManager();
        manager.DeleteItem(item);
    }

    public void Unknown(Item item)
    {
        var manager = Create();
        manager.DeleteItem(item);
    }
}
`,
	})
	for suffix, want := range map[string]string{
		"ByProperty.Run":   "ILibraryManager.DeleteItem",
		"ByPrimary.Run":    "ILibraryManager.DeleteItem",
		"ByLocals.Param":   "ILibraryManager.DeleteItem",
		"ByLocals.Typed":   "Lib.Impl.LibraryManager.DeleteItem",
		"ByLocals.Created": "Lib.Impl.LibraryManager.DeleteItem",
	} {
		if f := csSym(t, ff, suffix); !csCallsSuffix(f, want) {
			t.Errorf("%s calls %v, want %s", suffix, csCalls(f), want)
		}
	}
	// `var` from a call says nothing about the type, and nothing is made of it.
	if f := csSym(t, ff, "ByLocals.Unknown"); csCallsSuffix(f, "LibraryManager.DeleteItem") {
		t.Errorf("Unknown calls %v: the receiver's type is not declared", csCalls(f))
	}
}

// A method the receiver's type inherits is an edge to where it is declared, and a
// call on a type this repository does not declare is not an edge at all.
func TestCSharpTypedCallLandsOnADeclarationOrIsNotMade(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Lib.Core/Stores.cs": `namespace Lib.Core;

public abstract class StoreBase
{
    public void Flush() { }
}

public class DiskStore : StoreBase
{
    public void Write(string s) { }
}
`,
		"Lib.Tv/Job.cs": `namespace Lib.Tv;

using System.Text;
using Lib.Core;

public class Job
{
    private readonly DiskStore _store = new DiskStore();
    private readonly StringBuilder _text = new StringBuilder();

    public void Run(string s)
    {
        _store.Write(s);
        _store.Flush();
        _text.Append(s);
    }
}
`,
	})
	run := csSym(t, ff, "Job.Run")
	for _, want := range []string{"DiskStore.Write", "StoreBase.Flush"} {
		if !csCallsSuffix(run, want) {
			t.Errorf("Job.Run calls %v, want %s", csCalls(run), want)
		}
	}
	if csCallsSuffix(run, "StringBuilder.Append") {
		t.Errorf("Job.Run has an edge to a type this repository does not declare: %v", csCalls(run))
	}
}
