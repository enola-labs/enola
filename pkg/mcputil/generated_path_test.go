package mcputil

import "testing"

func TestIsGeneratedPath(t *testing.T) {
	generated := []string{
		// Pre-existing segment matches.
		"src/build/x.go", "app/node_modules/pkg/i.js", "gen/generated/y.ts",
		// New segment matches (vendored, codegen dirs).
		"vendor/github.com/x/y.go", "ui/openapi-gen/requests/core.ts",
		"proj/third_party/lib.cc", "app/__generated__/schema.ts",
		// Python virtual environments / installed dependencies.
		".venv/lib/python3.12/site-packages/pandas/core/frame.py",
		"sub/venv/lib/python3.11/site-packages/numpy/__init__.py",
		"any/site-packages/requests/api.py",
		// New suffix matches (codegen / minified files not under a marker dir).
		"airflow/ui/openapi-gen/requests/client/utils.gen.ts",
		"a/b/params.gen.tsx", "svc/api.pb.go", "svc/api_pb2.py",
		"svc/api_pb2_grpc.py", "web/app.min.js", "web/app.min.css",
		// Bare "gen" segment (prost-build and similar Rust/Go protobuf output).
		"crates/proto-rust/src/gen/v1.public.core_types.rs",
		"crates/dbt-telemetry/src/gen/v1.events.rs",
		"pkg/gen/api.pb.rs",
		// The Kubernetes code generators' file name.
		"apps/dashboard/pkg/apis/v0alpha1/zz_generated.deepcopy.go", "zz_generated.openapi.go",
	}
	for _, p := range generated {
		if !IsGeneratedPath(p) {
			t.Errorf("IsGeneratedPath(%q) = false, want true", p)
		}
	}

	source := []string{
		"airflow/models/dagrun.py", "internal/perf/perf.go",
		"src/components/Graph/reactflowUtils.ts",
		// "generator" is not the exact segment "generated"; ".genesis.ts" is not ".gen.ts".
		// "general"/"genetics" are not the exact segment "gen".
		"pkg/generator/x.go", "src/genesis.ts", "app/vendored_helpers.py",
		"internal/general/config.go", "src/genetics/model.py",
		// Bare "env" is too common to treat as a venv; only .venv/venv/site-packages match.
		"app/env/settings.py", "src/environments/prod.py",
		// Only the generators' own prefix, as a file name.
		"pkg/zz_generated/helpers.go", "pkg/generated_zz.go",
	}
	for _, p := range source {
		if IsGeneratedPath(p) {
			t.Errorf("IsGeneratedPath(%q) = true, want false", p)
		}
	}
}
