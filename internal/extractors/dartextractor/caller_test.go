package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A client call names the function or method whose body makes it. Dart's grammar
// makes a function's signature and body siblings, so the walker records each
// function's extent as it emits the symbol.
func TestDartClientCalls_NameTheirCaller(t *testing.T) {
	ff := walkSource(t, "lib/api/orders_api.dart", `import 'package:dio/dio.dart';

class OrdersApi {
  final Dio dio;
  OrdersApi(this.dio);

  Future<void> list() async {
    await dio.get('/api/orders');
  }

  Future<void> cancelAll(List<int> ids) async {
    for (final id in ids) {
      await dio.post('/api/orders/cancel');
    }
  }
}

Future<void> ping(Dio dio) async {
  await dio.get('/api/ping');
}
`)
	symbols := map[string]bool{}
	callers := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			callers[f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for path, want := range map[string]string{
		"/api/orders":        "lib/api.OrdersApi.list",
		"/api/orders/cancel": "lib/api.OrdersApi.cancelAll",
		"/api/ping":          "lib/api.ping",
	} {
		if got := callers[path]; got != want || !symbols[got] {
			t.Errorf("%s caller = %q, want the emitted symbol %q (callers: %v)", path, got, want, callers)
		}
	}
}
