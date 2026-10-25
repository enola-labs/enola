package swiftextractor

import (
	"path/filepath"
	"testing"
)

// testModuleForFile is the module-identity resolver used by stored-method endpoint
// tests: it maps a file to its leaf directory (the fallback the real resolver uses
// for loose sources).
func testModuleForFile(f string) string { return filepath.ToSlash(filepath.Dir(f)) }

func TestExtractEndpointFacts(t *testing.T) {
	// An endpoint-enum type (Moya TargetType-like idiom): path + prefix + method are
	// per-case computed properties driven by `switch self`. Uses neutral names.
	src := `import Foundation

extension APIService {
   enum Reports { case send([String: Any]); case list; case sourceType }
}

extension APIService.Reports: APIEndpoint {
   var urlPrefixComponent: String {
      switch self {
      case .send: return "v2"
      case .list, .sourceType: return "public/v1"
      }
   }
   var urlPathComponent: String {
      switch self {
      case .send: return "reports.json"
      case .list: return "report_reasons.json"
      case .sourceType: return "report_source_types.json"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .send: return .post
      case .list, .sourceType: return .get
      }
   }
}
`
	// defaultPrefix "v2" is present but must be ignored — this type defines its own
	// per-case prefix.
	ff := extractEndpointFacts([]byte(src), "Sources/Core/APIService.Reports.swift", "Sources/Core", "v2")
	got := map[string]string{}
	for _, f := range ff {
		if f.Props["role"] != "client" || f.Props["source"] != "swift-endpoint" {
			t.Errorf("%s wrong props: %+v", f.Name, f.Props)
		}
		got[f.Name] = f.Props["method"].(string)
	}
	if got["v2/reports.json"] != "POST" {
		t.Errorf("send: want v2/reports.json POST, got %+v", got)
	}
	if got["public/v1/report_reasons.json"] != "GET" {
		t.Errorf("list: want public/v1/report_reasons.json GET, got %+v", got)
	}
	if got["public/v1/report_source_types.json"] != "GET" {
		t.Errorf("sourceType: want GET, got %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 endpoints, got %d: %+v", len(got), got)
	}
}

// TestExtractEndpointFacts_Interpolated covers interpolated paths (associated
// values) collapsing to the {} placeholder.
func TestExtractEndpointFacts_Interpolated(t *testing.T) {
	src := `extension APIService.Group: APIEndpoint {
   var urlPathComponent: String {
      switch self {
      case .memberships(let groupId, let userId):
         return "groups/\(groupId)/members/\(userId).json"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .memberships: return .delete
      }
   }
}
`
	// No prefix property and empty defaultPrefix -> path stays base-relative.
	ff := extractEndpointFacts([]byte(src), "Sources/Core/APIService.Group.swift", "Sources/Core", "")
	if len(ff) != 1 || ff[0].Name != "groups/{}/members/{}.json" || ff[0].Props["method"] != "DELETE" {
		t.Fatalf("interpolated endpoint mismatch: %+v", ff)
	}
}

// TestExtractEndpointFacts_NotAnEndpointType ensures ordinary types with a `path`
// or `method` alone are not treated as endpoint enums.
func TestExtractEndpointFacts_NotAnEndpointType(t *testing.T) {
	src := `struct FileHelper {
   var path: String { return "/tmp/cache" }
}
`
	if ff := extractEndpointFacts([]byte(src), "Sources/Core/FileHelper.swift", "Sources/Core", "v2"); len(ff) != 0 {
		t.Errorf("a type with a path but no method must not be an endpoint: %+v", ff)
	}
}

// TestExtractEndpointFacts_DefaultPrefix: a type with no urlPrefixComponent gets
// the repo-wide default prefix applied to every case.
func TestExtractEndpointFacts_DefaultPrefix(t *testing.T) {
	src := `extension APIService.Device: APIEndpoint {
   var urlPathComponent: String {
      switch self {
      case .register: return "devices.json"
      case .unregister(let id): return "devices/\(id).json"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .register: return .post
      case .unregister: return .delete
      }
   }
}
`
	got := map[string]string{}
	for _, f := range extractEndpointFacts([]byte(src), "Sources/Core/APIService.Device.swift", "Sources/Core", "v2") {
		got[f.Name] = f.Props["method"].(string)
	}
	if got["v2/devices.json"] != "POST" {
		t.Errorf("collection endpoint should get default v2 prefix: %+v", got)
	}
	if got["v2/devices/{}.json"] != "DELETE" {
		t.Errorf("param endpoint should get default v2 prefix: %+v", got)
	}
}

