package dotnetextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A C# declaration's doc is the <summary> of its XML doc comment, read above its
// attributes.
func TestDoc_XMLDocSummary(t *testing.T) {
	repo := t.TempDir()
	rel := "src/Api/UserController.cs"
	src := `namespace Jellyfin.Api.Controllers;

/// <summary>
/// User controller.
/// </summary>
[Route("Users")]
public class UserController : BaseJellyfinApiController
{
    /// <summary>
    /// Authenticates a user by name, see <see cref="IUserManager"/>.
    /// </summary>
    /// <param name="request">The request.</param>
    [HttpPost("AuthenticateByName")]
    public ActionResult AuthenticateUserByName(AuthenticateUserByName request) { return null; }

    public ActionResult Logout() { return null; }
}
`
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ff, err := New().Extract(context.Background(), repo, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			got[facts.ShortName(f.Name)] = f.PropString(facts.PropDoc)
		}
	}
	for name, doc := range map[string]string{
		"UserController":         "User controller.",
		"AuthenticateUserByName": "Authenticates a user by name, see IUserManager.",
		"Logout":                 "",
	} {
		if got[name] != doc {
			t.Errorf("%s doc = %q, want %q (have %v)", name, got[name], doc, got)
		}
	}
}
