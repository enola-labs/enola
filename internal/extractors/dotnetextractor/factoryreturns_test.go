package dotnetextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCSharpCallOnAMethodsResultIsAnEdge(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"App.csproj": `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"Engine/Engine.cs": `namespace Acme.Engine;
public class Engine {
  public int Run() { return Step(); }
  public int Step() { return 1; }
  public Session Session() { return new Session(); }
}
public class Session { public void Commit() {} }
`,
		"Engine/EngineFactory.cs": `using System.Threading.Tasks;
namespace Acme.Engine;
public static class EngineFactory {
  public static Engine Make() { return new Engine(); }
  public static async Task<Engine> MakeAsync() { return new Engine(); }
}
`,
		"Api/Api.cs": `using System.Threading.Tasks;
using Acme.Engine;
namespace Acme.Api;
public class Api {
  public int ViaVar() { var e = EngineFactory.Make(); return e.Run(); }
  public async Task<int> ViaAwait() { var e = await EngineFactory.MakeAsync(); return e.Run(); }
  public void Chained() { EngineFactory.Make().Session().Commit(); }
  public int Own() { var e = Local(); return e.Run(); }
  private Engine Local() { return EngineFactory.Make(); }
  public int NotAwaited() { var t = EngineFactory.MakeAsync(); return t.Run(); }
}
`,
	})
	calls := func(suffix string) []string {
		for _, f := range ff {
			if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
				var out []string
				for _, r := range f.Relations {
					if r.Kind == facts.RelCalls {
						out = append(out, r.Target)
					}
				}
				return out
			}
		}
		t.Fatalf("no symbol ending in %s", suffix)
		return nil
	}
	has := func(list []string, target string) bool {
		for _, c := range list {
			if c == target {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"Api.ViaVar", "Api.ViaAwait", "Api.Own"} {
		if got := calls(name); !has(got, "Engine.Engine.Run") {
			t.Errorf("%s must call Engine.Run; calls: %v", name, got)
		}
	}
	if got := calls("Api.Chained"); !has(got, "Engine.Session.Commit") {
		t.Errorf("a chain is typed step by step; calls: %v", got)
	}
	if got := calls("Api.NotAwaited"); has(got, "Engine.Engine.Run") {
		t.Errorf("a Task<Engine> not awaited is no Engine; calls: %v", got)
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil || f.PropAny(propAwaitedType) != nil {
			t.Errorf("%s keeps a return-type prop, which must not leave the extractor", f.Name)
		}
	}
}
