package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestRouteForms_APIRouteAndWebsocket(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/api.py": `from fastapi import APIRouter
router = APIRouter()

@router.api_route("/items", methods=["GET", "POST"])
def items(): ...

@router.api_route("/tuple", methods=("PUT",))
def tup(): ...

@router.api_route("/default")
def default(): ...

@router.websocket("/ws")
async def ws(websocket): ...

@router.websocket_route("/ws2")
async def ws2(websocket): ...
`,
	})
	wantRoutes(t, ff, "GET /items", "POST /items", "PUT /tuple", "GET /default", "GET /ws", "GET /ws2")
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		ws := f.Name == "/ws" || f.Name == "/ws2"
		if got := f.Props["protocol"] == "websocket"; got != ws {
			t.Errorf("%s: protocol=websocket is %v, want %v", f.Name, got, ws)
		}
	}
}

func TestRouteForms_AddAPIRouteCalls(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/controller.py": `from fastapi import APIRouter

class UserController:
    def __init__(self, service):
        self.service = service
        self.router = APIRouter(prefix="/users")
        self.router.add_api_route("/{user_id}", self.get_user, methods=["GET"])
        self.router.add_api_websocket_route("/events", self.events)

    def get_user(self, user_id): ...
    async def events(self, ws): ...

router = APIRouter()

def health(): ...

router.add_api_route("/health", health)
router.add_api_route(path="/ready", endpoint=health, methods=["HEAD"])
`,
	})
	wantRoutes(t, ff, "GET /users/{user_id}", "GET /users/events", "GET /health", "HEAD /ready")
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/users/{user_id}" {
			if h := f.Props["handler"]; h != "app/controller.UserController.get_user" {
				t.Errorf("handler = %v", h)
			}
		}
	}
}

// aiohttp's router.add_route("GET", "/x", h) and unrelated add_route methods do
// not register a FastAPI/Starlette route: the path must read as one.
func TestRouteForms_AddRouteNeedsAPath(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/web.py": `from starlette.applications import Starlette
app = Starlette()
app.add_route("/home", home, methods=["GET"])
web_app.router.add_route("GET", "/aiohttp", handler)
graph.add_route("a", "b")
`,
	})
	wantRoutes(t, ff, "GET /home")
}

func TestRouteForms_GuardedDefinitions(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/api.py": `from typing import TYPE_CHECKING
from fastapi import APIRouter
router = APIRouter()

if settings.DEBUG:
    @router.get("/debug")
    def debug(): ...

try:
    @router.get("/optional")
    def optional(): ...
except ImportError:
    pass

if TYPE_CHECKING:
    @router.get("/never")
    def never(): ...

if sys.platform == "darwin":
    def shim(): ...
`,
	})
	wantRoutes(t, ff, "GET /debug", "GET /optional")
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "app/api.shim" {
			t.Errorf("a guarded def without a route decorator must stay symbol-less")
		}
	}
}

func TestRouteForms_ConstantPaths(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/__init__.py":      "",
		"app/core/__init__.py": `ROOT = "/root"`,
		"app/core/paths.py": `from typing import Final
API: Final = "/api"
USERS = API + "/users"

class Routes:
    ITEMS = "/items"
    ITEM = f"{ITEMS}/{{item_id}}"
`,
		"app/api.py": `from fastapi import APIRouter
from app.core.paths import USERS, Routes
from app.core import paths, ROOT
router = APIRouter()
LOCAL = "/local"

@router.get(USERS)
def users(): ...

@router.get(Routes.ITEM)
def item(): ...

@router.post(paths.API + "/login")
def login(): ...

@router.get(f"{LOCAL}/x")
def local(): ...

@router.get(ROOT)
def root(): ...

@router.get(UNDEFINED)
def unresolved(): ...

@router.get(build_path())
def computed(): ...

@router.get(f"{paths.API}/{version:>3}")
def formatted(): ...
`,
	})
	wantRoutes(t, ff, "GET /api/users", "GET /items/{item_id}", "POST /api/login", "GET /local/x", "GET /root")
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props[pathPendingProp] != nil {
			t.Errorf("%s keeps %s", f.Name, pathPendingProp)
		}
	}
}

// A name bound by the enclosing function is a parameter or a local, never a
// constant, even when a module-level constant shares its name.
func TestRouteForms_LocalNameIsNotAConstant(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/server.py": `from fastapi import FastAPI
path = "/module-level"

def create_app(path):
    app = FastAPI()

    @app.get(path)
    def h(): ...
    return app
`,
	})
	wantRoutes(t, ff)
}

// A resolved constant path still takes the include_router mount prefix.
func TestRouteForms_ConstantPathComposesWithMount(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/users.py": `from fastapi import APIRouter
LIST = "/list"
router = APIRouter()

@router.get(LIST)
def list_users(): ...
`,
		"app/main.py": `from fastapi import FastAPI
from app import users
app = FastAPI()
app.include_router(users.router, prefix="/users")
`,
	})
	wantRoutes(t, ff, "GET /users/list")
}

// A value that is not a URL path is a constant that happens to share the name.
func TestRouteForms_NonPathConstantIsDropped(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/api.py": `from fastapi import APIRouter
router = APIRouter()
NAME = "users"

@router.get(NAME)
def h(): ...
`,
	})
	wantRoutes(t, ff)
}
