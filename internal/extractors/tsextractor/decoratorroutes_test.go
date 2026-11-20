package tsextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// serverRoutes indexes emitted server-role routes by path -> method.
func serverRoutes(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["role"] == "server" {
			out[f.Name] = f.Props["method"].(string)
		}
	}
	return out
}

// extractTS runs a one-file project through the real extractor, so these tests
// exercise the class walk in ts.go and the IsTestPath gate — not decoratorRouteFacts
// in isolation. The wiring is half the fix.
func extractTS(t *testing.T, src, relFile string) []facts.Fact {
	t.Helper()
	return extractAll(t, map[string]string{relFile: src}, false)
}

// TestDecoratorRoutes_NestObjectForm covers the argument form that dominates real
// NestJS code: @Controller({path: …}). A bare-string-only implementation scores
// almost nothing on a real codebase: on one production NestJS API, 37 of 38
// controllers use the object form.
func TestDecoratorRoutes_NestObjectForm(t *testing.T) {
	src := `
@Controller({
  path: "/v2/slots",
  version: VERSION_2024_04_15,
})
export class SlotsController {
  @Get("/available")
  async getAvailableSlots() {}

  @Post("/reserve")
  async reserveSlot() {}

  @Delete("/selected-slot")
  async deleteSelectedSlot() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/slots/slots.controller.ts"))
	for path, want := range map[string]string{
		"/v2/slots/available":     "GET",
		"/v2/slots/reserve":       "POST",
		"/v2/slots/selected-slot": "DELETE",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	// `version:` is NOT a URL segment — NestJS versioning may be header- or
	// media-type-based, and the decorator does not say which.
	for p := range got {
		if p == "/v2/slots/VERSION_2024_04_15/available" {
			t.Errorf("version composed into the path: %+v", got)
		}
	}
}

// TestDecoratorRoutes_NestStringFormAndBareVerb covers @Controller("/users") and a
// verb decorator with no argument, which serves the class path itself.
func TestDecoratorRoutes_NestStringFormAndBareVerb(t *testing.T) {
	src := `
@Controller("/users")
export class UsersController {
  @Get()
  findAll() {}

  @Get(":id")
  findOne() {}

  @Patch("/:id/profile")
  update() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/users/users.controller.ts"))
	for path, want := range map[string]string{
		"/users":             "GET",
		"/users/:id":         "GET",
		"/users/:id/profile": "PATCH",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
}

// TestDecoratorRoutes_Inversify covers the second supported vocabulary: lowercase
// @controller + @httpGet-prefixed verbs.
func TestDecoratorRoutes_Inversify(t *testing.T) {
	src := `
@controller("/api/orders")
export class OrderController {
  @httpGet("/")
  async list() {}

  @httpPost("/:id/cancel")
  async cancel() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/orders/order.controller.ts"))
	if got["/api/orders"] != "GET" {
		t.Errorf("httpGet('/') should serve the class path: %+v", got)
	}
	if got["/api/orders/:id/cancel"] != "POST" {
		t.Errorf("httpPost: %+v", got)
	}
}

// TestDecoratorRoutes_RequiresControllerDecorator is the precision gate. @Get is a
// generic name and Inversify's vocabulary is generic enough to appear on ordinary
// classes; only a class that declares itself a controller may mint routes.
func TestDecoratorRoutes_RequiresControllerDecorator(t *testing.T) {
	src := `
@Injectable()
export class SlotsService {
  @Get("/available")
  async notARoute() {}
}

export class PlainClass {
  @httpGet("/nope")
  alsoNotARoute() {}
}
`
	if got := serverRoutes(extractTS(t, src, "src/slots/slots.service.ts")); len(got) != 0 {
		t.Errorf("verb decorators outside a controller class must not emit routes: %+v", got)
	}
}

// TestDecoratorRoutes_VocabulariesDoNotMix guards the per-framework verb maps: an
// Inversify verb inside a NestJS controller is not a route NestJS would serve.
func TestDecoratorRoutes_VocabulariesDoNotMix(t *testing.T) {
	src := `
@Controller("/users")
export class UsersController {
  @httpGet("/wrong-vocabulary")
  stray() {}

  @Get("/right")
  ok() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/users/users.controller.ts"))
	if _, found := got["/users/wrong-vocabulary"]; found {
		t.Errorf("inversify verb inside a NestJS controller must not emit: %+v", got)
	}
	if got["/users/right"] != "GET" {
		t.Errorf("NestJS verb should still emit: %+v", got)
	}
}

// TestDecoratorRoutes_DecoratorsDoNotCarryAcrossMembers pins the ordered-walk
// bookkeeping: member decorators are siblings preceding their method, so a run must
// be flushed at every member — otherwise a field's decorators would be attributed to
// the next method down.
func TestDecoratorRoutes_DecoratorsDoNotCarryAcrossMembers(t *testing.T) {
	src := `
@Controller("/users")
export class UsersController {
  @Get("/first")
  first() {}

  @Inject(TOKEN)
  private readonly dep: Dep;

  second() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/users/users.controller.ts"))
	if len(got) != 1 || got["/users/first"] != "GET" {
		t.Errorf("exactly one route expected, on first(): %+v", got)
	}
}

// TestDecoratorRoutes_CommentBetweenDecoratorAndMethod pins a miss found by
// measuring against a real backend rather than by unit testing: a JSDoc block sits
// between a handler's decorators and its signature often enough that treating a
// comment as a class member — and so as the end of a decorator run — silently
// dropped two live routes on a real NestJS API. Comments and the unnamed children of
// class_body (braces, stray semicolons) are transparent.
func TestDecoratorRoutes_CommentBetweenDecoratorAndMethod(t *testing.T) {
	src := `
@Controller("/stripe")
export class StripeController {
  @Get("/save")
  @UseGuards()
  @Redirect(undefined, 301)
  /**
   * Handles saving credentials.
   * Proxied so route guards still run.
   */
  async save() {}

  // A line comment is transparent too.
  @Get("/check")
  async check() {}
}
`
	got := serverRoutes(extractTS(t, src, "src/stripe/stripe.controller.ts"))
	for path, want := range map[string]string{
		"/stripe/save":  "GET",
		"/stripe/check": "GET",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
}

// TestDecoratorRoutes_TestFileEmitsNothing mirrors the v141 client-side gate. A
// controller in an e2e fixture would otherwise become a server route no production
// client calls — a false unused-route finding.
func TestDecoratorRoutes_TestFileEmitsNothing(t *testing.T) {
	src := `
@Controller("/users")
export class FixtureController {
  @Get("/x")
  x() {}
}
`
	for _, f := range []string{
		"src/users/users.controller.e2e-spec.ts",
		"src/users/e2e/users.e2e.ts",
		"src/users/users.controller.spec.ts",
	} {
		if got := serverRoutes(extractTS(t, src, f)); len(got) != 0 {
			t.Errorf("%s: test-file controller must emit no routes: %+v", f, got)
		}
	}
}

// A route records the API versions it serves as the source writes them: the
// controller's `version` (a constant, a literal or an array), replaced by a handler's
// own @Version. A controller with none records none.
func TestDecoratorRoutes_NestAPIVersions(t *testing.T) {
	src := `
@Controller({ path: "/v2/event-types", version: [VERSION_2024_06_11, VERSION_2024_04_15, VERSION_2024_04_15] })
export class EventTypesController {
  @Get("/")
  async list() {}

  @Version("3")
  @Delete("/:id")
  async remove() {}
}

@Controller("/health")
export class HealthController {
  @Get("/")
  async ok() {}
}
`
	got := map[string][]string{}
	for _, f := range extractTS(t, src, "src/event-types.controller.ts") {
		if f.Kind == facts.KindRoute {
			got[f.PropString("method")+" "+f.Name] = f.PropStrings(facts.PropAPIVersions)
		}
	}
	want := map[string]string{
		"GET /v2/event-types":        "VERSION_2024_04_15,VERSION_2024_06_11",
		"DELETE /v2/event-types/:id": "3",
		"GET /health":                "",
	}
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("no route %s; got %v", k, got)
			continue
		}
		if s := strings.Join(v, ","); s != w {
			t.Errorf("%s: api_versions = %q, want %q", k, s, w)
		}
	}
}
