package facts

import "testing"

func TestIsGeneratedDartPath(t *testing.T) {
	for _, p := range []string{
		"lib/models/user.g.dart", "lib/models/user.freezed.dart", "lib/api/client.mocks.dart",
		"lib/routing/router.gr.dart", "lib/di/injection.config.dart", "lib/forms/login.gform.dart",
		"lib/proto/messages.pb.dart", "lib/proto/messages.pbgrpc.dart",
		"lib/model/device.mapper.dart",
		// drift_dev make-migrations writes its banner after the imports, so the
		// file name is the only marker a leading-comment scan can see.
		"lib/database/database.steps.dart", "LIB/DB.STEPS.DART",
	} {
		if !IsGeneratedDartPath(p) {
			t.Errorf("IsGeneratedDartPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"lib/config.dart", "lib/gr.dart", "lib/pbx.dart", "lib/steps.dart", "lib/user.g.ts", "lib/user_g.dart", "lib/mapper.dart"} {
		if IsGeneratedDartPath(p) {
			t.Errorf("IsGeneratedDartPath(%q) = true, want false", p)
		}
	}
}
