package dotnetextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The selection keeps the first branch of each chain, blanks every other byte of the
// chain including the directives, and keeps src's length and line breaks.
func TestActiveBranches(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"first branch kept", "a\n#if NET\nb\n#else\nc\n#endif\nd\n", "a\nb\nd\n"},
		{"elif and else dropped", "#if A\nx\n#elif B\ny\n#else\nz\n#endif\n", "x\n"},
		{"literal false falls through", "#if false\nx\n#else\ny\n#endif\n", "y\n"},
		{"nested keeps first of each", "#if A\n#if B\np\n#else\nq\n#endif\n#else\nr\n#endif\n", "p\n"},
		{"inside a skipped branch everything goes", "#if A\nk\n#else\n#if B\nm\n#endif\nn\n#endif\n", "k\n"},
		{"other directives stay", "#region R\n#if A\nx\n#endif\n#endregion\n", "#region R\nx\n#endregion\n"},
		{"indented directive", "  #if A\n  x\n  #else\n  y\n  #endif\n", "x\n"},
		{"verbatim string is not a directive", "var s = @\"\n#if A\n\";\n#if B\nz\n#else\nw\n#endif\n", "var s = @\"\n#if A\n\";\nz\n"},
		{"raw string is not a directive", "var s = \"\"\"\n#else\n\"\"\";\n", "var s = \"\"\"\n#else\n\"\"\";\n"},
		{"block comment is not a directive", "/*\n#if A\n*/\nx\n", "/*\n#if A\n*/\nx\n"},
		{"no directive, same slice", "class C { }\n", "class C { }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(activeBranches([]byte(tc.src)))
			if len(got) != len(tc.src) || strings.Count(got, "\n") != strings.Count(tc.src, "\n") {
				t.Fatalf("layout changed: %d bytes/%d lines, want %d/%d", len(got), strings.Count(got, "\n"), len(tc.src), strings.Count(tc.src, "\n"))
			}
			if squash(got) != tc.want {
				t.Errorf("got %q, want %q", squash(got), tc.want)
			}
		})
	}
}

// squash drops the blank padding so a case can state the surviving text.
func squash(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Directive placements from runtime, roslyn, avalonia and MudBlazor files that lost
// their symbols. The base-list case turns the file into an ERROR node on its own; the
// others did so only inside their full files, where error recovery cascades, and are
// kept as guards that the selected configuration still yields every member.
func TestPreprocessorPlacementsKeepTheFile(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"#if inside a base list", `namespace N;
public partial class LinkContext
#if !ILTRIM
    : IMetadataResolver, IDisposable
#endif
{
    public void Dispose() { }
}
`, []string{"src.LinkContext", "src.LinkContext.Dispose"}},
		{"#if around a member's attributes", `namespace System.Data.OleDb
{
    public sealed partial class OleDbException : System.Data.Common.DbException
    {
        internal OleDbException() { }
        [System.ComponentModel.DesignerSerializationVisibilityAttribute(System.ComponentModel.DesignerSerializationVisibility.Content)]
        public System.Data.OleDb.OleDbErrorCollection Errors { get { throw null; } }
#if NET
        [System.ObsoleteAttribute("This API supports obsolete formatter-based serialization.")]
        [System.ComponentModel.EditorBrowsableAttribute(System.ComponentModel.EditorBrowsableState.Never)]
#endif
        public override void GetObjectData(System.Runtime.Serialization.SerializationInfo si, System.Runtime.Serialization.StreamingContext context) { }
    }
    public sealed partial class OleDbInfoMessageEventArgs : System.EventArgs
    {
        public int ErrorCode { get { throw null; } }
    }
}
`, []string{"src.OleDbException.GetObjectData", "src.OleDbInfoMessageEventArgs.ErrorCode"}},
		{"#if splitting a local function's expression body", `namespace Avalonia.iOS;

internal sealed class InputHandler
{
    public bool Handle(UIEvent? evt)
    {
        var point = new RawPointerPoint
        {
            Pressure = 0.5f
        };

        bool IsRightClick()
#if !TVOS
            => OperatingSystem.IsIOSVersionAtLeast(13, 4) && (evt?.ButtonMask.HasFlag(UIEventButtonMask.Secondary) ?? false);
#else
            => false;
#endif
        return IsRightClick();
    }

    public bool After() => true;
}
`, []string{"src.InputHandler.Handle", "src.InputHandler.After"}},
		{"#if around a catch clause followed by more catches", `namespace MudBlazor;

public static class IJSRuntimeExtensions
{
    public static async ValueTask InvokeVoidAsyncIgnoreErrors(this IJSRuntime jsRuntime, string identifier, params object?[] args)
    {
        try
        {
            await jsRuntime.InvokeVoidAsync(identifier, args);
        }
#if !DEBUG
        catch (JSException)
        {
        }
#endif
        catch (InvalidOperationException) when (IsUnsupportedJavaScriptRuntime(jsRuntime))
        {
        }
        catch (JSDisconnectedException)
        {
        }
    }

    public static bool IsUnsupportedJavaScriptRuntime(IJSRuntime jsRuntime) => false;
}
`, []string{"src.IJSRuntimeExtensions.InvokeVoidAsyncIgnoreErrors", "src.IJSRuntimeExtensions.IsUnsupportedJavaScriptRuntime"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := extractFileAST([]byte(tc.src), "src/F.cs")
			for _, want := range tc.want {
				if factByName(ff, want) == nil {
					t.Errorf("missing %s; got %v", want, names(ff, facts.KindSymbol))
				}
			}
		})
	}
}
