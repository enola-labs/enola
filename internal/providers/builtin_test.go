package providers

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/providers/rubydex"
	"github.com/enola-labs/enola/internal/providerspec"
)

// The names a config may use without a command are declared in providerspec,
// where the config validates against them, and implemented here. A name
// declared with no runner would validate and then fail at snapshot time; a
// runner with no declared name could never be configured.
func TestBuiltIns_ImplementExactlyTheDeclaredNames(t *testing.T) {
	implemented := make([]string, 0, len(builtIns))
	for name := range builtIns {
		implemented = append(implemented, name)
	}
	sort.Strings(implemented)
	if declared := providerspec.BuiltInNames(); !reflect.DeepEqual(implemented, declared) {
		t.Errorf("runners exist for %v, providerspec declares %v", implemented, declared)
	}
}

// Without the library in the cache the built-in is a named skip that says
// which command installs it; the snapshot never fails and never fetches.
func TestRun_BuiltInWithoutItsLibraryIsANamedSkip(t *testing.T) {
	if _, installed := rubydex.Installed(); installed {
		t.Skip("the Rubydex library is installed here, so the skip cannot be observed")
	}
	_, records := Run(context.Background(), []Provider{{Name: "rubydex"}}, t.TempDir(), nil, nil)
	if len(records) != 1 || !records[0].Skipped || !strings.Contains(records[0].Reason, rubydex.FetchHint) {
		t.Fatalf("records = %+v, want one skip naming %q", records, rubydex.FetchHint)
	}
}

func TestRun_UnknownBuiltInIsANamedSkip(t *testing.T) {
	_, records := Run(context.Background(), []Provider{{Name: "nope"}}, t.TempDir(), nil, nil)
	if len(records) != 1 || !records[0].Skipped || !strings.Contains(records[0].Reason, "no built-in provider") {
		t.Fatalf("records = %+v", records)
	}
}
