package rubyextractor

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestGraphQLRubyRoutes_RootFieldsOnly(t *testing.T) {
	src := []byte(`module Types
  class QueryType < Types::BaseObject
    field :page_views, [Types::PageViewType], null: false
    field :company, Types::CompanyType do
      argument :id, ID, required: true
    end
  end
end
`)
	ff := extractGraphQLRubyRoutes(src, "app/graphql/types/query_type.rb")
	if len(ff) != 2 {
		t.Fatalf("got %d routes, want 2: %+v", len(ff), ff)
	}
	if ff[0].Name != "Query.pageViews" {
		t.Errorf("name = %q, want the camelized root field", ff[0].Name)
	}
	if ff[0].Props[facts.PropRouteType] != facts.RouteTypeGraphQL || ff[0].Props[facts.PropRole] != facts.RoleServer {
		t.Errorf("props = %v", ff[0].Props)
	}
	if got := extractGraphQLRubyRoutes([]byte("class UserType < Types::BaseObject\n  field :name, String\nend\n"), "app/graphql/types/user_type.rb"); got != nil {
		t.Errorf("non-root type emitted %v — schema internals are not operations", got)
	}
	if got := extractGraphQLRubyRoutes(src, "app/models/query_type.rb"); got != nil {
		t.Errorf("outside a graphql dir emitted %v", got)
	}
}

func TestGraphQLRubyClient_QuotedAndHeredocOperations(t *testing.T) {
	src := []byte(`class SessionsJob
  def insights_query
    "query ($dateRange: DateRangeAttributes!) {
      pageviews(dateRange: $dateRange) {
        count
      }
      visits {
        count
      }
    }"
  end

  def mutation_doc
    <<~GQL
      mutation {
        trackEvent(name: "x") {
          id
        }
      }
    GQL
  end
end
`)
	ff := extractGraphQLRubyClientOps(src, "app/jobs/sessions_job.rb")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.pageviews", "Query.visits", "Mutation.trackEvent"}
	if len(ff) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("client ops = %v, want %v", names, want)
	}
	for _, f := range ff {
		if f.Props[facts.PropRole] != facts.RoleClient || f.Props[facts.PropRouteType] != facts.RouteTypeGraphQL {
			t.Fatalf("op %s lacks client graphql props: %+v", f.Name, f.Props)
		}
	}
}

func TestGraphQLRubyClient_RubyBlocksAndServerFilesExcluded(t *testing.T) {
	block := []byte("items = query { |x| x.active }\nresult = mutation { compute }\n")
	if ff := extractGraphQLRubyClientOps(block, "app/services/finder.rb"); len(ff) != 0 {
		t.Fatalf("Ruby block syntax read as an operation: %+v", ff)
	}
	server := []byte("class Types::QueryType < Types::BaseObject\n  field :me\nend\nDOC = \"query { me }\"\n")
	if ff := extractGraphQLRubyClientOps(server, "app/models/query_type.rb"); len(ff) != 0 {
		t.Fatalf("a root-type server file emitted client ops: %+v", ff)
	}
	closing := []byte("return query if value == \"not_started\"\n\n    query.where(fastly_status: BUCKETS.fetch(value, []))\n  end\n\n  def options\n    {\n")
	if ff := extractGraphQLRubyClientOps(closing, "app/avo/filters/fastly_status.rb"); len(ff) != 0 {
		t.Fatalf("a closing quote followed by Ruby code read as an operation: %+v", ff)
	}
	schemaDir := []byte("DESC = \"query { example }\"\n")
	if ff := extractGraphQLRubyClientOps(schemaDir, "app/graphql/types/thing_type.rb"); len(ff) != 0 {
		t.Fatalf("a graphql/ tree file emitted client ops: %+v", ff)
	}
}