// TestExtractEndpointFacts_SingleValueOverride: a single-value computed prefix
// (implicit return, no switch) overrides the default for all cases.
func TestExtractEndpointFacts_SingleValueOverride(t *testing.T) {
	src := `extension APIService.Interactions: APIEndpoint {
   var urlPrefixComponent: String { "core/v3" }
   var urlPathComponent: String {
      switch self {
      case .get: return "interactions"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .get: return .get
      }
   }
}
`
	got := map[string]string{}
	for _, f := range extractEndpointFacts([]byte(src), "Sources/Core/APIService.Interactions.swift", "Sources/Core", "v2") {
		got[f.Name] = f.Props["method"].(string)
	}
	if _, ok := got["core/v3/interactions"]; !ok {
		t.Errorf("single-value override should win over default: %+v", got)
	}
}

// TestExtractEndpointFacts_VersionInterpPrefix: a version-constant interpolation in
// the prefix resolves to the version token, not the {} placeholder.
func TestExtractEndpointFacts_VersionInterpPrefix(t *testing.T) {
	src := `extension APIService.Items: APIEndpoint {
   var urlPrefixComponent: String { "core/\(APIVersion.v3)" }
   var urlPathComponent: String {
      switch self {
      case .list: return "items"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .list: return .get
      }
   }
}
`
	got := map[string]string{}
	for _, f := range extractEndpointFacts([]byte(src), "Sources/Core/APIService.Items.swift", "Sources/Core", "v2") {
		got[f.Name] = f.Props["method"].(string)
	}
	if _, ok := got["core/v3/items"]; !ok {
		t.Errorf("version interpolation should resolve to core/v3: %+v", got)
	}
}

// TestExtractEndpointFacts_SwitchPrefixDefault: a prefix switch with a `default:`
// branch resolves unlisted cases to the default branch value.
func TestExtractEndpointFacts_SwitchPrefixDefault(t *testing.T) {
	src := `extension APIService.Account: APIEndpoint {
   var urlPrefixComponent: String {
      switch self {
      case .generateToken, .deleteToken: return "core/v3"
      default: return "v2"
      }
   }
   var urlPathComponent: String {
      switch self {
      case .loadAccount: return "account.json"
      case .generateToken: return "account/token.json"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .loadAccount: return .get
      case .generateToken: return .post
      }
   }
}
`
	got := map[string]string{}
	for _, f := range extractEndpointFacts([]byte(src), "Sources/Core/APIService.Account.swift", "Sources/Core", "v2") {
		got[f.Name] = f.Props["method"].(string)
	}
	if got["v2/account.json"] != "GET" {
		t.Errorf("unlisted case should use switch default v2: %+v", got)
	}
	if got["core/v3/account/token.json"] != "POST" {
		t.Errorf("explicit case should use core/v3: %+v", got)
	}
}

// TestDetectDefaultURLPrefix: the default is read from the protocol-declaring file
// and a concrete single-value override elsewhere is not mistaken for it.
func TestDetectDefaultURLPrefix(t *testing.T) {
	protoFile := "Sources/Core/APIEndpoint.swift"
	proto := `public protocol APIEndpoint {
   var urlPrefixComponent: String { get }
   var urlPathComponent: String { get }
}

public extension APIEndpoint {
   var urlPrefixComponent: String {
      "v2" // default for all existing urls
   }
}
`
	concrete := "Sources/Core/APIService.Widget.swift"

	dir := t.TempDir()
	mustWrite(t, dir, protoFile, proto)
	// A concrete single-value override — must NOT be picked as the default.
	mustWrite(t, dir, concrete, "struct WidgetEndpoint: APIEndpoint {\n   var urlPrefixComponent: String { \"core/v3\" }\n}\n")

	if got := detectDefaultURLPrefix(dir, []string{concrete, protoFile}); got != "v2" {
		t.Errorf("detectDefaultURLPrefix = %q, want v2 (from the protocol file, ignoring the concrete override)", got)
	}
}

