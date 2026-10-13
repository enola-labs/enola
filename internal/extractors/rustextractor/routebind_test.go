package rustextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// rustRouteHandlers extracts a repository written from files and returns
// "<METHOD> <path>" -> the route's handled_by target, "" when it has none.
func rustRouteHandlers(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	repo := t.TempDir()
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		abs := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		target := ""
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				target = r.Target
			}
		}
		out[f.PropString("method")+" "+f.Name] = target
	}
	return out
}

func wantRustHandlers(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, target := range want {
		have, ok := got[key]
		if !ok {
			t.Errorf("no route %q; have %v", key, got)
			continue
		}
		if have != target {
			t.Errorf("%s handled_by = %q, want %q", key, have, target)
		}
	}
}

const cargoToml = "[package]\nname = \"server\"\n"

// A bare handler name is the function of the route's own file. A path names the
// function in the module the path ends in, whichever other function shares its name.
func TestAxumHandler_BareNameAndModulePath(t *testing.T) {
	got := rustRouteHandlers(t, map[string]string{
		"Cargo.toml": cargoToml,
		"src/server.rs": `
pub fn router() -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/users", get(handlers::users::list).post(handlers::users::create))
        .route("/admin", get(admin::list))
        .route("/closure", get(|| async { "ok" }))
        .route("/nowhere", get(handlers::missing::list))
}

async fn health() -> &'static str { "ok" }
`,
		"src/handlers/users.rs": `
pub async fn list() {}
pub async fn create() {}
`,
		"src/handlers/admin/mod.rs": `
pub async fn list() {}
`,
	})
	wantRustHandlers(t, got, map[string]string{
		"GET /health":  "src.health",
		"GET /users":   "src/handlers.list",
		"POST /users":  "src/handlers.create",
		"GET /admin":   "src/handlers/admin.list",
		"GET /closure": "",
		"GET /nowhere": "",
	})
}

// A bare name the route's file does not declare was imported: it is the only
// function of that name in the crate, and nothing when there are two.
func TestAxumHandler_ImportedBareName(t *testing.T) {
	got := rustRouteHandlers(t, map[string]string{
		"Cargo.toml": cargoToml,
		"src/server.rs": `
use crate::handlers::users::list_users;
use crate::handlers::things::twin;

pub fn router() -> Router {
    Router::new()
        .route("/users", get(list_users))
        .route("/twin", get(twin))
}
`,
		"src/handlers/users.rs":  "pub async fn list_users() {}\n",
		"src/handlers/things.rs": "pub async fn twin() {}\n",
		"src/other/things.rs":    "pub async fn twin() {}\n",
	})
	wantRustHandlers(t, got, map[string]string{
		"GET /users": "src/handlers.list_users",
		"GET /twin":  "",
	})
}

// A utoipa route is served by the function its attribute sits on.
func TestUtoipaHandler_FunctionUnderTheAttribute(t *testing.T) {
	got := rustRouteHandlers(t, map[string]string{
		"Cargo.toml": cargoToml,
		"src/controllers/krate/publish.rs": `
#[utoipa::path(
    put,
    path = "/api/v1/crates/new",
    tag = "publish",
)]
pub async fn publish(app: AppState) -> AppResult<Json<GoodCrate>> {
    todo!()
}
`,
	})
	wantRustHandlers(t, got, map[string]string{
		"PUT /api/v1/crates/new": "src/controllers/krate.publish",
	})
}

// A router a test builds inside a #[cfg(test)] module is not a route of the program.
func TestAxumRoutes_TestModuleRouterIsNotARoute(t *testing.T) {
	got := rustRouteHandlers(t, map[string]string{
		"Cargo.toml": cargoToml,
		"src/routers/cognify.rs": `
pub fn router() -> Router {
    Router::new().route("/cognify", post(post_cognify))
}

async fn post_cognify() {}

#[cfg(test)]
mod tests {
    use super::*;

    async fn post_cognify_no_auth() {}

    #[tokio::test]
    async fn empty_body() {
        let app = Router::new().route("/", post(post_cognify_no_auth));
    }
}
`,
	})
	wantRustHandlers(t, got, map[string]string{"POST /cognify": "src/routers.post_cognify"})
	if _, found := got["POST /"]; found {
		t.Errorf("the test module's router was emitted as a route: %v", got)
	}
}
