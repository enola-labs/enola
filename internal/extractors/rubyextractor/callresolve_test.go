package rubyextractor

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// methodCalls extracts a repository written from files and returns each method's
// sorted calls targets.
func methodCalls(t *testing.T, files map[string]string) map[string][]string {
	t.Helper()
	repo := t.TempDir()
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		writeFile(t, repo, rel, content)
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), repo, rels)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if k := f.PropString("symbol_kind"); k != facts.SymbolMethod && k != facts.SymbolFunc {
			continue
		}
		calls := []string{}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				calls = append(calls, r.Target)
			}
		}
		sort.Strings(calls)
		out[f.Name] = calls
	}
	return out
}

func wantCalls(t *testing.T, got map[string][]string, method string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got[method], w) {
			t.Errorf("%s calls %v, want it to include %q", method, got[method], w)
		}
	}
}

func wantNoCall(t *testing.T, got map[string][]string, method string, absent ...string) {
	t.Helper()
	for _, a := range absent {
		if slices.Contains(got[method], a) {
			t.Errorf("%s calls %v, want no %q", method, got[method], a)
		}
	}
}

// A service object called on a new instance reaches its call method, from a
// controller in another namespace and through a class method that builds one.
func TestRubyCalls_ServiceObjectOnANewInstance(t *testing.T) {
	got := methodCalls(t, map[string]string{
		"app/services/base_service.rb": `
class BaseService
end
`,
		"app/services/post_status_service.rb": `
class PostStatusService < BaseService
  def self.call(*args)
    new.call(*args)
  end

  def call(account, options = {})
    process_status!
    schedule_status!
  end

  private

  def process_status!; end
end
`,
		"app/controllers/api/v1/statuses_controller.rb": `
module Api
  module V1
    class StatusesController < ApplicationController
      def create
        @status = PostStatusService.new.call(current_account, text: params[:status])
        Missing.new.call
      end

      def update
        PostStatusService.call(current_account)
      end
    end
  end
end
`,
	})
	wantCalls(t, got, "Api::V1::StatusesController#create", "PostStatusService#call", "new.call")
	wantCalls(t, got, "Api::V1::StatusesController#update", "PostStatusService.call")
	wantCalls(t, got, "PostStatusService.call", "PostStatusService#call")
	// A method the class declares is reached by name, one it does not is left bare.
	wantCalls(t, got, "PostStatusService#call", "PostStatusService#process_status!", "schedule_status!")
}

// A call on self reaches the method the nearest ancestor declares, and a constant is
// looked up from the caller's namespace outward.
func TestRubyCalls_InheritedMethodAndNamespacedConstant(t *testing.T) {
	got := methodCalls(t, map[string]string{
		"app/controllers/admin/base_controller.rb": `
module Admin
  class BaseController < ApplicationController
    def require_admin!; end
  end
end
`,
		"app/models/admin/report.rb": `
module Admin
  class Report
    def self.build(params); end
  end
end
`,
		"app/controllers/admin/reports_controller.rb": `
module Admin
  class ReportsController < BaseController
    def show
      require_admin!
      Report.build(params)
    end
  end
end
`,
	})
	wantCalls(t, got, "Admin::ReportsController#show", "Admin::BaseController#require_admin!", "Admin::Report.build")
	wantNoCall(t, got, "Admin::ReportsController#show", "require_admin!", "Report.build")
}
