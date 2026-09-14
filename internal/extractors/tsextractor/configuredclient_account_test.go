package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/facts"
)

func clientAccountOf(t *testing.T, fs []facts.Fact, spec string) facts.Fact {
	t.Helper()
	for _, f := range fs {
		if f.Kind == facts.KindExtraction && f.PropString(facts.PropClientSpec) == spec {
			return f
		}
	}
	t.Fatalf("no account for client %s", spec)
	return facts.Fact{}
}

func accountTally(f facts.Fact) (detected, resolved, unresolved int) {
	entry := f.Props["edge_coverage"].([]map[string]any)[0]
	return entry["detected"].(int), entry["resolved"].(int), entry["unresolved"].(int)
}

// One account per declared client per repository: its receivers, the calls through it,
// the routes they became and the calls skipped by cause. A client that found nothing
// still gets its account, with zero receivers.
func TestConfiguredClient_AccountsPerSpec(t *testing.T) {
	specs := append(sdkClient(), clientspec.Spec{
		Name: "legacy-http", Language: "typescript", ReceiverTypes: []string{"LegacyHttpClient"},
		Methods: []clientspec.Method{{Name: "call", PathArg: intp(0)}},
	})
	clientspec.Normalize(specs)

	fs := extractWithClients(t, specs, map[string]string{
		// 1 receiver; 3 calls: 2 routes, and this.pathFor(key) skipped as dynamic_path.
		"src/connectors/resource-connector.ts": connectorSource,
		// 1 receiver; 2 calls, both skipped.
		"src/misuse.ts": `export class Misuse {
  constructor(private readonly http: IHttpRequestService) {}
  run() {
    this.http.sendRequest("svc");
    this.http.sendRequest("svc", "https://api.example.com/v1/items", { method: "GET" });
  }
}
`,
	})

	sdk := clientAccountOf(t, fs, "sdk-http")
	if got := sdk.Props["receivers"]; got != 2 {
		t.Errorf("receivers = %v, want 2", got)
	}
	if d, r, u := accountTally(sdk); d != 5 || r != 2 || u != 3 {
		t.Errorf("detected/resolved/unresolved = %d/%d/%d, want 5/2/3", d, r, u)
	}
	if got := sdk.PropString("skipped"); got != "dynamic_path=1,missing_path_argument=1,non_route_path=1" {
		t.Errorf("skipped = %q", got)
	}

	legacy := clientAccountOf(t, fs, "legacy-http")
	if d, _, _ := accountTally(legacy); legacy.Props["receivers"] != 0 || d != 0 {
		t.Errorf("a client that found nothing must still report zero receivers and calls: %+v", legacy.Props)
	}
	if _, has := legacy.Props["skipped"]; has {
		t.Errorf("a client that skipped nothing must carry no skipped prop: %+v", legacy.Props)
	}
}
