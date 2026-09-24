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

// The layout of FastAPI's "Bigger Applications" tutorial: routers imported as
// submodules through relative imports, one mounted with a prefix, and a
// dependency imported two packages up.
func TestRouteForms_RelativeSubmoduleMount(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/__init__.py":          "",
		"app/routers/__init__.py":  "",
		"app/internal/__init__.py": "",
		"app/dependencies.py":      "async def get_token_header(): ...\n",
		"app/routers/items.py": `from fastapi import APIRouter, Depends
from ..dependencies import get_token_header
router = APIRouter(prefix="/items", dependencies=[Depends(get_token_header)])

@router.get("/{item_id}")
async def read_item(item_id: str): ...
`,
		"app/internal/admin.py": `from fastapi import APIRouter
router = APIRouter()

@router.post("/")
async def update_admin(): ...
`,
		"app/main.py": `from fastapi import FastAPI
from .internal import admin
from .routers import items
app = FastAPI()
app.include_router(items.router)
app.include_router(admin.router, prefix="/admin")
`,
	})
	wantRoutes(t, ff, "GET /items/{item_id}", "POST /admin")
	found := false
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Target == "app/dependencies.get_token_header" {
				found = true
			}
			if r.Target == "app/routers/dependencies.get_token_header" {
				t.Errorf("`from ..dependencies` resolved one package too shallow: %s", r.Target)
			}
		}
	}
	if !found {
		t.Error("no reference reaches app/dependencies.get_token_header")
	}
}

func TestRouteForms_ComputedMountPrefixes(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/__init__.py":      "",
		"app/core/__init__.py": "",
		"app/core/config.py": `from pydantic_settings import BaseSettings
API_PREFIX = "/api"

class Settings(BaseSettings):
    API_V1_STR: str = "/api/v1"

settings = Settings()
`,
		"app/users.py": `from fastapi import APIRouter
router = APIRouter()

@router.get("/users")
def list_users(): ...
`,
		"app/api.py": `from fastapi import APIRouter
from app import users
from app.core.config import API_PREFIX
api_router = APIRouter(prefix=API_PREFIX + "/v2")
api_router.include_router(users.router)
`,
		"app/main.py": `from fastapi import FastAPI
from app import users
from app.api import api_router
from app.core.config import settings
app = FastAPI()
app.include_router(users.router, prefix=settings.API_V1_STR)
app.include_router(users.router, prefix="/legacy")
app.include_router(api_router)
`,
	})
	wantRoutes(t, ff, "GET /api/v1/users", "GET /legacy/users", "GET /api/v2/users")
}

// A prefix held in a factory's parameter is not the module constant of the same
// name; it stays unresolved, which leaves the mount without a prefix.
func TestRouteForms_LocalPrefixIsNotAConstant(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/main.py": `from fastapi import FastAPI, APIRouter
prefix = "/wrong"
router = APIRouter()

@router.get("/x")
def x(): ...

def create_app(prefix):
    app = FastAPI()
    app.include_router(router, prefix=prefix)
    return app
`,
	})
	wantRoutes(t, ff, "GET /x")
}

// `router = _build_router()` in a package __init__ names the router its factory
// builds; mounting the package's router applies the mount prefix to it.
func TestRouteForms_FactoryAliasInPackage(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/__init__.py":         "",
		"app/routers/__init__.py": "",
		"app/routers/account/__init__.py": `from fastapi import APIRouter
from . import user

def _build_router() -> APIRouter:
    rt = APIRouter()
    rt.include_router(user.router, prefix="/user")
    return rt

router = _build_router()
`,
		"app/routers/account/user.py": `from fastapi import APIRouter
router = APIRouter()

@router.post("")
def create(): ...
`,
		"app/main.py": `from fastapi import FastAPI
from app.routers import account
app = FastAPI()
app.include_router(account.router, prefix="/account")
`,
	})
	wantRoutes(t, ff, "POST /account/user")
}