func TestExtractURLSessionFacts(t *testing.T) {
	src := `import Foundation

final class EntitlementAPIService {
    func getDefinitions() async throws -> [EntitlementDto] {
        var request = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/definitions"))
        request.httpMethod = "GET"
        return try await send(request)
    }

    func getActive(userID: Int) async throws -> [ActiveEntitlementDto] {
        var request = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/users/\(userID)/active"))
        return try await send(request)   // no explicit httpMethod -> default GET
    }

    func grant(userID: Int) async throws {
        var urlRequest = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/users/\(userID)/grant"))
        urlRequest.httpMethod = "POST"
        _ = try await send(urlRequest)
    }
}
`
	ff := extractURLSessionFacts([]byte(src), "Data/Network/EntitlementAPIService.swift")
	if len(ff) != 3 {
		t.Fatalf("expected 3 client routes, got %d: %+v", len(ff), ff)
	}

	byName := map[string]string{} // name -> method
	for _, f := range ff {
		if f.Props["role"] != "client" || f.Props["framework"] != "urlsession" {
			t.Errorf("%s wrong props: %+v", f.Name, f.Props)
		}
		if f.Props["api"] != "EntitlementAPIService" {
			t.Errorf("%s api hint = %v", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props["method"].(string)
	}

	if byName["settings/entitlements/definitions"] != "GET" {
		t.Errorf("definitions: want GET, got %q", byName["settings/entitlements/definitions"])
	}
	// Interpolation collapsed to {}, no httpMethod line -> default GET.
	if byName["settings/entitlements/users/{}/active"] != "GET" {
		t.Errorf("active: want default GET on {}, got %+v", byName)
	}
	if byName["settings/entitlements/users/{}/grant"] != "POST" {
		t.Errorf("grant: want POST, got %+v", byName)
	}
}

// TestSwitchReturns_MultiLineCaseLabels verifies that a case-label list wrapping
// across several lines maps EVERY label to the clause's return value, not just the
// labels on the `case` line (the bug that defaulted continuation-line cases to GET).
func TestSwitchReturns_MultiLineCaseLabels(t *testing.T) {
	body := `
      case .alpha, .beta,
           .gamma,
           .delta, .epsilon:
         return .post
      case .zeta:
         return .get
      default:
         return .put
      `
	got := switchReturns(body, swiftReturnEnumCase)
	for _, lbl := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		if got[lbl] != "post" {
			t.Errorf("label %q: got %q, want post (multi-line case)", lbl, got[lbl])
		}
	}
	if got["zeta"] != "get" {
		t.Errorf("single-line case zeta: got %q, want get", got["zeta"])
	}
	if got[swiftDefaultLabel] != "put" {
		t.Errorf("default: got %q, want put", got[swiftDefaultLabel])
	}
}

// TestExtractEndpointFacts_MultiLineMethodCase verifies the end-to-end effect: an
// endpoint whose method switch places a case on a continuation line gets its real
// verb (POST), not the GET default.
func TestExtractEndpointFacts_MultiLineMethodCase(t *testing.T) {
	src := `extension APIService.Generic: APIEndpoint {
   var urlPathComponent: String {
      switch self {
      case .stats: return "profile/counter_stats.json"
      case .magicToken: return "magic_token.json"
      }
   }
   var method: HTTPMethod {
      switch self {
      case .stats:
         return .get
      case .forgotPassword,
           .magicToken:
         return .post
      }
   }
}
`
	byName := map[string]string{}
	for _, f := range extractEndpointFacts([]byte(src), "Sources/Core/APIService.Generic.swift", "Sources/Core", "") {
		byName[f.Name] = f.Props["method"].(string)
	}
	if byName["magic_token.json"] != "POST" {
		t.Errorf("magic_token: want POST from continuation-line case, got %q (%+v)", byName["magic_token.json"], byName)
	}
	if byName["profile/counter_stats.json"] != "GET" {
		t.Errorf("stats: want GET, got %q", byName["profile/counter_stats.json"])
	}
}

// TestExtractEndpointFacts_ConstantMethod verifies that a single-value method
// property (`var method: HTTPMethod { return .post }`, no switch) applies its verb to
// the endpoint's routes instead of defaulting to GET.
func TestExtractEndpointFacts_ConstantMethod(t *testing.T) {
	src := `extension APIService.Tracking: APIEndpoint {
   var urlPathComponent: String {
      switch self {
      case .order: return "order_trackings.json"
      }
   }
   var method: HTTPMethod {
      return .post
   }
}
`
	ff := extractEndpointFacts([]byte(src), "Sources/Core/APIService.Tracking.swift", "Sources/Core", "")
	if len(ff) != 1 {
		t.Fatalf("expected 1 route, got %d: %+v", len(ff), ff)
	}
	if ff[0].Props["method"] != "POST" {
		t.Errorf("constant method property: want POST, got %q", ff[0].Props["method"])
	}
}

// TestMethodNear_BeforeAndEnumForms verifies the widened method inference: a verb
// set on the request BEFORE the path line is found (symmetric window), and enum/
// member forms (HTTPMethod.delete.rawValue, Alamofire .put) map to the verb instead
// of silently defaulting to GET.
func TestMethodNear_BeforeAndEnumForms(t *testing.T) {
	src := `import Foundation

final class OrdersAPIService {
    func remove(id: Int) async throws {
        var request = URLRequest(url: URL(string: "placeholder")!)
        request.httpMethod = HTTPMethod.delete.rawValue
        request.url = baseURL.appendingPathComponent("orders/\(id)/remove")
        _ = try await send(request)
    }
    func replace(id: Int) async throws {
        var request = URLRequest(url: baseURL.appendingPathComponent("orders/\(id)/replace"))
        request.method = .put
        _ = try await send(request)
    }
}
`
	byName := map[string]string{}
	for _, f := range extractURLSessionFacts([]byte(src), "Data/Network/OrdersAPIService.swift") {
		byName[f.Name] = f.Props["method"].(string)
		if _, ok := f.Props["external"]; ok {
			t.Errorf("%s should not be external (injected base URL): %+v", f.Name, f.Props)
		}
	}
	if byName["orders/{}/remove"] != "DELETE" {
		t.Errorf("remove: want DELETE from enum form set before the path, got %q (%+v)", byName["orders/{}/remove"], byName)
	}
	if byName["orders/{}/replace"] != "PUT" {
		t.Errorf("replace: want PUT from .put form, got %q (%+v)", byName["orders/{}/replace"], byName)
	}
}

// A verb set on the request well below its path, past a multipart body, is still the
// request's: the request built from the path is followed to its assignment. A verb on
// another function's request is not borrowed, and with none set the default stays GET.
func TestMethodNear_FollowsTheRequestBuiltFromThePath(t *testing.T) {
	src := `import Foundation

final class CoachAPIService {
    func analyze(images: [Data], prompt: String) async throws {
        var components = URLComponents(url: baseURL.appendingPathComponent("settings/coach/analyze"), resolvingAgainstBaseURL: false)
        components?.queryItems = [URLQueryItem(name: "feature", value: "swing")]
        guard let url = components?.url else { throw CoachError.invalidURL }

        let boundary = "Boundary-\(UUID().uuidString)"
        var body = Data()
        for image in images {
            body.append("--\(boundary)\r\n")
            body.append(image)
            body.append("\r\n")
        }
        body.append("--\(boundary)--\r\n")

        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        _ = try await session.upload(for: request, from: body)
    }
    func status() async throws {
        let url = baseURL.appendingPathComponent("settings/coach/status")
        let other = 1
        let more = 2
        let evenMore = 3
        let stillMore = 4
        let andMore = 5
        let request = URLRequest(url: url)
        _ = try await session.data(for: request)
    }
    func reset() async throws {
        var request = URLRequest(url: baseURL)
        request.httpMethod = "DELETE"
        _ = try await session.data(for: request)
    }
}
`
	byName := map[string]string{}
	for _, f := range extractURLSessionFacts([]byte(src), "Data/Network/CoachAPIService.swift") {
		byName[f.Name] = f.Props["method"].(string)
	}
	if byName["settings/coach/analyze"] != "POST" {
		t.Errorf("analyze: want POST from the request built from the path, got %q (%+v)", byName["settings/coach/analyze"], byName)
	}
	if byName["settings/coach/status"] != "GET" {
		t.Errorf("status: want GET, not the next function's verb, got %q (%+v)", byName["settings/coach/status"], byName)
	}
}

// TestExtractEndpointFacts_ExternalHost verifies that a client whose base URL is a
// hardcoded absolute host (the APIService.Jira idiom) is tagged external + host, so
// the linker buckets it out of the internal unresolved count.
func TestExtractEndpointFacts_ExternalHost(t *testing.T) {
	src := `import Foundation

extension APIService.Jira: ImageUploadEndpoint {
    public var urlString: String {
        return "https://jira.service.com/\(urlPathComponent)"
    }
    public var urlPathComponent: String {
        switch self {
        case .createIssue:
            return "rest/api/2/issue"
        case .addAttachment:
            return "rest/api/2/issue/attachments"
        }
    }
    public var method: HTTPMethod {
        switch self {
        case .createIssue, .addAttachment:
            return .post
        }
    }
}
`
	ff := extractEndpointFacts([]byte(src), "Sources/Core/3P/APIService.Jira.swift", "Sources/Core", "")
	if len(ff) == 0 {
		t.Fatalf("expected endpoint routes, got none")
	}
	for _, f := range ff {
		if ext, _ := f.Props["external"].(bool); !ext {
			t.Errorf("%s: want external=true, got %+v", f.Name, f.Props)
		}
		if f.Props["host"] != "jira.service.com" {
			t.Errorf("%s: want host jira.service.com, got %v", f.Name, f.Props["host"])
		}
		if f.Props["method"] != "POST" {
			t.Errorf("%s: want POST, got %v", f.Name, f.Props["method"])
		}
	}
}

// TestExtractURLSessionFacts_NonNetworkFileSkipped verifies the file-level gate:
// a source that never references URLSession/URLRequest (e.g. a PDF exporter using
// appendingPathComponent for file I/O) emits no client routes.
func TestExtractURLSessionFacts_NonNetworkFileSkipped(t *testing.T) {
	src := `import Foundation

final class PdfExporter {
    func exportURL() -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("exports")
        return dir.appendingPathComponent("report.pdf")
    }
}
`
	if ff := extractURLSessionFacts([]byte(src), "Screens/PerformanceReport/PdfExporter.swift"); len(ff) != 0 {
		t.Fatalf("expected no routes from a non-network file, got %d: %+v", len(ff), ff)
	}
}

// TestExtractURLSessionFacts_FileURLsExcluded verifies that file-URL building inside
// a network file (a temp .mov written before an upload) is excluded — via the
// file-URL line signal and the media-extension filter — while a real API call in the
// same file is still detected.
func TestExtractURLSessionFacts_FileURLsExcluded(t *testing.T) {
	src := `import Foundation

final class MediaAnalysisModal {
    func upload() async throws {
        let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("media-analysis-\(UUID().uuidString).mov")
        var request = URLRequest(url: baseURL.appendingPathComponent("settings/visual-ai-coach/analyze"))
        request.httpMethod = "POST"
        _ = try await URLSession.shared.data(for: request)
    }
}
`
	ff := extractURLSessionFacts([]byte(src), "Components/QuickActions/MediaAnalysisModal.swift")
	if len(ff) != 1 {
		t.Fatalf("expected exactly 1 route (the real API call), got %d: %+v", len(ff), ff)
	}
	if ff[0].Name != "settings/visual-ai-coach/analyze" || ff[0].Props["method"] != "POST" {
		t.Errorf("unexpected route: %+v", ff[0])
	}
}

// --- stored-method endpoint structs (call-site verb resolution) ---

// storedEndpointDefSource is a stored-method endpoint type: path + prefix are
// computed, but `method` is a stored property set by the caller at init. Neutral
// names throughout.
const storedEndpointDefSource = `import Foundation

struct WidgetActionEndpoint: APIEndpoint {
   var urlPrefixComponent: String { "core/v3" }
   var urlPathComponent: String { "widgets/\(id)/action" }
   let id: String
   let method: HTTPMethod
}
`

// TestExtractStoredMethodEndpointFacts: a stored-method endpoint defined in one file
// and instantiated with `.post` in another emits one client route with that verb.
func TestExtractStoredMethodEndpointFacts(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/WidgetActionEndpoint.swift"
	callFile := "Sources/Feature/WidgetService.swift"
	mustWrite(t, dir, defFile, storedEndpointDefSource)
	mustWrite(t, dir, callFile, `final class WidgetService {
   func perform(id: String) {
      let endpoint = WidgetActionEndpoint(id: id, method: .post)
      client.send(endpoint)
   }
}
`)

	ff := extractCallSiteEndpointFacts(dir, []string{defFile, callFile}, "", testModuleForFile)
	if len(ff) != 1 {
		t.Fatalf("expected 1 client route, got %d: %+v", len(ff), ff)
	}
	f := ff[0]
	if f.Name != "core/v3/widgets/{}/action" || f.Props["method"] != "POST" {
		t.Errorf("route mismatch: name=%q method=%v", f.Name, f.Props["method"])
	}
	if f.Props["role"] != "client" || f.Props["source"] != "swift-endpoint" || f.Props["framework"] != "apiendpoint" {
		t.Errorf("wrong props: %+v", f.Props)
	}
	if f.File != defFile {
		t.Errorf("route should anchor at the type definition file, got %q", f.File)
	}
}

// TestExtractStoredMethodEndpointFacts_MultipleVerbs: one type instantiated with two
// different verbs yields two routes, one per verb.
func TestExtractStoredMethodEndpointFacts_MultipleVerbs(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/WidgetActionEndpoint.swift"
	callFile := "Sources/Feature/WidgetService.swift"
	mustWrite(t, dir, defFile, storedEndpointDefSource)
	mustWrite(t, dir, callFile, `final class WidgetService {
   func create(id: String) { _ = WidgetActionEndpoint(id: id, method: .post) }
   func remove(id: String) { _ = WidgetActionEndpoint(id: id, method: .delete) }
}
`)

	got := map[string]bool{}
	for _, f := range extractCallSiteEndpointFacts(dir, []string{defFile, callFile}, "", testModuleForFile) {
		if f.Name != "core/v3/widgets/{}/action" {
			t.Errorf("unexpected path: %q", f.Name)
		}
		got[f.Props["method"].(string)] = true
	}
	if !got["POST"] || !got["DELETE"] || len(got) != 2 {
		t.Fatalf("want POST+DELETE, got %+v", got)
	}
}

// TestExtractStoredMethodEndpointFacts_NoCallSite: a stored-method endpoint that is
// never instantiated emits nothing (Approach A — the verb is unknowable).
func TestExtractStoredMethodEndpointFacts_NoCallSite(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/WidgetActionEndpoint.swift"
	mustWrite(t, dir, defFile, storedEndpointDefSource)

	if ff := extractCallSiteEndpointFacts(dir, []string{defFile}, "", testModuleForFile); len(ff) != 0 {
		t.Fatalf("expected no routes without a call site, got %d: %+v", len(ff), ff)
	}
}

// TestExtractStoredMethodEndpointFacts_DefaultPrefix: a stored-method endpoint with
// no prefix property inherits the repo-wide default prefix.
func TestExtractStoredMethodEndpointFacts_DefaultPrefix(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/ItemEndpoint.swift"
	callFile := "Sources/Feature/ItemService.swift"
	mustWrite(t, dir, defFile, `struct ItemEndpoint: APIEndpoint {
   var urlPathComponent: String { "items/\(id)" }
   let id: String
   let method: HTTPMethod
}
`)
	mustWrite(t, dir, callFile, `func load(id: String) { _ = ItemEndpoint(id: id, method: .get) }
`)

	ff := extractCallSiteEndpointFacts(dir, []string{defFile, callFile}, "v2", testModuleForFile)
	if len(ff) != 1 || ff[0].Name != "v2/items/{}" || ff[0].Props["method"] != "GET" {
		t.Fatalf("default-prefix stored endpoint mismatch: %+v", ff)
	}
}

// TestExtractStoredMethodEndpointFacts_NotEndpoints: a type with a stored `method`
// but no path property, a computed-method endpoint (handled elsewhere), and an
// unrelated constructor call all emit nothing from this pass.
func TestExtractStoredMethodEndpointFacts_NotEndpoints(t *testing.T) {
	dir := t.TempDir()
	f1 := "Sources/A.swift"
	f2 := "Sources/B.swift"
	f3 := "Sources/C.swift"
	// Stored method but no path property -> not an endpoint.
	mustWrite(t, dir, f1, `struct PlainModel { let method: HTTPMethod }
`)
	// Computed method -> owned by extractEndpointFacts, not this pass.
	mustWrite(t, dir, f2, `struct ComputedEndpoint: APIEndpoint {
   var urlPathComponent: String { "things" }
   var method: HTTPMethod { .get }
}
`)
	// An unrelated constructor passing a method: arg for a non-endpoint type.
	mustWrite(t, dir, f3, `func go() {
   _ = PlainModel(method: .post)
   _ = ComputedEndpoint()
}
`)

	if ff := extractCallSiteEndpointFacts(dir, []string{f1, f2, f3}, "", testModuleForFile); len(ff) != 0 {
		t.Fatalf("expected no routes, got %d: %+v", len(ff), ff)
	}
}

// TestEndpointCallSites: top-level `method:`/`httpMethod:` verbs and
// `urlPathComponent:` literals are read; a ternary/computed verb is skipped, and a
// nested call's args are attributed to the nested type, not the outer one.
func TestEndpointCallSites(t *testing.T) {
	src := `let a = FooEndpoint(id: x, method: .post)
let b = BarEndpoint(method: isEdit ? .put : .patch)
let c = OuterEndpoint(body: InnerEndpoint(method: .delete), method: .get)
let d = WrapRequest(urlPathComponent: "posts/\(id)", httpMethod: .put)
`
	byType := map[string]endpointCallSite{}
	for _, cs := range endpointCallSites(src, "X.swift") {
		byType[cs.typeName] = cs
	}
	if cs := byType["FooEndpoint"]; cs.verb != "post" {
		t.Errorf("FooEndpoint verb = %q, want post", cs.verb)
	}
	if cs := byType["BarEndpoint"]; cs.verb != "" {
		t.Errorf("BarEndpoint ternary verb should be skipped, got %q", cs.verb)
	}
	if cs := byType["OuterEndpoint"]; cs.verb != "get" || cs.pathLiteral {
		t.Errorf("OuterEndpoint = %+v, want verb get and no path", cs)
	}
	if cs := byType["InnerEndpoint"]; cs.verb != "delete" {
		t.Errorf("InnerEndpoint verb = %q, want delete", cs.verb)
	}
	if cs := byType["WrapRequest"]; !cs.pathLiteral || cs.pathArg != `posts/\(id)` || cs.verb != "put" {
		t.Errorf("WrapRequest = %+v, want path posts/\\(id) + verb put", cs)
	}
}

// wrapperDefSource is a request-wrapper endpoint: the path is a stored, required
// property supplied at each call site; prefix and default verb live on the type.
const wrapperDefSource = `import Foundation

extension API {
   struct SimpleRequest {
      var urlPrefixComponent = "core/v3"
      var urlPathComponent: String
      var requestParams: [String: Any] = [:]
      var method: HTTPMethod = .get
   }
}

extension API.SimpleRequest: APIEndpoint {}
`

// TestWrapperEndpoint_PathAndVerbFromCallSite: a wrapper's path comes from the call
// site's urlPathComponent: literal; the verb from method: or the type default.
func TestWrapperEndpoint_PathAndVerbFromCallSite(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/SimpleRequest.swift"
	callFile := "Sources/Feature/Service.swift"
	mustWrite(t, dir, defFile, wrapperDefSource)
	mustWrite(t, dir, callFile, `func f(id: String) {
   _ = API.SimpleRequest(urlPathComponent: "posts/\(id)", method: .delete)
   _ = API.SimpleRequest(urlPathComponent: "items?limit=10")
}
`)

	got := map[string]string{}
	for _, f := range extractCallSiteEndpointFacts(dir, []string{defFile, callFile}, "", testModuleForFile) {
		got[f.Name] = f.Props["method"].(string)
		if f.File != callFile {
			t.Errorf("wrapper route should anchor at the call site, got %q", f.File)
		}
	}
	if got["core/v3/posts/{}"] != "DELETE" {
		t.Errorf("explicit verb: want core/v3/posts/{} DELETE, got %+v", got)
	}
	if got["core/v3/items"] != "GET" { // query string stripped; default verb .get
		t.Errorf("default verb + query strip: want core/v3/items GET, got %+v", got)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 routes, got %+v", got)
	}
}

// TestWrapperEndpoint_HttpMethodArgAndComputedDefault covers the httpMethod: arg name
// (with a stored default) and a verb fixed by a computed method in the conformance
// extension.
func TestWrapperEndpoint_HttpMethodArgAndComputedDefault(t *testing.T) {
	dir := t.TempDir()
	// httpMethod default .post, overridable via httpMethod: arg.
	encFile := "Sources/Core/EncRequest.swift"
	mustWrite(t, dir, encFile, `extension API {
   struct EncRequest {
      var urlPrefixComponent = "core/v3"
      var urlPathComponent: String
      var httpMethod: HTTPMethod = .post
   }
}
extension API.EncRequest: APIEndpoint { public var method: HTTPMethod { httpMethod } }
`)
	// verb fixed by a computed method in the extension (no init verb).
	putFile := "Sources/Core/PutOnly.swift"
	mustWrite(t, dir, putFile, `extension API {
   struct PutOnly {
      var urlPrefixComponent = "core/v3"
      var urlPathComponent: String
   }
}
extension API.PutOnly: APIEndpoint { public var method: HTTPMethod { .put } }
`)
	callFile := "Sources/Feature/Svc.swift"
	mustWrite(t, dir, callFile, `func f(id: String) {
   _ = API.EncRequest(urlPathComponent: "a/\(id)")
   _ = API.EncRequest(urlPathComponent: "b/\(id)", httpMethod: .put)
   _ = API.PutOnly(urlPathComponent: "c/\(id)")
}
`)

	got := map[string]string{}
	for _, f := range extractCallSiteEndpointFacts(dir, []string{encFile, putFile, callFile}, "", testModuleForFile) {
		got[f.Name] = f.Props["method"].(string)
	}
	if got["core/v3/a/{}"] != "POST" {
		t.Errorf("EncRequest default: want core/v3/a/{} POST, got %+v", got)
	}
	if got["core/v3/b/{}"] != "PUT" {
		t.Errorf("EncRequest httpMethod override: want core/v3/b/{} PUT, got %+v", got)
	}
	if got["core/v3/c/{}"] != "PUT" {
		t.Errorf("PutOnly computed-fixed verb: want core/v3/c/{} PUT, got %+v", got)
	}
}

// TestWrapperEndpoint_NonLiteralPathSkipped: a wrapper call site whose path is a
// variable (not a literal) is unresolvable and emits nothing.
func TestWrapperEndpoint_NonLiteralPathSkipped(t *testing.T) {
	dir := t.TempDir()
	defFile := "Sources/Core/SimpleRequest.swift"
	callFile := "Sources/Feature/Service.swift"
	mustWrite(t, dir, defFile, wrapperDefSource)
	mustWrite(t, dir, callFile, `func f(path: String) {
   _ = API.SimpleRequest(urlPathComponent: path, method: .get)
}
`)

	if ff := extractCallSiteEndpointFacts(dir, []string{defFile, callFile}, "", testModuleForFile); len(ff) != 0 {
		t.Fatalf("expected no routes for a variable path, got %d: %+v", len(ff), ff)
	}
}

// TestParenBody_StringLiteral: a ')' inside a Swift string literal does not close
// the argument span early.
func TestParenBody_StringLiteral(t *testing.T) {
	src := `Foo(label: "a )( b", method: .post)`
	open := len("Foo")
	body, ok := parenBody(src, open)
	if !ok || body != `label: "a )( b", method: .post` {
		t.Fatalf("parenBody = (%q, %v)", body, ok)
	}
}

func TestCollapseSwiftInterpolation(t *testing.T) {
	cases := map[string]string{
		`media-analysis-\(UUID().uuidString).mov`: "media-analysis-{}.mov",
		`users/\(userID)/active`:                  "users/{}/active",
		`a/\(x)/b/\(y)`:                           "a/{}/b/{}",
		`no-interpolation`:                        "no-interpolation",
	}
	for in, want := range cases {
		if got := collapseSwiftInterpolation(in); got != want {
			t.Errorf("collapseSwiftInterpolation(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExtractURLSessionFacts_TestSourcesSkipped covers the v69 test/fixture gate:
// URLSession request-building under a Tests/ path (or a *Tests basename) is fixture
// code, not a real endpoint, so it emits no client route — while the identical code
// under a production path still does.
func TestExtractURLSessionFacts_TestSourcesSkipped(t *testing.T) {
	src := `import Foundation

final class MockAPI {
    func fetch() async throws {
        var request = URLRequest(url: baseURL.appendingPathComponent("users"))
        request.httpMethod = "GET"
        _ = try await send(request)
    }
}
`
	if ff := extractURLSessionFacts([]byte(src), "Tests/AppTests/MockAPITests.swift"); len(ff) != 0 {
		t.Fatalf("expected no routes from a test source, got %d: %+v", len(ff), ff)
	}
	if ff := extractURLSessionFacts([]byte(src), "Sources/App/MockAPI.swift"); len(ff) != 1 {
		t.Fatalf("expected 1 route from the same code under a production path, got %d: %+v", len(ff), ff)
	}
}
