package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/facts"
)

func intp(i int) *int { return &i }

// sdkClient is the declared client these tests read through: sendRequest(service,
// path, options) on an injected IHttpRequestService.
func sdkClient() []clientspec.Spec {
	specs := []clientspec.Spec{{
		Name:          "sdk-http",
		Language:      "typescript",
		ReceiverTypes: []string{"IHttpRequestService"},
		Methods: []clientspec.Method{{
			Name:       "sendRequest",
			ServiceArg: intp(0),
			PathArg:    intp(1),
			OptionsArg: intp(2),
		}},
	}}
	clientspec.Normalize(specs)
	return specs
}

func extractWithClients(t *testing.T, specs []clientspec.Spec, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	all := map[string]string{"package.json": `{"name": "svc"}`, "tsconfig.json": "{}"}
	for name, body := range files {
		all[name] = body
	}
	var rel []string
	for name, body := range all {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(name) == ".ts" {
			rel = append(rel, name)
		}
	}
	sort.Strings(rel)

	e := New()
	e.SetClientSpecs(specs)
	out, err := e.Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return out
}

// configuredRoutes keys the routes read through a declared client by "METHOD path".
func configuredRoutes(fs []facts.Fact) map[string]facts.Fact {
	out := map[string]facts.Fact{}
	for _, f := range fs {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropSource) == facts.RouteSourceConfiguredHTTPClient {
			out[f.PropString("method")+" "+f.Name] = f
		}
	}
	return out
}

func routeKeys(m map[string]facts.Fact) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const connectorSource = `import { Injectable } from "@nestjs/common";
import { GET, HttpMethod, IHttpRequestService } from "../http/http-request.service";

@Injectable()
export class ResourceConnector {
  private readonly serviceName = "resource-api";
  private readonly basePath: string = "/v1/resources/";

  constructor(private readonly httpRequestService: IHttpRequestService) {}

  async getCatalogItems() {
    const url = ` + "`${this.basePath}catalog/items`" + `;
    return this.httpRequestService.sendRequest<string[]>(this.serviceName, url, { method: GET });
  }

  async startImport(body: unknown) {
    await this.httpRequestService.sendRequest<void>(this.serviceName, "/v1/catalog/imports", {
      method: HttpMethod.POST,
      body,
    });
  }

  async getByKey(key: string) {
    return this.httpRequestService.sendRequest<string>(this.serviceName, this.pathFor(key), { method: GET });
  }

  private pathFor(key: string): string {
    return ` + "`/v1/resources/${key}`" + `;
  }
}
`

// The whole reported shape: the path folds through a class field and a local const,
// the verb is a bare imported constant, and the service name is a string field.
func TestConfiguredClient_FoldsFieldAndLocalConst(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/connectors/resource-connector.ts": connectorSource,
	}))

	f, ok := routes["GET /v1/resources/catalog/items"]
	if !ok {
		t.Fatalf("folded request not read; routes: %v", routeKeys(routes))
	}
	if got := f.PropString(facts.PropClientSpec); got != "sdk-http" {
		t.Errorf("client_spec = %q, want sdk-http", got)
	}
	if got := f.PropString("target_hint"); got != "resource-api" {
		t.Errorf("target_hint = %q, want the service name field's value", got)
	}
	if f.PropString(facts.PropRole) != facts.RoleClient {
		t.Errorf("role = %q, want client", f.PropString(facts.PropRole))
	}
}

func TestConfiguredClient_LiteralPathAndEnumMemberVerb(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/connectors/resource-connector.ts": connectorSource,
	}))
	if _, ok := routes["POST /v1/catalog/imports"]; !ok {
		t.Errorf("literal path with HttpMethod.POST not read; routes: %v", routeKeys(routes))
	}
}

// A path that is a method's return value has no text to read. It must produce
// nothing: exactly two routes come out of the connector, never a third guessed one.
func TestConfiguredClient_DynamicPathEmitsNothing(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/connectors/resource-connector.ts": connectorSource,
	}))
	if len(routes) != 2 {
		t.Errorf("want exactly the two derivable routes, got %v", routeKeys(routes))
	}
}