func TestGraphQLRubyRoutes_NamespaceQualifiedRootClass(t *testing.T) {
	src := []byte("class Types::QueryType < Types::BaseObject\n  field :event_query, Types::EventQuery, null: false\nend\n")
	ff := extractGraphQLRubyRoutes(src, "app/graphql/types/query_type.rb")
	if len(ff) != 1 || ff[0].Name != "Query.eventQuery" {
		t.Fatalf("namespace-qualified root class = %+v, want exactly Query.eventQuery", ff)
	}
}

// A mounted mutation is named after its class's graphql_name and handled by the
// class's resolve; an edition's root module is a root type too.
func TestGraphQLRubyRoutes_MountedMutationsAndModuleRoots(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/graphql/types/mutation_type.rb": `module Types
  class MutationType < BaseObject
    mount_mutation Mutations::Issues::Create, experiment: { milestone: '15.8' }
    mount_mutation ::Mutations::AuditEvents::HTTPFiltersDelete
    mount_mutation Mutations::Unnamed
    mount_mutation Mutations::Missing
    mount_aliased_mutation 'CreateIteration', Mutations::Iterations::Create
    field :echo_create, mutation: Mutations::Echo
  end
end`,
		"ee/app/graphql/ee/types/query_type.rb": `module EE
  module Types
    module QueryType
      prepended do
        field :duo_workflow_workflows, null: true
      end
    end
  end
end`,
		"app/graphql/mutations/issues/create.rb": `module Mutations
  module Issues
    class Create < BaseMutation
      graphql_name 'CreateIssue'
      def resolve(project_path:); end
    end
  end
end`,
		"app/graphql/mutations/audit_events/http_filters_delete.rb": `module Mutations
  module AuditEvents
    class HTTPFiltersDelete < BaseMutation
      graphql_name 'AuditEventsStreamingHTTPNamespaceFiltersDelete'
      def resolve; end
    end
  end
end`,
		"app/graphql/types/deprecated_mutations.rb": `module Types
  module DeprecatedMutations
    extend ActiveSupport::Concern
    included do
      mount_aliased_mutation 'WorkItemExport', Mutations::Issues::Create
      field :not_a_root_field, null: true
    end
  end
end`,
		"app/graphql/mutations/unnamed.rb":           "module Mutations\n  class Unnamed < BaseMutation\n    def resolve; end\n  end\nend",
		"app/graphql/mutations/iterations/create.rb": "module Mutations\n  module Iterations\n    class Create < BaseMutation\n      graphql_name 'IterationCreate'\n      def resolve; end\n    end\n  end\nend",
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL && f.PropString(facts.PropRole) == facts.RoleServer {
			h := ""
			for _, r := range f.Relations {
				if r.Kind == facts.RelHandledBy {
					h = r.Target
				}
			}
			got[f.Name] = h
			if f.PropString(propGraphQLMountClass) != "" || f.PropString(propGraphQLName) != "" {
				t.Errorf("%s kept a pending prop: %v", f.Name, f.Props)
			}
		}
	}
	want := map[string]string{
		"Mutation.createIssue": "Mutations::Issues::Create#resolve",
		"Mutation.auditEventsStreamingHttpNamespaceFiltersDelete": "Mutations::AuditEvents::HTTPFiltersDelete#resolve",
		"Mutation.createIteration":                                "Mutations::Iterations::Create#resolve",
		"Mutation.echoCreate":                                     "",
		"Mutation.workItemExport":                                 "Mutations::Issues::Create#resolve",
		"Query.duoWorkflowWorkflows":                              "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("routes\n got %v\nwant %v", got, want)
	}
}

func TestRailsUnderscore(t *testing.T) {
	for in, want := range map[string]string{
		"CreateIssue":           "create_issue",
		"AmazonS3Configuration": "amazon_s3_configuration",
		"HTTPFiltersDelete":     "http_filters_delete",
		"SavedReplyCreate":      "saved_reply_create",
	} {
		if got := railsUnderscore(in); got != want {
			t.Errorf("railsUnderscore(%q) = %q, want %q", in, got, want)
		}
	}
}