func TestConfiguredClient_UnknownOperandLeadingOrInside(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/items.ts": `export class Items {
  constructor(private readonly http: IHttpRequestService) {}
  byId(id: string, base: string) {
    this.http.sendRequest("svc", ` + "`/v1/items/${id}/stats`" + `, { method: "GET" });
    this.http.sendRequest("svc", ` + "`${base}/v1/items`" + `, { method: "GET" });
    this.http.sendRequest("svc", "/v1/items/" + id, { method: "DELETE" });
  }
}
`,
	}))
	if _, ok := routes["GET /v1/items/{}/stats"]; !ok {
		t.Errorf("an unknown operand inside the path must become {}; routes: %v", routeKeys(routes))
	}
	if _, ok := routes["DELETE /v1/items/{}"]; !ok {
		t.Errorf("a concatenated parameter must become {}; routes: %v", routeKeys(routes))
	}
	for k := range routes {
		if k == "GET /v1/items" {
			t.Errorf("an unknown leading base was stripped instead of refused: %s", k)
		}
	}
	if len(routes) != 2 {
		t.Errorf("want 2 routes, got %v", routeKeys(routes))
	}
}

// The receiver's declared type decides. The same method name, arguments and a real
// path on an unconfigured type is not a request.
func TestConfiguredClient_UnconfiguredTypeIsIgnored(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/bus.ts": `export class MessageBus {
  constructor(private readonly transport: QueueTransport) {}
  publish() {
    return this.transport.sendRequest("imports", "/v1/catalog/imports", { method: "POST" });
  }
}
`,
	}))
	if len(routes) != 0 {
		t.Errorf("a sendRequest on an unconfigured type became a route: %v", routeKeys(routes))
	}
}

// A local of the configured type is not a member the class declared, so it does not
// qualify either: the receiver must be this.<member>.
func TestConfiguredClient_OtherMethodOnConfiguredTypeIsIgnored(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/svc.ts": `export class Svc {
  constructor(private readonly http: IHttpRequestService) {}
  run() {
    this.http.configure("svc", "/v1/not-a-request", {});
  }
}
`,
	}))
	if len(routes) != 0 {
		t.Errorf("an undeclared method on the configured type became a route: %v", routeKeys(routes))
	}
}

func TestConfiguredClient_InjectFieldDialect(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/svc.ts": `export class Svc {
  private readonly http = inject(IHttpRequestService);
  run() {
    return this.http.sendRequest("svc", "/v1/reports", { method: "PUT" });
  }
}
`,
	}))
	if _, ok := routes["PUT /v1/reports"]; !ok {
		t.Errorf("inject() field not read as a receiver; routes: %v", routeKeys(routes))
	}
}

// With no verb in the call, the spec's default applies; with no default either, the
// route states no verb rather than inventing GET.
func TestConfiguredClient_VerbDefaults(t *testing.T) {
	specs := []clientspec.Spec{{
		Name: "a", Language: "typescript", ReceiverTypes: []string{"ClientA"},
		Methods: []clientspec.Method{{Name: "call", PathArg: intp(0), DefaultVerb: "POST"}},
	}, {
		Name: "b", Language: "typescript", ReceiverTypes: []string{"ClientB"},
		Methods: []clientspec.Method{{Name: "call", PathArg: intp(0), OptionsArg: intp(1)}},
	}}
	clientspec.Normalize(specs)
	routes := configuredRoutes(extractWithClients(t, specs, map[string]string{
		"src/svc.ts": `export class Svc {
  constructor(private readonly a: ClientA, private readonly b: ClientB) {}
  run(opts: object) {
    this.a.call("/v1/with-default");
    this.b.call("/v1/shorthand", { method });
  }
}
`,
	}))
	if _, ok := routes["POST /v1/with-default"]; !ok {
		t.Errorf("default_verb not applied; routes: %v", routeKeys(routes))
	}
	if _, ok := routes[facts.MethodAny+" /v1/shorthand"]; !ok {
		t.Errorf("a call stating no verb, with no default, must carry MethodAny; routes: %v", routeKeys(routes))
	}
}

// Two connectors in one file, each with its own basePath. A repository-wide constant
// table would see basePath bound twice and resolve neither; each class's own field is
// the only value its `this.basePath` can mean.
func TestConfiguredClient_FieldResolvesAgainstItsOwnClass(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/connectors.ts": `export class Orders {
  private readonly basePath = "/v1/orders";
  constructor(private readonly http: IHttpRequestService) {}
  list() { return this.http.sendRequest("orders", this.basePath + "/open", { method: "GET" }); }
}
export class Invoices {
  private readonly basePath = "/v2/invoices";
  constructor(private readonly http: IHttpRequestService) {}
  list() { return this.http.sendRequest("billing", this.basePath + "/open", { method: "GET" }); }
}
`,
	}))
	for _, want := range []string{"GET /v1/orders/open", "GET /v2/invoices/open"} {
		if _, ok := routes[want]; !ok {
			t.Errorf("missing %s; routes: %v", want, routeKeys(routes))
		}
	}
}

// A local reassigned after its declaration has no single value, so nothing folds.
func TestConfiguredClient_ReassignedLocalDoesNotFold(t *testing.T) {
	routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{
		"src/svc.ts": `export class Svc {
  constructor(private readonly http: IHttpRequestService) {}
  run(flag: boolean) {
    let url = "/v1/first";
    if (flag) { url = "/v1/second"; }
    return this.http.sendRequest("svc", url, { method: "GET" });
  }
}
`,
	}))
	if len(routes) != 0 {
		t.Errorf("a reassigned local was folded: %v", routeKeys(routes))
	}
}

func TestConfiguredClient_TestFilesAndNoSpecsEmitNothing(t *testing.T) {
	body := `export class Svc {
  constructor(private readonly http: IHttpRequestService) {}
  run() { return this.http.sendRequest("svc", "/v1/reports", { method: "GET" }); }
}
`
	if routes := configuredRoutes(extractWithClients(t, sdkClient(), map[string]string{"src/svc.spec.ts": body})); len(routes) != 0 {
		t.Errorf("a test file's request became a route: %v", routeKeys(routes))
	}
	if routes := configuredRoutes(extractWithClients(t, nil, map[string]string{"src/svc.ts": body})); len(routes) != 0 {
		t.Errorf("routes read with no client declared: %v", routeKeys(routes))
	}
}

// The declared clients decide the extractor's output, so they must reach its cache key,
// and declaring none must leave the key as it was.
func TestConfiguredClient_ConfigKeyFollowsSpecs(t *testing.T) {
	e := New()
	if e.ConfigKey() != "" {
		t.Errorf("no specs must key as nothing, got %q", e.ConfigKey())
	}
	e.SetClientSpecs(sdkClient())
	withSpec := e.ConfigKey()
	if withSpec == "" {
		t.Fatal("declared specs produced an empty config key")
	}
	changed := sdkClient()
	changed[0].Methods[0].PathArg = intp(3)
	e.SetClientSpecs(changed)
	if e.ConfigKey() == withSpec {
		t.Error("changing a spec did not change the config key")
	}
}
