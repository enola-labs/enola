package rubyextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// symbolsByName indexes the symbol facts in a result by name (storage/dependency
// facts may share a class name, so they are excluded here).
func symbolsByName(result []facts.Fact) map[string]facts.Fact {
	m := make(map[string]facts.Fact)
	for _, f := range result {
		if f.Kind == facts.KindSymbol {
			m[f.Name] = f
		}
	}
	return m
}

// hasCall returns true if the fact has a RelCalls relation to target.
func hasCall(f facts.Fact, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == target {
			return true
		}
	}
	return false
}

func TestExtractFile_BasicClassAndMethod(t *testing.T) {
	src := `# frozen_string_literal: true

module Orders
  class Order < ApplicationRecord
    def total
      items.sum(:price)
    end

    def self.recent
      where("created_at > ?", 1.day.ago)
    end
  end
end
`
	result := extractFileAST([]byte(src), "packages/orders/app/models/order.rb", true, false)
	byName := symbolsByName(result)

	mod, ok := byName["Orders"]
	if !ok {
		t.Fatal("missing module Orders")
	}
	if sk, _ := mod.Props["symbol_kind"].(string); sk != facts.SymbolInterface {
		t.Errorf("Orders symbol_kind = %q, want interface", sk)
	}

	cls, ok := byName["Orders::Order"]
	if !ok {
		t.Fatal("missing class Orders::Order")
	}
	if sk, _ := cls.Props["symbol_kind"].(string); sk != facts.SymbolClass {
		t.Errorf("Orders::Order symbol_kind = %q, want class", sk)
	}
	if sc, _ := cls.Props["superclass"].(string); sc != "ApplicationRecord" {
		t.Errorf("superclass = %q, want ApplicationRecord", sc)
	}
	hasImpl := false
	for _, r := range cls.Relations {
		if r.Kind == facts.RelImplements && r.Target == "ApplicationRecord" {
			hasImpl = true
		}
	}
	if !hasImpl {
		t.Error("Orders::Order missing implements relation to ApplicationRecord")
	}

	meth, ok := byName["Orders::Order#total"]
	if !ok {
		t.Fatal("missing method Orders::Order#total")
	}
	if sk, _ := meth.Props["symbol_kind"].(string); sk != facts.SymbolMethod {
		t.Errorf("total symbol_kind = %q, want method", sk)
	}

	cmeth, ok := byName["Orders::Order.recent"]
	if !ok {
		t.Fatal("missing class method Orders::Order.recent")
	}
	if sk, _ := cmeth.Props["symbol_kind"].(string); sk != facts.SymbolFunc {
		t.Errorf("recent symbol_kind = %q, want function", sk)
	}
}

func TestStorageFacts_DeclaresTargetIsDirectory(t *testing.T) {
	relFile := "packages/items/app/models/item.rb"
	src := `class Item < ApplicationRecord
end
`
	result := extractFileAST([]byte(src), relFile, true, true)

	var storageFact *facts.Fact
	for i, f := range result {
		if f.Kind == facts.KindStorage && f.Name == "Item" {
			storageFact = &result[i]
			break
		}
	}
	if storageFact == nil {
		t.Fatal("expected a storage fact named Item")
	}
	if sk, _ := storageFact.Props["storage_kind"].(string); sk != "model" {
		t.Errorf("storage_kind = %q, want model", sk)
	}
	if len(storageFact.Relations) == 0 {
		t.Fatal("storage fact has no relations")
	}
	declTarget := storageFact.Relations[0].Target
	want := "packages/items/app/models"
	if declTarget != want {
		t.Errorf("declares target = %q, want %q", declTarget, want)
	}
	if declTarget == "Item" {
		t.Error("declares target must not be the class name (self-loop)")
	}
}

func TestAssociationFactNames_IncludeFilePath(t *testing.T) {
	relFile := "packages/orders/app/models/order.rb"
	src := `class Order < ApplicationRecord
  belongs_to :user
  has_many :items
end
`
	result := extractFileAST([]byte(src), relFile, true, true)

	names := make(map[string]bool)
	for _, f := range result {
		if f.Kind != facts.KindDependency {
			continue
		}
		if _, ok := f.Props["association_kind"]; !ok {
			continue
		}
		if !strings.HasPrefix(f.Name, relFile+":") {
			t.Errorf("association fact name %q should start with file path %q", f.Name, relFile+":")
		}
		names[f.Name] = true
	}
	if !names[relFile+":belongs_to :user"] {
		t.Error("missing belongs_to :user with file prefix")
	}
	if !names[relFile+":has_many :items"] {
		t.Error("missing has_many :items with file prefix")
	}

	// has_many target is singularized + camelized; belongs_to is camelized as-is.
	for _, f := range result {
		if f.Name == relFile+":has_many :items" {
			if f.Relations[0].Target != "Item" {
				t.Errorf("has_many :items target = %q, want Item", f.Relations[0].Target)
			}
		}
		if f.Name == relFile+":belongs_to :user" {
			if f.Relations[0].Target != "User" {
				t.Errorf("belongs_to :user target = %q, want User", f.Relations[0].Target)
			}
		}
	}
}

// --- RelCalls extraction tests ---

func TestExtractFile_QualifiedClassMethodCall(t *testing.T) {
	src := `module Items
  class FetchService
    def call(ids)
      Items::Facade.fetch_item_fields(ids, ITEM_FIELDS)
    end
  end
end
`
	result := extractFileAST([]byte(src), "packages/items/app/services/fetch_service.rb", false, true)
	meth, ok := symbolsByName(result)["Items::FetchService#call"]
	if !ok {
		t.Fatal("missing method Items::FetchService#call")
	}
	if !hasCall(meth, "Items::Facade.fetch_item_fields") {
		t.Errorf("missing RelCalls -> Items::Facade.fetch_item_fields; relations = %v", meth.Relations)
	}
}

func TestExtractFile_MultiLevelNamespaceCall(t *testing.T) {
	src := `module HomepageSources
  class Builder
    def build(ids)
      HomepageSources::ItemDto.from_ids(ids)
    end
  end
end
`
	result := extractFileAST([]byte(src), "packages/homepage_sources/app/builder.rb", false, true)
	meth, ok := symbolsByName(result)["HomepageSources::Builder#build"]
	if !ok {
		t.Fatal("missing method HomepageSources::Builder#build")
	}
	if !hasCall(meth, "HomepageSources::ItemDto.from_ids") {
		t.Errorf("missing RelCalls -> HomepageSources::ItemDto.from_ids; relations = %v", meth.Relations)
	}
}

func TestExtractFile_ReceiverVariableCall(t *testing.T) {
	src := `class OrderProcessor
  def process(order)
    service.call(order)
  end
end
`
	result := extractFileAST([]byte(src), "app/models/order_processor.rb", false, true)
	meth, ok := symbolsByName(result)["OrderProcessor#process"]
	if !ok {
		t.Fatal("missing method OrderProcessor#process")
	}
	if !hasCall(meth, "service.call") {
		t.Errorf("missing RelCalls -> service.call; relations = %v", meth.Relations)
	}
}

func TestExtractFile_BareMethodCalls(t *testing.T) {
	src := `class PostsController
  def markdown_num(arg)
    render :json
    helper(arg)
    current_user
  end
end
`
	result := extractFileAST([]byte(src), "app/controllers/posts_controller.rb", true, true)
	meth, ok := symbolsByName(result)["PostsController#markdown_num"]
	if !ok {
		t.Fatal("missing method PostsController#markdown_num")
	}
	for _, want := range []string{"render", "helper", "current_user"} {
		if !hasCall(meth, want) {
			t.Errorf("missing bare RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
	if hasCall(meth, "arg") {
		t.Errorf("parameter 'arg' must not be emitted as a call; relations = %v", meth.Relations)
	}
}

// TestExtractFile_BareConstantReferences checks that a constant used as a value
// (registered, passed as an argument, matched in case/when, in an array) is
// recorded as a RelCalls edge so the referenced class/module is not mis-reported
// as dead code. scope_resolution paths are recorded whole.
func TestExtractFile_BareConstantReferences(t *testing.T) {
	src := `class Registry
  def wire
    register(MyJob)
    handlers = [FooHandler, BarHandler]
    klass = Chat::Message
    case obj
    when SomeError
      retry
    end
  end
end
`
	result := extractFileAST([]byte(src), "app/services/registry.rb", false, true)
	meth, ok := symbolsByName(result)["Registry#wire"]
	if !ok {
		t.Fatal("missing method Registry#wire")
	}
	for _, want := range []string{"MyJob", "FooHandler", "BarHandler", "Chat::Message", "SomeError"} {
		if !hasCall(meth, want) {
			t.Errorf("missing bare-constant RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
	// The bare constant target carries no ".", so it is not a coupling-graph
	// "Recv.method" form — guard that we did not accidentally emit one.
	if hasCall(meth, "MyJob.register") {
		t.Errorf("bare constant must not become a Recv.method target; relations = %v", meth.Relations)
	}
}

// TestExtractFile_ConstantReceiverCallStillQualified checks that the bare-constant
// capture does not regress the qualified-call form: a Const.method call must still
// produce the "Const.method" edge (for coupling), now alongside a bare "Const" one.
func TestExtractFile_ConstantReceiverCallStillQualified(t *testing.T) {
	src := `class Builder
  def run(ids)
    Items::Facade.fetch(ids)
  end
end
`
	result := extractFileAST([]byte(src), "app/services/builder.rb", false, true)
	meth := symbolsByName(result)["Builder#run"]
	if !hasCall(meth, "Items::Facade.fetch") {
		t.Errorf("qualified call edge lost; relations = %v", meth.Relations)
	}
	if !hasCall(meth, "Items::Facade") {
		t.Errorf("missing bare-receiver constant edge; relations = %v", meth.Relations)
	}
}

// TestExtractFile_BuiltinConstantsSkipped checks that bare references to Ruby
// core/stdlib constants are not emitted as call edges (they inflate fan-in on
// monkey-patch reopenings), while application constants still are.
func TestExtractFile_BuiltinConstantsSkipped(t *testing.T) {
	src := `class Worker
  def run
    Array.new
    x = [String, Time]
    enqueue(MyJob)
  end
end
`
	result := extractFileAST([]byte(src), "app/services/worker.rb", false, true)
	meth := symbolsByName(result)["Worker#run"]
	for _, skip := range []string{"Array", "String", "Time"} {
		if hasCall(meth, skip) {
			t.Errorf("builtin constant %s must not be emitted as a call edge; relations = %v", skip, meth.Relations)
		}
	}
	if !hasCall(meth, "MyJob") {
		t.Errorf("application constant MyJob should still be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_SerializerAttributeFold checks that a serializer's attribute and
// association DSL folds the backing methods (and the include_<name>? predicate) in
// as references on the serializer class, so they are not mis-reported as dead.
func TestExtractFile_SerializerAttributeFold(t *testing.T) {
	src := `class PostSerializer < ApplicationSerializer
  attributes :cooked, :score
  has_one :user
  def cooked
    object.cooked
  end
  def include_score?
    scope.admin?
  end
end
`
	result := extractFileAST([]byte(src), "app/serializers/post_serializer.rb", true, true)
	cls, ok := symbolsByName(result)["PostSerializer"]
	if !ok {
		t.Fatal("missing class PostSerializer")
	}
	for _, want := range []string{"cooked", "include_cooked?", "score", "include_score?", "user", "include_user?"} {
		if !hasCall(cls, want) {
			t.Errorf("missing serializer DSL fold RelCalls -> %s; relations = %v", want, cls.Relations)
		}
	}
}

// TestExtractFile_NonSerializerHasManyUnaffected guards that has_many on a model
// (not a serializer) still produces an association dependency fact and is NOT
// short-circuited by the serializer fold.
func TestExtractFile_NonSerializerHasManyUnaffected(t *testing.T) {
	src := `class User < ApplicationRecord
  has_many :posts
end
`
	result := extractFileAST([]byte(src), "app/models/user.rb", true, true)
	var sawAssoc bool
	for _, f := range result {
		if f.Kind == facts.KindDependency {
			for _, r := range f.Relations {
				if r.Kind == facts.RelDependsOn && r.Target == "Post" {
					sawAssoc = true
				}
			}
		}
	}
	if !sawAssoc {
		t.Error("model has_many :posts should still emit a depends_on association fact (serializer fold must not swallow it)")
	}
}

func TestExtractFile_BareCallSkipsLocalsAndKeywords(t *testing.T) {
	src := `class A
  def b
    x = compute
    x
    raise
    super
  end
end
`
	result := extractFileAST([]byte(src), "app/models/a.rb", false, true)
	meth := symbolsByName(result)["A#b"]
	if !hasCall(meth, "compute") {
		t.Errorf("missing RelCalls -> compute; relations = %v", meth.Relations)
	}
	for _, bad := range []string{"x", "raise", "super"} {
		if hasCall(meth, bad) {
			t.Errorf("%s must not be emitted as a call; relations = %v", bad, meth.Relations)
		}
	}
}

func TestExtractFile_RailsCallbackSymbolCalls(t *testing.T) {
	src := `class PostsController < ApplicationController
  before_action :authenticate_user!
  validate :check_something

  def authenticate_user!
  end
end
`
	result := extractFileAST([]byte(src), "app/controllers/posts_controller.rb", true, true)
	cls, ok := symbolsByName(result)["PostsController"]
	if !ok {
		t.Fatal("missing class PostsController")
	}
	for _, want := range []string{"authenticate_user!", "check_something"} {
		if !hasCall(cls, want) {
			t.Errorf("missing DSL RelCalls -> %s on class; relations = %v", want, cls.Relations)
		}
	}
}

func TestExtractFile_CallsDeduplication(t *testing.T) {
	src := `class Dispatcher
  def run(ids)
    Items::Facade.fetch_item_fields(ids, FIELDS)
    Items::Facade.fetch_item_fields(ids, OTHER_FIELDS)
  end
end
`
	result := extractFileAST([]byte(src), "app/dispatcher.rb", false, true)
	meth, ok := symbolsByName(result)["Dispatcher#run"]
	if !ok {
		t.Fatal("missing method Dispatcher#run")
	}
	count := 0
	for _, r := range meth.Relations {
		if r.Kind == facts.RelCalls && r.Target == "Items::Facade.fetch_item_fields" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 RelCalls edge, got %d", count)
	}
}

func TestExtractFile_TopLevelMethodCalls(t *testing.T) {
	// Ruby allows method calls without parentheses; the qualified tier must still
	// capture them.
	src := `def bootstrap
  Config.load_defaults
  Rails.application.initialize!
end
`
	result := extractFileAST([]byte(src), "config/init.rb", false, true)
	meth, ok := symbolsByName(result)["config.bootstrap"]
	if !ok {
		t.Fatal("missing top-level method config.bootstrap")
	}
	if !hasCall(meth, "Config.load_defaults") {
		t.Errorf("missing RelCalls -> Config.load_defaults; relations = %v", meth.Relations)
	}
	if !hasCall(meth, "Rails.application") {
		t.Errorf("missing RelCalls -> Rails.application; relations = %v", meth.Relations)
	}
}

func TestExtractFile_EndlessMethodCall(t *testing.T) {
	// Ruby 3.0+ endless method: def name(args) = Expr.call(args)
	src := `module HomepageSources
  class ItemDto
    ITEM_FIELDS = %i[id title].freeze

    def fields_by_id(item_ids) = Items::Facade.fetch_item_fields(item_ids, ITEM_FIELDS).index_by { |item| item[:id] }
  end
end
`
	result := extractFileAST([]byte(src), "packages/homepage_sources/app/public/homepage_sources/item_dto.rb", false, true)
	byName := symbolsByName(result)

	meth, ok := byName["HomepageSources::ItemDto#fields_by_id"]
	if !ok {
		t.Fatal("missing method HomepageSources::ItemDto#fields_by_id")
	}
	if !hasCall(meth, "Items::Facade.fetch_item_fields") {
		t.Errorf("missing RelCalls -> Items::Facade.fetch_item_fields; relations = %v", meth.Relations)
	}
	// The ALL-CAPS constant should be captured.
	if _, ok := byName["HomepageSources::ItemDto::ITEM_FIELDS"]; !ok {
		t.Error("missing constant HomepageSources::ItemDto::ITEM_FIELDS")
	}
}

func TestCallEdges_QualifiedAndReceiverAndChain(t *testing.T) {
	src := `class Logger
  def run(x)
    Items::Facade.fetch_item_fields(x)
    service.call(x)
    Rails.logger.info("msg")
  end
end
`
	result := extractFileAST([]byte(src), "app/logger.rb", false, true)
	meth := symbolsByName(result)["Logger#run"]
	for _, want := range []string{
		"Items::Facade.fetch_item_fields", // scope-resolution receiver
		"service.call",                    // lowercase receiver with args
		"Rails.logger",                    // qualified inner of a chain
		"logger.info",                     // chained receiver with args
	} {
		if !hasCall(meth, want) {
			t.Errorf("missing RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
}

// --- AST-only coverage (cases the regex scanner handled poorly) ---

func TestAST_MultiLineCallArguments(t *testing.T) {
	src := `class Svc
  def run(ids)
    Items::Facade.fetch_item_fields(
      ids,
      FIELDS,
    )
  end
end
`
	result := extractFileAST([]byte(src), "app/svc.rb", false, true)
	meth := symbolsByName(result)["Svc#run"]
	if !hasCall(meth, "Items::Facade.fetch_item_fields") {
		t.Errorf("multi-line call not captured; relations = %v", meth.Relations)
	}
}

func TestAST_HeredocContainingEnd(t *testing.T) {
	// A heredoc body containing a bare "end" line used to corrupt the regex
	// depth counter; the grammar treats it as string content.
	src := "class Foo\n" +
		"  def bar\n" +
		"    sql = <<~SQL\n" +
		"      SELECT 1\n" +
		"      end\n" +
		"    SQL\n" +
		"    Other.call(sql)\n" +
		"  end\n" +
		"end\n"
	result := extractFileAST([]byte(src), "app/foo.rb", false, true)
	byName := symbolsByName(result)
	if _, ok := byName["Foo"]; !ok {
		t.Fatal("missing class Foo")
	}
	meth, ok := byName["Foo#bar"]
	if !ok {
		t.Fatal("missing method Foo#bar (heredoc likely broke scope tracking)")
	}
	if !hasCall(meth, "Other.call") {
		t.Errorf("call after heredoc not captured; relations = %v", meth.Relations)
	}
}

func TestAST_NestedModulesAndEigenclass(t *testing.T) {
	src := `module A
  module B
    class C
      class << self
        def build
        end
      end
    end
  end
end
`
	result := extractFileAST([]byte(src), "app/a.rb", false, true)
	byName := symbolsByName(result)
	if _, ok := byName["A::B::C"]; !ok {
		t.Fatal("missing deeply nested class A::B::C")
	}
	build, ok := byName["A::B::C.build"]
	if !ok {
		t.Fatal("missing eigenclass method A::B::C.build")
	}
	if sk, _ := build.Props["symbol_kind"].(string); sk != facts.SymbolFunc {
		t.Errorf("eigenclass method symbol_kind = %q, want func", sk)
	}
}

func TestAST_ConcernDetection(t *testing.T) {
	src := `module Trackable
  extend ActiveSupport::Concern
end
`
	result := extractFileAST([]byte(src), "app/models/concerns/trackable.rb", true, true)
	mod := symbolsByName(result)["Trackable"]
	if c, _ := mod.Props["concern"].(bool); !c {
		t.Errorf("Trackable should be flagged concern:true; props = %v", mod.Props)
	}
	// A Concern is a mixin → abstract for package metrics.
	if ab, _ := mod.Props["abstract"].(bool); !ab {
		t.Errorf("Concern Trackable should be abstract:true; props = %v", mod.Props)
	}
	// extend ActiveSupport::Concern must not be emitted as a mixin dependency.
	for _, f := range result {
		if f.Kind == facts.KindDependency {
			for _, r := range f.Relations {
				if r.Target == "ActiveSupport::Concern" {
					t.Error("ActiveSupport::Concern should not be a mixin dependency")
				}
			}
		}
	}
}

// moduleAbstract returns the `abstract` prop of the named module symbol.
func moduleAbstract(t *testing.T, result []facts.Fact, name string) bool {
	t.Helper()
	mod, ok := symbolsByName(result)[name]
	if !ok {
		t.Fatalf("module %q not found among symbols", name)
	}
	ab, _ := mod.Props["abstract"].(bool)
	return ab
}

func TestAST_ModuleAbstractness(t *testing.T) {
	// A namespace module (only nested class/module defs) is concrete.
	nsResult := extractFileAST([]byte("module Api\n  module V2\n    class Foo\n    end\n  end\nend\n"),
		"app/controllers/api/v2/foo.rb", true, false)
	if moduleAbstract(t, nsResult, "Api") {
		t.Error("namespace module Api should be abstract:false")
	}
	if moduleAbstract(t, nsResult, "Api::V2") {
		t.Error("namespace module Api::V2 should be abstract:false")
	}

	// A mixin module (defines an instance method) is abstract.
	mixinResult := extractFileAST([]byte("module Greetable\n  def greet\n    \"hi\"\n  end\nend\n"),
		"app/models/concerns/greetable.rb", true, false)
	if !moduleAbstract(t, mixinResult, "Greetable") {
		t.Error("mixin module Greetable should be abstract:true")
	}

	// A utility module with only class methods (`def self.x`) is concrete.
	utilResult := extractFileAST([]byte("module PhoneUtils\n  def self.format(n)\n    n\n  end\nend\n"),
		"lib/phone_utils.rb", false, false)
	if moduleAbstract(t, utilResult, "PhoneUtils") {
		t.Error("class-method-only module PhoneUtils should be abstract:false")
	}

	// module_function makes subsequent defs class methods → still concrete.
	mfResult := extractFileAST([]byte("module M\n  module_function\n  def helper\n    1\n  end\nend\n"),
		"lib/m.rb", false, false)
	if moduleAbstract(t, mfResult, "M") {
		t.Error("module_function module M should be abstract:false")
	}

	// A mixed module (nested class AND an instance method) is abstract — the
	// instance method wins.
	mixedResult := extractFileAST([]byte("module M\n  class Inner\n  end\n  def instance_m\n    1\n  end\nend\n"),
		"app/services/m.rb", false, false)
	if !moduleAbstract(t, mixedResult, "M") {
		t.Error("mixed module M with an instance method should be abstract:true")
	}

	// An empty module is a pure namespace → concrete.
	emptyResult := extractFileAST([]byte("module Empty\nend\n"), "lib/empty.rb", false, false)
	if moduleAbstract(t, emptyResult, "Empty") {
		t.Error("empty module Empty should be abstract:false")
	}
}

func TestAST_MixinsAndImports(t *testing.T) {
	src := `class Account < ApplicationRecord
  include Trackable
  prepend Auditable
  attr_accessor :name, :token
  require "set"
  require_relative "../helper"
end
`
	result := extractFileAST([]byte(src), "app/models/account.rb", true, true)

	var includeKind, prependKind, reqRel string
	attrs := map[string]bool{}
	imports := map[string]bool{}
	for _, f := range result {
		if f.Kind == facts.KindDependency {
			if mk, _ := f.Props["mixin_kind"].(string); mk != "" {
				if f.Relations[0].Target == "Trackable" {
					includeKind = mk
				}
				if f.Relations[0].Target == "Auditable" {
					prependKind = mk
				}
			}
			if rr, _ := f.Props["require_relative"].(bool); rr {
				reqRel = f.Relations[0].Target
			}
			for _, r := range f.Relations {
				if r.Kind == facts.RelImports {
					imports[r.Target] = true
				}
			}
		}
		if f.Kind == facts.KindSymbol {
			if ak, _ := f.Props["attr_kind"].(string); ak == "accessor" {
				attrs[f.Name] = true
			}
		}
	}
	if includeKind != "include" {
		t.Errorf("include mixin_kind = %q, want include", includeKind)
	}
	if prependKind != "prepend" {
		t.Errorf("prepend mixin_kind = %q, want prepend", prependKind)
	}
	if reqRel != "../helper" {
		t.Errorf("require_relative target = %q, want ../helper", reqRel)
	}
	if !imports["set"] {
		t.Error("missing require 'set' import")
	}
	if !attrs["Account#name"] || !attrs["Account#token"] {
		t.Errorf("missing attr_accessor symbols; got %v", attrs)
	}
}

// --- route tests ---

func TestRoutes_NestedNamespaceResourcesMember(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :admin do
    resources :users, only: [:index, :show] do
      member do
        post "ban"
      end
    end
  end
  draw(:billing)
end
`
	result := parseRouteFileAST([]byte(src), "config/routes.rb")
	names := make(map[string]facts.Fact)
	for _, f := range result {
		if f.Kind == facts.KindRoute {
			names[f.Name] = f
		}
	}

	for _, want := range []string{"/admin/users", "/admin/users/:id", "/admin/users/:id/ban", "/billing"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing route %q; got %v", want, keys(names))
		}
	}
	// only: [:index, :show] must exclude create/new/edit/destroy.
	for _, absent := range []string{"/admin/users/new", "/admin/users/:id/edit"} {
		if _, ok := names[absent]; ok {
			t.Errorf("route %q should be excluded by only:", absent)
		}
	}
	if f, ok := names["/admin/users/:id/ban"]; ok {
		if m, _ := f.Props["method"].(string); m != "POST" {
			t.Errorf("ban route method = %q, want POST", m)
		}
	}
	if f, ok := names["/billing"]; ok {
		if m, _ := f.Props["method"].(string); m != "DRAW" {
			t.Errorf("draw route method = %q, want DRAW", m)
		}
	}
}

// routeMethods indexes route facts as name -> set of HTTP methods (a singular
// resource emits several verbs on one path, so a plain name->fact map would drop them).
func routeMethods(result []facts.Fact) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, f := range result {
		if f.Kind != facts.KindRoute {
			continue
		}
		if out[f.Name] == nil {
			out[f.Name] = map[string]bool{}
		}
		if m, _ := f.Props["method"].(string); m != "" {
			out[f.Name][m] = true
		}
	}
	return out
}

// TestRoutes_NestedSingularResource: a singular `resource` nested in a plural
// `resources` nests under the parent member (`/:<singular>_id`) and has no id of its
// own — show/create/destroy all map to the base path.
func TestRoutes_NestedSingularResource(t *testing.T) {
	src := `Rails.application.routes.draw do
  resources :widgets, only: [:show] do
    resource :follow, only: [:show, :create, :destroy]
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))

	if _, ok := routes["/widgets/:id"]; !ok {
		t.Errorf("missing parent route /widgets/:id; got %v", routes)
	}
	follow := routes["/widgets/:widget_id/follow"]
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if !follow[m] {
			t.Errorf("follow: missing %s among %v (route /widgets/:widget_id/follow)", m, follow)
		}
	}
	// The old buggy shapes must NOT appear.
	for _, absent := range []string{"/widgets/follow", "/widgets/follow/:id", "/widgets/:widget_id/follow/:id"} {
		if _, ok := routes[absent]; ok {
			t.Errorf("route %q should not be produced (nested singular resource)", absent)
		}
	}
}

// TestRoutes_SymbolPathArg: `get :sym` inside a collection block is emitted (symbol
// path arg), and a `to:` handler string is not mistaken for the path.
func TestRoutes_SymbolPathArg(t *testing.T) {
	src := `Rails.application.routes.draw do
  resource :registration_helper, only: [] do
    collection do
      get :cities_by_zip
      post :valid_attributes
    end
  end
  get :standalone, to: 'foo#bar'
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	if !routes["/registration_helper/cities_by_zip"]["GET"] {
		t.Errorf("symbol collection route missing; got %v", routes)
	}
	if !routes["/registration_helper/valid_attributes"]["POST"] {
		t.Errorf("symbol collection POST missing; got %v", routes)
	}
	if !routes["/standalone"]["GET"] {
		t.Errorf("symbol path with to: should be /standalone; got %v", routes)
	}
	// The handler string must not become a route path.
	if _, bad := routes["/foo#bar"]; bad {
		t.Errorf("to: handler leaked into route path: %v", routes)
	}
}

// TestRoutes_ScopeBareSymbolPrefix: `scope :users` adds a /users path prefix; a
// keyword-only `scope module:` adds none.
func TestRoutes_ScopeBareSymbolPrefix(t *testing.T) {
	src := `Rails.application.routes.draw do
  scope :users do
    post 'email_sign_in', to: 'magic_link#create'
  end
  scope module: :api do
    get 'ping'
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	if !routes["/users/email_sign_in"]["POST"] {
		t.Errorf("scope :users prefix missing; got %v", routes)
	}
	if !routes["/ping"]["GET"] {
		t.Errorf("scope module: should not add a path prefix; got %v", routes)
	}
}

// TestRoutes_ResourcePathOverride: `resource ..., path:` overrides the URL segment
// while nesting param stays derived from the resource name.
func TestRoutes_ResourcePathOverride(t *testing.T) {
	src := `Rails.application.routes.draw do
  resource :static_page, only: [:show], path: 'nebenpage'
  resources :photos, path: 'images' do
    resources :comments, only: [:index]
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	if !routes["/nebenpage"]["GET"] {
		t.Errorf("resource path: override missing /nebenpage; got %v", routes)
	}
	if _, bad := routes["/static_page"]; bad {
		t.Errorf("un-overridden /static_page must not be emitted: %v", routes)
	}
	if !routes["/images/:photo_id/comments"]["GET"] {
		t.Errorf("nested route should use path segment + name-derived param; got %v", routes)
	}
}

// TestRoutes_ResourcesUpdatePutAndPatch: a resources/resource update action serves
// both PATCH and PUT (Rails routes both verbs to update), so both are emitted.
func TestRoutes_ResourcesUpdatePutAndPatch(t *testing.T) {
	src := `Rails.application.routes.draw do
  resources :widgets, only: [:update]
  resource :profile, only: [:update]
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	for _, m := range []string{"PATCH", "PUT"} {
		if !routes["/widgets/:id"][m] {
			t.Errorf("resources update missing %s; got %v", m, routes["/widgets/:id"])
		}
		if !routes["/profile"][m] {
			t.Errorf("singular resource update missing %s; got %v", m, routes["/profile"])
		}
	}
}

// TestRoutes_MatchViaVerbs: `match ... via:` emits one route per listed verb
// (array or single symbol), and nothing when via: is absent.
func TestRoutes_MatchViaVerbs(t *testing.T) {
	src := `Rails.application.routes.draw do
  match 'search', to: 'search#index', via: [:get, :post]
  match 'ping', via: :get
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	search := routes["/search"]
	for _, m := range []string{"GET", "POST"} {
		if !search[m] {
			t.Errorf("match via: missing %s for /search; got %v", m, search)
		}
	}
	if !routes["/ping"]["GET"] {
		t.Errorf("match via: :get missing GET for /ping; got %v", routes["/ping"])
	}
}

// TestRoutes_ScopePathKeyword: a `scope path:`/`namespace path:` keyword sets the
// URL prefix (independently of module:).
func TestRoutes_ScopePathKeyword(t *testing.T) {
	src := `Rails.application.routes.draw do
  scope path: 'api', module: :api do
    get 'health'
  end
  namespace :admin, path: 'administration' do
    get 'dashboard'
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	if !routes["/api/health"]["GET"] {
		t.Errorf("scope path: 'api' should prefix child route; got %v", routes)
	}
	if !routes["/administration/dashboard"]["GET"] {
		t.Errorf("namespace path: override should apply; got %v", routes)
	}
}

// TestRoutes_NestedPluralResources: a plural `resources` nested in a plural
// `resources` nests under the parent member id.
func TestRoutes_NestedPluralResources(t *testing.T) {
	src := `Rails.application.routes.draw do
  resources :widgets do
    resources :items, only: [:index, :show]
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	for _, want := range []string{"/widgets/:widget_id/items", "/widgets/:widget_id/items/:id"} {
		if _, ok := routes[want]; !ok {
			t.Errorf("missing route %q; got %v", want, routes)
		}
	}
	if _, ok := routes["/widgets/items"]; ok {
		t.Errorf("nested plural resources must nest under the parent member id")
	}
}

// TestRoutes_TopLevelSingularResource: a top-level singular `resource` has no id.
func TestRoutes_TopLevelSingularResource(t *testing.T) {
	src := `Rails.application.routes.draw do
  resource :session, only: [:show, :create, :destroy]
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	session := routes["/session"]
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if !session[m] {
			t.Errorf("session: missing %s among %v", m, session)
		}
	}
	if _, ok := routes["/session/:id"]; ok {
		t.Errorf("singular resource must not have an :id member path")
	}
}

// TestRoutes_DrawPrefixSeeding verifies that (1) a top-level routes.rb reports the
// prefix each draw(:pkg) is scoped under, and (2) parsing a delegated file seeded
// with that prefix yields fully-qualified routes that a client call can match.
func TestRoutes_DrawPrefixSeeding(t *testing.T) {
	main := `Rails.application.routes.draw do
  scope '/api', defaults: { format: 'json' } do
    namespace(:core) do
      namespace(:v3) do
        draw(:api_core_v3_routes)
      end
    end
    namespace(:v2) do
      draw(:api_v2_routes)
    end
  end
  draw(:admin_routes)
end
`
	_, res := parseRouteFile([]byte(main), "config/routes.rb", "", "", jsonapiContext{format: jsonapiRouteDasherized})
	draws := res.draws
	if draws["api_core_v3_routes"] != "/api/core/v3" {
		t.Errorf("api_core_v3_routes prefix = %q, want /api/core/v3", draws["api_core_v3_routes"])
	}
	if draws["api_v2_routes"] != "/api/v2" {
		t.Errorf("api_v2_routes prefix = %q, want /api/v2", draws["api_v2_routes"])
	}
	if draws["admin_routes"] != "" {
		t.Errorf("admin_routes prefix = %q, want empty", draws["admin_routes"])
	}

	// A delegated file parsed with the learned prefix produces qualified routes,
	// including single-segment collections lifted above the 2-segment match floor.
	sub := `resources :devices, only: [:destroy]
resources :interactions, only: [:index]
namespace :event do
  resources :posts, only: [:create]
end
`
	ff, _ := parseRouteFile([]byte(sub), "config/routes/api_v2_routes.rb", draws["api_v2_routes"], "", jsonapiContext{format: jsonapiRouteDasherized})
	names := map[string]bool{}
	for _, f := range ff {
		names[f.Name] = true
	}
	for _, want := range []string{"/api/v2/devices/:id", "/api/v2/interactions", "/api/v2/event/posts"} {
		if !names[want] {
			t.Errorf("missing seeded route %q; got %v", want, keys2(names))
		}
	}
}

func keys2(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRoutes_VerbWithHandler(t *testing.T) {
	src := `Rails.application.routes.draw do
  root to: "home#index"
  get "/health", to: "health#show"
end
`
	result := parseRouteFileAST([]byte(src), "config/routes.rb")
	byName := make(map[string]facts.Fact)
	for _, f := range result {
		byName[f.Name] = f
	}
	root, ok := byName["/"]
	if !ok {
		t.Fatal("missing root route")
	}
	if h, _ := root.Props["handler"].(string); h != "home#index" {
		t.Errorf("root handler = %q, want home#index", h)
	}
	health, ok := byName["/health"]
	if !ok {
		t.Fatal("missing /health route")
	}
	if h, _ := health.Props["handler"].(string); h != "health#show" {
		t.Errorf("/health handler = %q, want health#show", h)
	}
	if m, _ := health.Props["method"].(string); m != "GET" {
		t.Errorf("/health method = %q, want GET", m)
	}
}

func keys(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPackwerk_RootDependencyNormalization(t *testing.T) {
	dir := t.TempDir()

	packwerkYml := `package_paths:
  - "."
  - "packages/*"
`
	if err := os.WriteFile(filepath.Join(dir, "packwerk.yml"), []byte(packwerkYml), 0o644); err != nil {
		t.Fatal(err)
	}
	rootPkg := `enforce_dependencies: true
`
	if err := os.WriteFile(filepath.Join(dir, "package.yml"), []byte(rootPkg), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(dir, "packages", "orders")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ordersPkg := `enforce_dependencies: true
dependencies:
  - "."
  - "packages/payments"
`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.yml"), []byte(ordersPkg), 0o644); err != nil {
		t.Fatal(err)
	}

	info := parsePackwerk(dir)
	if !info.detected {
		t.Fatal("packwerk should be detected")
	}

	var ordersFact *facts.Fact
	for i, f := range info.facts {
		if f.Name == "packages/orders" {
			ordersFact = &info.facts[i]
			break
		}
	}
	if ordersFact == nil {
		t.Fatal("missing packages/orders module fact")
	}

	hasDotTarget := false
	hasRootTarget := false
	for _, r := range ordersFact.Relations {
		if r.Kind == facts.RelDependsOn {
			if r.Target == "." {
				hasDotTarget = true
			}
			if r.Target == "root" {
				hasRootTarget = true
			}
		}
	}
	if hasDotTarget {
		t.Error("dependency target '.' should have been normalized to 'root'")
	}
	if !hasRootTarget {
		t.Error("expected dependency target 'root' after normalization")
	}

	var rootFact *facts.Fact
	for i, f := range info.facts {
		if f.Name == "root" {
			rootFact = &info.facts[i]
			break
		}
	}
	if rootFact == nil {
		t.Fatal("missing root module fact (should be named 'root', not '.')")
	}
}

// testRefFact returns the single KindTestRef fact from extractTestRefsAST output.
func testRefFact(result []facts.Fact) (facts.Fact, bool) {
	for _, f := range result {
		if f.Kind == facts.KindTestRef {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// TestExtractFile_CustomClassMacroRecorded checks that a bare-receiver class-body
// call (a custom Rails DSL macro like `requires_login` / `cluster_concurrency`)
// records the MACRO NAME as a use on the enclosing class, so the base-class method
// backing the macro is not mis-reported as dead. The target must be bare (no "."),
// so it never pollutes the packwerk coupling graph.
func TestExtractFile_CustomClassMacroRecorded(t *testing.T) {
	src := `class ReportsController < ApplicationController
  requires_login except: [:index]
  cluster_concurrency 1
end
`
	result := extractFileAST([]byte(src), "app/controllers/reports_controller.rb", true, true)
	cls, ok := symbolsByName(result)["ReportsController"]
	if !ok {
		t.Fatal("missing class ReportsController")
	}
	for _, want := range []string{"requires_login", "cluster_concurrency"} {
		if !hasCall(cls, want) {
			t.Errorf("missing custom macro RelCalls -> %s; relations = %v", want, cls.Relations)
		}
	}
	if hasCall(cls, "ReportsController.requires_login") {
		t.Errorf("macro name must be bare, not a coupling form; relations = %v", cls.Relations)
	}
}

// TestExtractFile_StructuralMacrosNotRecordedAsCalls guards that structural
// keywords (require/private) are NOT recorded as macro call edges.
func TestExtractFile_StructuralMacrosNotRecordedAsCalls(t *testing.T) {
	src := `class Thing
  require "set"
  private
end
`
	result := extractFileAST([]byte(src), "app/models/thing.rb", false, true)
	cls := symbolsByName(result)["Thing"]
	for _, skip := range []string{"require", "private"} {
		if hasCall(cls, skip) {
			t.Errorf("structural keyword %s must not be a macro edge; relations = %v", skip, cls.Relations)
		}
	}
}

// TestExtractFile_TopLevelMacroNoSymbolEdge checks a body call outside any class
// attaches to no symbol fact (there is no enclosing class to hang it on).
func TestExtractFile_TopLevelMacroNoSymbolEdge(t *testing.T) {
	src := `configure_app foo: 1
`
	result := extractFileAST([]byte(src), "config/init.rb", false, true)
	for _, f := range result {
		if f.Kind == facts.KindSymbol && hasCall(f, "configure_app") {
			t.Errorf("top-level macro must not attach to any symbol fact; fact = %v", f)
		}
	}
}

// TestExtractFile_SelfAndSelfClassReceivers checks that `self.foo` and
// `self.class.bar` dispatch is resolved to the bare method name, while a bare
// `self.class` receiver does not emit a spurious `class` edge.
func TestExtractFile_SelfAndSelfClassReceivers(t *testing.T) {
	src := `class Job
  def run
    do_it if self.class.perform_when_readonly?
    self.reset
  end
end
`
	result := extractFileAST([]byte(src), "app/jobs/job.rb", false, true)
	meth := symbolsByName(result)["Job#run"]
	for _, want := range []string{"perform_when_readonly?", "reset"} {
		if !hasCall(meth, want) {
			t.Errorf("missing self-receiver RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
	if hasCall(meth, "class") {
		t.Errorf("self.class receiver must not emit a bare 'class' edge; relations = %v", meth.Relations)
	}
	if hasCall(meth, "class.perform_when_readonly?") {
		t.Errorf("self.class.X must resolve to a bare method, not class.X; relations = %v", meth.Relations)
	}
}

// TestExtractFile_SelfBangAndPredicatePreserved checks predicate/bang suffixes
// survive self-receiver resolution.
func TestExtractFile_SelfBangAndPredicatePreserved(t *testing.T) {
	src := `class Model
  def process
    self.save!
    return unless self.valid?
  end
end
`
	result := extractFileAST([]byte(src), "app/models/model.rb", false, true)
	meth := symbolsByName(result)["Model#process"]
	for _, want := range []string{"save!", "valid?"} {
		if !hasCall(meth, want) {
			t.Errorf("missing self predicate/bang RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
}

// TestExtractTestRefsAST checks the reference-only spec pass emits a single
// KindTestRef fact carrying the production symbols the spec exercises (qualified
// receivers fold to bare method via the collector's lastSeg), and NO symbol facts.
func TestExtractTestRefsAST(t *testing.T) {
	src := `# frozen_string_literal: true
describe Badge do
  it "computes ids" do
    expect(Badge.trust_level_badge_ids).to eq([1, 2])
    GlobalSetting.reset_redis_config!
  end
end
`
	result := extractTestRefsAST([]byte(src), "spec/services/badge_granter_spec.rb")
	for _, f := range result {
		if f.Kind == facts.KindSymbol {
			t.Fatalf("test-ref pass must not emit symbol facts; got %v", f)
		}
	}
	ref, ok := testRefFact(result)
	if !ok {
		t.Fatal("missing KindTestRef fact")
	}
	for _, want := range []string{"Badge.trust_level_badge_ids", "GlobalSetting.reset_redis_config!"} {
		if !hasCall(ref, want) {
			t.Errorf("missing test ref RelCalls -> %s; relations = %v", want, ref.Relations)
		}
	}
}

// fileRefFact returns the single KindFileRef fact from extractFileAST output, if any.
func fileRefFact(result []facts.Fact) (facts.Fact, bool) {
	for _, f := range result {
		if f.Kind == facts.KindFileRef {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// countCalls returns how many RelCalls relations on a fact target the given name.
func countCalls(f facts.Fact, target string) int {
	n := 0
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == target {
			n++
		}
	}
	return n
}

// TestExtractFile_ClassBodySplatArgMethodCall checks that a method call in the
// ARGUMENT of a class-body macro (`requires_login *show_methods`) is captured on
// the class fact — not just the macro name — so the arg method is not flagged dead.
func TestExtractFile_ClassBodySplatArgMethodCall(t *testing.T) {
	src := `class TagsController < ApplicationController
  requires_login *show_methods
end
`
	result := extractFileAST([]byte(src), "app/controllers/tags_controller.rb", true, true)
	cls := symbolsByName(result)["TagsController"]
	for _, want := range []string{"requires_login", "show_methods"} {
		if !hasCall(cls, want) {
			t.Errorf("missing class-body RelCalls -> %s; relations = %v", want, cls.Relations)
		}
	}
	if hasCall(cls, "TagsController.show_methods") {
		t.Errorf("arg method must be bare, not a coupling form; relations = %v", cls.Relations)
	}
}

// TestExtractFile_ClassBodyQualifiedCall checks a qualified Const.method call in a
// class body attaches to the class fact (previously dropped by handleBodyCall).
func TestExtractFile_ClassBodyQualifiedCall(t *testing.T) {
	src := `class Report
  Badge.register(self)
end
`
	result := extractFileAST([]byte(src), "app/models/report.rb", false, true)
	cls := symbolsByName(result)["Report"]
	if !hasCall(cls, "Badge.register") {
		t.Errorf("missing class-body qualified RelCalls -> Badge.register; relations = %v", cls.Relations)
	}
}

// TestExtractFile_ClassBodyMacroNoDuplicate guards the Fix A / walkForCalls dedup:
// a class-body macro name is recorded exactly once on the class fact.
func TestExtractFile_ClassBodyMacroNoDuplicate(t *testing.T) {
	src := `class Thing
  requires_login except: [:index]
end
`
	result := extractFileAST([]byte(src), "app/controllers/thing_controller.rb", true, true)
	cls := symbolsByName(result)["Thing"]
	if got := countCalls(cls, "requires_login"); got != 1 {
		t.Errorf("requires_login recorded %d times, want exactly 1; relations = %v", got, cls.Relations)
	}
}

// TestExtractFile_TopLevelQualifiedCallOnFileRef checks a top-level (fixture-style)
// call is captured on a KindFileRef fact, leaks no symbol fact, and that a file with
// only ignorable top-level calls produces no file-ref fact.
func TestExtractFile_TopLevelQualifiedCallOnFileRef(t *testing.T) {
	result := extractFileAST([]byte("Badge.like_badge_counts(1, 2)\n"), "db/fixtures/006_badges.rb", false, true)
	fr, ok := fileRefFact(result)
	if !ok {
		t.Fatal("missing KindFileRef fact for a top-level call")
	}
	if !hasCall(fr, "Badge.like_badge_counts") {
		t.Errorf("missing file-ref RelCalls -> Badge.like_badge_counts; relations = %v", fr.Relations)
	}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && hasCall(f, "Badge.like_badge_counts") {
			t.Errorf("top-level call must not attach to a symbol fact; fact = %v", f)
		}
	}
	// A file whose only top-level call is a suppressed keyword produces no file-ref.
	empty := extractFileAST([]byte("require \"set\"\n"), "config/init.rb", false, true)
	if _, ok := fileRefFact(empty); ok {
		t.Error("file with no real top-level refs should not emit a KindFileRef fact")
	}
}

// TestExtractFile_TopLevelAfterInitializeBlock checks a call inside a top-level
// plugin block (after_initialize do ... end) is captured on the file-ref fact.
func TestExtractFile_TopLevelAfterInitializeBlock(t *testing.T) {
	src := `after_initialize do
  CategoryList.register_included_association(:foo)
end
`
	result := extractFileAST([]byte(src), "plugins/foo/plugin.rb", true, true)
	fr, ok := fileRefFact(result)
	if !ok {
		t.Fatal("missing KindFileRef fact for a top-level block call")
	}
	if !hasCall(fr, "CategoryList.register_included_association") {
		t.Errorf("missing file-ref RelCalls -> CategoryList.register_included_association; relations = %v", fr.Relations)
	}
}

// TestExtractFile_TopLevelAssignmentRHS checks that a call on the RHS of a
// top-level assignment (a Rails initializer pattern) is captured on the file-ref
// fact — previously dropped because handleAssignment never walked the value.
func TestExtractFile_TopLevelAssignmentRHS(t *testing.T) {
	src := `if Rails.configuration.multisite
  assets_hostnames = GlobalSetting.cdn_hostnames
end
`
	result := extractFileAST([]byte(src), "config/initializers/200-first_middlewares.rb", true, true)
	fr, ok := fileRefFact(result)
	if !ok {
		t.Fatal("missing KindFileRef fact for a top-level assignment RHS call")
	}
	if !hasCall(fr, "GlobalSetting.cdn_hostnames") {
		t.Errorf("missing file-ref RelCalls -> GlobalSetting.cdn_hostnames; relations = %v", fr.Relations)
	}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && hasCall(f, "GlobalSetting.cdn_hostnames") {
			t.Errorf("top-level assignment RHS call must not attach to a symbol fact; fact = %v", f)
		}
	}
}

// TestExtractFile_TopLevelSetterAssignmentRHS checks a call on the RHS of a
// top-level setter-assignment inside an if/else is captured.
func TestExtractFile_TopLevelSetterAssignmentRHS(t *testing.T) {
	src := `if Rails.env.test?
  MessageBus.configure(backend: :memory)
else
  MessageBus.redis_config = GlobalSetting.message_bus_redis_config
end
`
	result := extractFileAST([]byte(src), "config/initializers/004-message_bus.rb", true, true)
	fr, ok := fileRefFact(result)
	if !ok {
		t.Fatal("missing KindFileRef fact")
	}
	if !hasCall(fr, "GlobalSetting.message_bus_redis_config") {
		t.Errorf("missing file-ref RelCalls -> GlobalSetting.message_bus_redis_config; relations = %v", fr.Relations)
	}
}

// TestExtractFile_ClassBodyConstAssignmentProc checks that calls inside a Proc in
// a class-body constant assignment (Discourse's TYPE_FILTERS pattern) are captured
// on the class fact.
func TestExtractFile_ClassBodyConstAssignmentProc(t *testing.T) {
	src := `class GroupsController < ApplicationController
  TYPE_FILTERS = {
    my: Proc.new { |groups, user| Group.member_of(groups, user) },
    owner: Proc.new { |groups, user| Group.owner_of(groups, user) },
  }
end
`
	result := extractFileAST([]byte(src), "app/controllers/groups_controller.rb", true, true)
	cls := symbolsByName(result)["GroupsController"]
	for _, want := range []string{"Group.owner_of", "Group.member_of"} {
		if !hasCall(cls, want) {
			t.Errorf("missing class-body const-assignment Proc RelCalls -> %s; relations = %v", want, cls.Relations)
		}
	}
}

// fileRefPrefixes returns the dynamic_send_prefixes prop of the KindFileRef fact.
func fileRefPrefixes(result []facts.Fact) []string {
	for _, f := range result {
		if f.Kind == facts.KindFileRef {
			if raw, ok := f.Props["dynamic_send_prefixes"].([]string); ok {
				return raw
			}
		}
	}
	return nil
}

// TestExtractFile_InterpolatedSymbolPrefix checks that an interpolated symbol
// (`:"report_#{type}"`) — the mark of dynamic dispatch by computed name — records
// its static prefix on the file-scope fact.
func TestExtractFile_InterpolatedSymbolPrefix(t *testing.T) {
	src := `class IncomingLinksReport
  def self.find(type)
    report_method = :"report_#{type}"
    public_send(report_method, type)
  end
end
`
	result := extractFileAST([]byte(src), "app/models/incoming_links_report.rb", true, true)
	got := fileRefPrefixes(result)
	found := false
	for _, p := range got {
		if p == "report_" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dynamic_send_prefixes to contain %q; got %v", "report_", got)
	}
}

// TestExtractFile_NoPrefixFromStaticSymbolOrString checks the heuristic does NOT
// fire for static symbols, too-short prefixes, or interpolated STRINGS (i18n keys).
func TestExtractFile_NoPrefixFromStaticSymbolOrString(t *testing.T) {
	src := `class Thing
  def go(type)
    a = :report_foo                 # static symbol, no interpolation
    b = :"m#{type}"                 # prefix too short / no underscore
    c = I18n.t("reports.#{type}.x") # interpolated STRING, not a symbol
    [a, b, c]
  end
end
`
	result := extractFileAST([]byte(src), "app/models/thing.rb", true, true)
	if got := fileRefPrefixes(result); len(got) != 0 {
		t.Errorf("expected no dynamic_send_prefixes, got %v", got)
	}
}

// TestExtractFile_SuperReferencesAncestor checks that a `super` call records a
// reference to the same-named ancestor method (the base an override delegates to).
func TestExtractFile_SuperReferencesAncestor(t *testing.T) {
	src := `module EE
  module IssuesFinder
    def negatable_params
      @negatable_params ||= super + [:weight]
    end
  end
end
`
	result := extractFileAST([]byte(src), "ee/app/finders/ee/issues_finder.rb", true, true)
	meth := symbolsByName(result)["EE::IssuesFinder#negatable_params"]
	if !hasCall(meth, "negatable_params") {
		t.Errorf("super should record a call to the same-named ancestor method; relations = %v", meth.Relations)
	}
}

// TestExtractFile_LiteralSymbolDispatch checks that a literal-symbol argument to a
// dispatcher (try/send/respond_to?) records a call to the named method, while a
// non-dispatcher method with a symbol arg (e.g. validates :name) does not.
func TestExtractFile_LiteralSymbolDispatch(t *testing.T) {
	src := `class BaseField
  def complexity(resolver)
    ext = resolver&.try(:calculate_ext_conn_complexity)
    v = @resolver_class.send(:requires_argument?)
    validates :name
    [ext, v]
  end
end
`
	result := extractFileAST([]byte(src), "app/graphql/types/base_field.rb", true, true)
	meth := symbolsByName(result)["BaseField#complexity"]
	for _, want := range []string{"calculate_ext_conn_complexity", "requires_argument?"} {
		if !hasCall(meth, want) {
			t.Errorf("dispatcher literal-symbol arg should record RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
	// `validates :name` is a DSL, not a dispatcher — `name` must not be a call here.
	if hasCall(meth, "name") {
		t.Errorf("non-dispatcher symbol arg must not be recorded as a call; relations = %v", meth.Relations)
	}
}

// TestExtractFile_ChainedNoArgCall checks that a no-arg method call at the end of
// a chain (ActiveRecord scope / class-method chains) is captured, while common
// attribute/enumerable reads and single-level reads are not.
func TestExtractFile_ChainedNoArgCall(t *testing.T) {
	src := `class Worker
  def run
    DeployToken.active.with_owners.ordered_for_keyset_pagination
    merge_request.merge_request_closing_issues.preload_issue
    group_link.class.access_options
    user.name
    list.map.first
    a.b.count
  end
end
`
	result := extractFileAST([]byte(src), "app/workers/worker.rb", true, true)
	meth := symbolsByName(result)["Worker#run"]
	for _, want := range []string{"ordered_for_keyset_pagination", "preload_issue", "access_options"} {
		if !hasCall(meth, want) {
			t.Errorf("chained no-arg call should record RelCalls -> %s; relations = %v", want, meth.Relations)
		}
	}
	// Cheap chained reads (name/first/class are in rubyCheapMethods) and the
	// single-level read (user.name — var receiver) must NOT be recorded.
	for _, skip := range []string{"name", "first", "class"} {
		if hasCall(meth, skip) {
			t.Errorf("attribute/enumerable/single-level read %q must not be recorded as a chained call; relations = %v", skip, meth.Relations)
		}
	}
}

// TestIsRubyFile_Rake checks that .rake files and Rakefile are treated as Ruby.
func TestIsRubyFile_Rake(t *testing.T) {
	for _, p := range []string{"lib/tasks/gitlab/graphql_introspection.rake", "Rakefile"} {
		if !isRubyFile(p) {
			t.Errorf("isRubyFile(%q) = false, want true", p)
		}
	}
	if isRubyFile("app/models/foo.py") {
		t.Error("isRubyFile should be false for non-Ruby files")
	}
}

// TestExtractFile_RakeTaskCallCaptured checks a top-level call in a .rake file is
// recorded on the file-scope fact (so its target is not mis-reported as dead).
func TestExtractFile_RakeTaskCallCaptured(t *testing.T) {
	src := `namespace :gitlab do
  task introspection: :environment do
    puts CachedIntrospectionQuery.query_string_no_deprecated
  end
end
`
	result := extractFileAST([]byte(src), "lib/tasks/gitlab/graphql_introspection.rake", true, true)
	fr, ok := fileRefFact(result)
	if !ok || !hasCall(fr, "CachedIntrospectionQuery.query_string_no_deprecated") {
		t.Errorf("rake task call should be captured on the file-ref fact; result = %v", result)
	}
}

// TestExtractFile_DefaultParamCall checks a call in a method's default parameter
// value is recorded (previously only the body was walked).
func TestExtractFile_DefaultParamCall(t *testing.T) {
	src := `class BuildTraceChunk
  def unsafe_persist_data!(new_store = self.class.persistable_store)
    new_store.to_s
  end
end
`
	result := extractFileAST([]byte(src), "app/models/ci/build_trace_chunk.rb", true, true)
	meth := symbolsByName(result)["BuildTraceChunk#unsafe_persist_data!"]
	if !hasCall(meth, "persistable_store") {
		t.Errorf("default-parameter call should be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_PredicateBangSingleLevelCall checks that predicate/bang calls on
// a plain variable receiver are recorded (they are unambiguous method calls), while
// plain attribute reads and cheap predicates are not.
func TestExtractFile_PredicateBangSingleLevelCall(t *testing.T) {
	src := `class BlobHelper
  def show(viewer, record)
    x = viewer.rich?
    record.save!
    y = viewer.present?
    z = viewer.blob
    [x, y, z]
  end
end
`
	result := extractFileAST([]byte(src), "app/helpers/blob_helper.rb", true, true)
	meth := symbolsByName(result)["BlobHelper#show"]
	for _, want := range []string{"rich?", "save!"} {
		if !hasCall(meth, want) {
			t.Errorf("single-level predicate/bang call should be recorded -> %s; relations = %v", want, meth.Relations)
		}
	}
	for _, skip := range []string{"present?", "blob"} {
		if hasCall(meth, skip) {
			t.Errorf("cheap predicate / plain attribute read %q must not be recorded; relations = %v", skip, meth.Relations)
		}
	}
}

// TestExtractFile_DelegateFold checks that `delegate :a, :b, ..., to: :class`
// records the delegated method names as calls on the enclosing class, while the
// `to:` keyword value is not recorded.
func TestExtractFile_DelegateFold(t *testing.T) {
	src := `class BlobViewer
  delegate :rich?, :simple?, :loading_partial_path, to: :class
end
`
	result := extractFileAST([]byte(src), "app/models/blob_viewer/base.rb", true, true)
	cls := symbolsByName(result)["BlobViewer"]
	for _, want := range []string{"rich?", "simple?", "loading_partial_path"} {
		if !hasCall(cls, want) {
			t.Errorf("delegate should record RelCalls -> %s; relations = %v", want, cls.Relations)
		}
	}
	for _, skip := range []string{"to", "class"} {
		if hasCall(cls, skip) {
			t.Errorf("delegate keyword %q must not be recorded; relations = %v", skip, cls.Relations)
		}
	}
}

// TestExtractFile_BareMethodChainCall checks that a scope/class-method call on a
// bare-method (non-local identifier) receiver is captured when the name is
// scope-like (has `_`), while single-word attribute reads and local receivers are not.
func TestExtractFile_BareMethodChainCall(t *testing.T) {
	src := `class Service
  def run
    ordered_relation_scope.pluck_job_id.uniq
    current_user.email
    rel = base_scope
    rel.pluck_something
  end
end
`
	result := extractFileAST([]byte(src), "app/services/service.rb", true, true)
	meth := symbolsByName(result)["Service#run"]
	// Non-local bare method receiver AND local relation-variable receiver both record
	// their underscored scope-like calls.
	for _, want := range []string{"pluck_job_id", "pluck_something"} {
		if !hasCall(meth, want) {
			t.Errorf("scope-like call should record %s; relations = %v", want, meth.Relations)
		}
	}
	// current_user.email: single-word (no underscore) -> not recorded.
	if hasCall(meth, "email") {
		t.Errorf("single-word attribute read email must not be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_LocalRelationScopeCall checks the exact GitLab shape: a scope
// method invoked on a local variable holding an AR relation.
func TestExtractFile_LocalRelationScopeCall(t *testing.T) {
	src := `class FeatureFlagsFinder
  def execute(preload: true)
    items = feature_flags
    items = items.preload_relations if preload
    items.ordered
  end
end
`
	result := extractFileAST([]byte(src), "app/finders/feature_flags_finder.rb", true, true)
	meth := symbolsByName(result)["FeatureFlagsFinder#execute"]
	if !hasCall(meth, "preload_relations") {
		t.Errorf("scope call on a local relation var should record preload_relations; relations = %v", meth.Relations)
	}
}

// TestExtractFile_IvarUnderscoredCall checks that an underscored method call on an
// instance/class variable receiver (`@klass.bo_search_fields`) is recorded, while a
// single-word read on an ivar (`@user.name`) is not.
func TestExtractFile_IvarUnderscoredCall(t *testing.T) {
	src := `class BoSearchService
  def search
    @klass.bo_search_fields
    @@config.some_setting
    @user.name
  end
end
`
	result := extractFileAST([]byte(src), "app/services/bo_search_service.rb", true, true)
	meth := symbolsByName(result)["BoSearchService#search"]
	for _, want := range []string{"bo_search_fields", "some_setting"} {
		if !hasCall(meth, want) {
			t.Errorf("underscored call on ivar/cvar receiver should record %s; relations = %v", want, meth.Relations)
		}
	}
	if hasCall(meth, "name") {
		t.Errorf("single-word read @user.name must not be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_KlassReceiverDispatch checks that a method call on a klass-named
// receiver (the Ruby idiom for a Class var) is recorded as a class-method dispatch
// regardless of the method name, while cheap reads are not.
func TestExtractFile_KlassReceiverDispatch(t *testing.T) {
	src := `class Dispatcher
  def run
    registry.each do |klass|
      klass.inline
      klass.name
    end
    @klass.trigger_now
  end
end
`
	result := extractFileAST([]byte(src), "app/services/dispatcher.rb", true, true)
	meth := symbolsByName(result)["Dispatcher#run"]
	for _, want := range []string{"inline", "trigger_now"} {
		if !hasCall(meth, want) {
			t.Errorf("klass-receiver dispatch should record %s; relations = %v", want, meth.Relations)
		}
	}
	if hasCall(meth, "name") {
		t.Errorf("cheap klass.name must not be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_InterpolatedStringPrefix checks that an interpolated string used
// as a computed dispatch name records its prefix, while i18n-key and message strings
// (`.`/space endings) do not.
func TestExtractFile_InterpolatedStringPrefix(t *testing.T) {
	src := `class ComposeResults
  def handle(hit)
    model_to_present = "present_#{hit['_index'].singularize}"
    title = "reports.#{hit['type']}.title"
    msg = "Hello #{hit['name']}"
    SearchPresenter.send(model_to_present, hit)
  end
end
`
	result := extractFileAST([]byte(src), "app/services/es/compose_results.rb", true, true)
	got := fileRefPrefixes(result)
	has := func(p string) bool {
		for _, x := range got {
			if x == p {
				return true
			}
		}
		return false
	}
	if !has("present_") {
		t.Errorf("expected dynamic_send_prefixes to contain present_ (method dispatches); got %v", got)
	}
	if has("reports.") || has("Hello ") {
		t.Errorf("i18n/message string prefixes must be gated out; got %v", got)
	}
}

// TestExtractFile_StringPrefixGatedOnDispatcher checks that an interpolated-string
// prefix is NOT recorded when the enclosing scope invokes no dispatcher (so cache/key
// strings like `"fetch_#{id}"` don't hide genuine orphans), while an interpolated
// SYMBOL is still recorded unconditionally.
func TestExtractFile_StringPrefixGatedOnDispatcher(t *testing.T) {
	src := `class Keys
  def cache_key(id)
    "fetch_#{id}"
  end
  def dispatch_by_symbol(type)
    m = :"report_#{type}"
    public_send(m)
  end
end
`
	result := extractFileAST([]byte(src), "app/services/keys.rb", true, true)
	got := fileRefPrefixes(result)
	has := func(p string) bool {
		for _, x := range got {
			if x == p {
				return true
			}
		}
		return false
	}
	if has("fetch_") {
		t.Errorf("string prefix in a non-dispatching method must NOT be recorded; got %v", got)
	}
	if !has("report_") {
		t.Errorf("interpolated symbol prefix should be recorded unconditionally; got %v", got)
	}
}

// TestExtractFile_GvarUnderscoredCall covers the v27 $gvar receiver variant: an
// underscored (scope/class-method-like) call on a global-variable receiver is a
// method invocation and must be recorded, while a cheap single-word read is not.
func TestExtractFile_GvarUnderscoredCall(t *testing.T) {
	src := `class GlobalService
  def run
    $registry.bo_search_fields
    $config.name
  end
end
`
	result := extractFileAST([]byte(src), "app/services/global_service.rb", true, true)
	meth := symbolsByName(result)["GlobalService#run"]
	if !hasCall(meth, "bo_search_fields") {
		t.Errorf("underscored call on $gvar receiver should record bo_search_fields; relations = %v", meth.Relations)
	}
	if hasCall(meth, "name") {
		t.Errorf("cheap $config.name must not be recorded; relations = %v", meth.Relations)
	}
}

// TestExtractFile_ClazzKlazzReceiverDispatch covers the v28 clazz/klazz spellings
// (alongside klass): a method call on a Class-object-named receiver is class-method
// dispatch regardless of the method name.
func TestExtractFile_ClazzKlazzReceiverDispatch(t *testing.T) {
	src := `class Dispatcher
  def run
    registry.each do |clazz|
      clazz.inline
    end
    klazz.trigger_now
  end
end
`
	result := extractFileAST([]byte(src), "app/services/dispatcher2.rb", true, true)
	meth := symbolsByName(result)["Dispatcher#run"]
	for _, want := range []string{"inline", "trigger_now"} {
		if !hasCall(meth, want) {
			t.Errorf("clazz/klazz-receiver dispatch should record %s; relations = %v", want, meth.Relations)
		}
	}
}

// --- Detection: Gemfile-less repos and extensionless Ruby executables ---

// writeFile creates a file (and parent dirs) under root with the given content.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

func TestDetect_Gemfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Gemfile", "source 'https://rubygems.org'\n")
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect with Gemfile = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestDetect_LooseRubyFileNoGemfile(t *testing.T) {
	dir := t.TempDir()
	// No Gemfile; a plain .rb source nested under scripts/lib (depth 2).
	writeFile(t, dir, "scripts/lib/installer.rb", "class Installer\nend\n")
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect with loose .rb = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestDetect_ExtensionlessRubyShebang(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "scripts/new-intent", "#!/usr/bin/env ruby\nputs 'hi'\n")
	ok, err := New().Detect(dir)
	if err != nil || !ok {
		t.Fatalf("Detect with extensionless ruby executable = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestDetect_NoRubyOnlyBashShebang(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "scripts/deploy", "#!/bin/bash\necho hi\n")
	writeFile(t, dir, "README.md", "# docs\n")
	ok, err := New().Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if ok {
		t.Fatalf("Detect with only a bash executable = true, want false")
	}
}

// Detection has no depth bound. This test asserted the opposite until the bound was
// removed, and it was right to: the three-level scan it described was real, and a
// Ruby file at a/b/c/d was genuinely invisible. That is the bug, not the contract.
// A bounded scan is beatable by any repository one level deeper than the bound —
// dotnet/runtime hides 3,270 C/C++ sources from a three-level scan, flutterfire's
// Java sits at depth 10 — so the fix was to stop walking rather than to walk further.
func TestDetect_LooseRubyAtAnyDepth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a/b/c/d/deep.rb", "class Deep\nend\n")
	ok, err := New().Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if !ok {
		t.Fatalf("Detect with .rb at depth 4 = false, want true")
	}
}

// The other half of the same contract: vendored code is still not a signal, at any
// depth. Removing the bound must not turn "a repository that carries someone else's
// Ruby" into "a Ruby repository".
func TestDetect_VendoredRubyIsNotASignal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vendor/bundle/gems/foo/lib/foo.rb", "class Foo\nend\n")
	writeFile(t, dir, "node_modules/pkg/x.rb", "class X\nend\n")
	ok, err := New().Detect(dir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if ok {
		t.Fatalf("Detect with only vendored .rb = true, want false")
	}
}

func TestIsRubySourceFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "scripts/foo.rb", "class Foo\nend\n")
	writeFile(t, dir, "scripts/new-intent", "#!/usr/bin/env ruby\nputs 1\n")
	writeFile(t, dir, "scripts/deploy", "#!/bin/bash\necho hi\n")
	writeFile(t, dir, "bin/plastic.js", "console.log('shim')\n")

	cases := []struct {
		rel  string
		want bool
	}{
		{"scripts/foo.rb", true},     // extension match
		{"scripts/new-intent", true}, // extensionless ruby shebang
		{"scripts/deploy", false},    // extensionless bash shebang
		{"bin/plastic.js", false},    // non-ruby extension
		{"scripts/missing", false},   // extensionless, unreadable → false
	}
	for _, c := range cases {
		if got := isRubySourceFile(dir, c.rel); got != c.want {
			t.Errorf("isRubySourceFile(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestHasRubyShebang(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ruby_env", "#!/usr/bin/env ruby\n")
	writeFile(t, dir, "ruby_abs", "#!/usr/bin/ruby -w\n")
	writeFile(t, dir, "bash", "#!/bin/bash\n")
	writeFile(t, dir, "node", "#!/usr/bin/env node\n")
	writeFile(t, dir, "noshebang", "puts 'hi'\n")

	cases := []struct {
		name string
		want bool
	}{
		{"ruby_env", true},
		{"ruby_abs", true},
		{"bash", false},
		{"node", false},
		{"noshebang", false},
	}
	for _, c := range cases {
		if got := hasRubyShebang(filepath.Join(dir, c.name)); got != c.want {
			t.Errorf("hasRubyShebang(%q) = %v, want %v", c.name, got, c.want)
		}
	}
	if hasRubyShebang(filepath.Join(dir, "does-not-exist")) {
		t.Errorf("hasRubyShebang(missing) = true, want false")
	}
}

// A nested plural resource with `shallow: true` serves its MEMBER routes at the
// shallow path (parent segment + :parent_id dropped) while collection routes stay
// nested — mirroring Rails.
func TestRoutes_ShallowResource(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v3 do
    resources :posts do
      resources :replies, only: [:index, :create, :show, :update, :destroy], shallow: true
    end
  end
end
`
	names := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))

	// Collection routes stay nested.
	if names["/v3/posts/:post_id/replies"] == nil {
		t.Errorf("missing nested collection /v3/posts/:post_id/replies; got %v", keysMM(names))
	}
	// Member routes are shallow.
	if names["/v3/replies/:id"] == nil {
		t.Errorf("missing shallow member /v3/replies/:id; got %v", keysMM(names))
	}
	// The fully-nested member path must NOT be emitted.
	if names["/v3/posts/:post_id/replies/:id"] != nil {
		t.Errorf("shallow member must not be emitted at the nested path /v3/posts/:post_id/replies/:id")
	}
}

// A Rails optional route segment `foo(/:bar)` expands to both concrete paths.
func TestRoutes_OptionalSegment(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v2 do
    get 'email_subscriptions(/:key)', to: 'subs#update'
  end
end
`
	names := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	for _, want := range []string{"/v2/email_subscriptions", "/v2/email_subscriptions/:key"} {
		if names[want] == nil {
			t.Errorf("missing expanded optional route %q; got %v", want, keysMM(names))
		}
	}
}

// keysMM returns the sorted keys of a name->methods map for error messages.
func keysMM(m map[string]map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The Rails hash-rocket route form `get 'path' => 'ctrl#action'` (path as the
// string key) is extracted — including with an optional segment, as company uses.
func TestRoutes_HashRocketPath(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v2 do
    get 'email_subscriptions(/:key)' => 'email_subscriptions#index', as: :email_subscriptions_index
    put 'settings' => 'settings#update'
  end
end
`
	names := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))
	for _, want := range []string{"/v2/email_subscriptions", "/v2/email_subscriptions/:key", "/v2/settings"} {
		if names[want] == nil {
			t.Errorf("missing hash-rocket route %q; got %v", want, keysMM(names))
		}
	}
	if names["/v2/settings"] != nil && !names["/v2/settings"]["PUT"] {
		t.Errorf("/v2/settings method = %v, want PUT", names["/v2/settings"])
	}
}

func TestRoutes_BareVerbInResourcesNestsUnderMember(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :api do
    resources :steps, only: :update do
      get :status
      post :reset
      collection do
        get :get_custom_fields
      end
      post :callback, on: :collection
      get :audit, on: :member
    end
  end
end
`
	result := parseRouteFileAST([]byte(src), "config/routes.rb")
	names := make(map[string]string)
	for _, f := range result {
		if f.Kind == facts.KindRoute {
			names[f.Name] = f.Props["method"].(string)
		}
	}
	// `on: :member` addresses the resource itself and takes :id; only a bare
	// verb nests under the parent's :step_id. Verified against
	// ActionDispatch::Routing::RouteSet on Rails 7.2, which serves
	// GET /api/steps/:id/audit — this assertion previously read :step_id and
	// was asserting an endpoint Rails does not serve.
	for path, method := range map[string]string{
		"/api/steps/:step_id/status":   "GET",
		"/api/steps/:step_id/reset":    "POST",
		"/api/steps/:id/audit":         "GET",
		"/api/steps/get_custom_fields": "GET",
		"/api/steps/callback":          "POST",
	} {
		if names[path] != method {
			t.Errorf("route %s %s missing or wrong method (got %v)", method, path, names)
		}
	}
}

// TestRoutes_JsonapiResourcesServeNoFormRoutes pins the served set against a
// production Rails monolith's booted route table: JSONAPI::Resources is an API-only
// gem, so /new and /edit — which exist in Rails only to render HTML forms — are
// not served, and reusing the Rails expansion here would invent two routes per
// declaration.
func TestRoutes_JsonapiResourcesServeNoFormRoutes(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v1 do
    jsonapi_resources :answers
    jsonapi_resources :career_sites, only: [:index]
    jsonapi_resource :company
  end
end
`
	routes := routeMethods(parseRouteFileAST([]byte(src), "config/routes.rb"))

	for _, m := range []string{"GET", "POST"} {
		if !routes["/v1/answers"][m] {
			t.Errorf("/v1/answers: missing %s among %v", m, routes["/v1/answers"])
		}
	}
	for _, m := range []string{"GET", "PATCH", "PUT", "DELETE"} {
		if !routes["/v1/answers/:id"][m] {
			t.Errorf("/v1/answers/:id: missing %s among %v", m, routes["/v1/answers/:id"])
		}
	}
	for _, path := range []string{"/v1/answers/new", "/v1/answers/:id/edit", "/v1/company/new", "/v1/company/edit"} {
		if _, ok := routes[path]; ok {
			t.Errorf("jsonapi declarations must not serve %s", path)
		}
	}

	// The gem's default route formatter dasherizes the segment, so the served
	// path is /v1/career-sites and /v1/career_sites is served by nothing.
	if !routes["/v1/career-sites"]["GET"] {
		t.Errorf("career_sites: want GET /v1/career-sites, got %v", routes["/v1/career-sites"])
	}
	if _, ok := routes["/v1/career_sites"]; ok {
		t.Errorf("underscored segment /v1/career_sites is not served")
	}

	// A singular jsonapi_resource acts on the bare path: no index, no :id.
	for _, m := range []string{"GET", "POST", "PATCH", "PUT", "DELETE"} {
		if !routes["/v1/company"][m] {
			t.Errorf("/v1/company: missing %s among %v", m, routes["/v1/company"])
		}
	}
	if _, ok := routes["/v1/company/:id"]; ok {
		t.Errorf("singular jsonapi_resource must not have an :id member path")
	}
}

// TestRoutes_JsonapiUnknownRouteFormatIsCounted covers the repository that
// installs its own route formatter: the URL segment is then decided by Ruby this
// extractor cannot run, so the declaration is counted rather than expanded.
func TestRoutes_JsonapiUnknownRouteFormatIsCounted(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v1 do
    jsonapi_resources :career_sites
  end
end
`
	ff, res := parseRouteFile([]byte(src), "config/routes.rb", "", "", jsonapiContext{format: jsonapiRouteUnknown})
	unhandled := res.unresolved
	if len(ff) != 0 {
		t.Errorf("an unreadable route formatter must produce no routes, got %d", len(ff))
	}
	if unhandled["jsonapi_resources"] != 1 {
		t.Errorf("the refusal must be counted, got %v", unhandled)
	}
}

// TestRoutes_ImmutableResourceServesNoWrites covers the one thing config/routes.rb
// cannot answer on its own: a resource class declaring `immutable` serves no
// create, update or destroy, however unfiltered its declaration looks.
func TestRoutes_ImmutableResourceServesNoWrites(t *testing.T) {
	src := `Rails.application.routes.draw do
  namespace :v1 do
    jsonapi_resources :stage_types
    jsonapi_resources :answers
  end
end
`
	resolver := &jsonapiResolver{
		resourceFiles: map[string]string{"v1/stage_type": "app/resources/v1/stage_type_resource.rb"},
		relCache: map[string]*jsonapiResourceClass{
			"app/resources/v1/stage_type_resource.rb": {immutable: true},
		},
		declaredTypes: map[string]string{},
		modelFiles:    map[string]string{},
		modelCache:    map[string]map[string]string{},
	}
	ff, _ := parseRouteFile([]byte(src), "config/routes.rb", "", "", jsonapiContext{format: jsonapiRouteDasherized, resolver: resolver})
	routes := routeMethods(ff)

	for _, m := range []string{"POST", "PATCH", "PUT", "DELETE"} {
		if routes["/v1/stage-types"][m] || routes["/v1/stage-types/:id"][m] {
			t.Errorf("an immutable resource must not serve %s", m)
		}
	}
	for _, path := range []string{"/v1/stage-types", "/v1/stage-types/:id"} {
		if !routes[path]["GET"] {
			t.Errorf("%s: an immutable resource still serves its reads", path)
		}
	}
	// The marker applies to the resource that declares it and to nothing else.
	if !routes["/v1/answers"]["POST"] {
		t.Errorf("answers is not immutable and must keep its writes: %v", routes["/v1/answers"])
	}
}

// TestJsonapiResolverModulePathDisambiguates pins the reason hop 1 reads the
// route scope's module path: seven of the monolith's resource classes share a
// basename across api/v1, api/partner/v1 and api/channel/v1, and a filename-only
// lookup answers for the wrong one or refuses them all.
func TestJsonapiResolverModulePathDisambiguates(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/resources/api/v1/company_resource.rb", "class CompanyResource\n  has_one :manager\nend\n")
	write("app/resources/api/partner/v1/company_resource.rb", "class CompanyResource\n  immutable\nend\n")

	files := []string{
		"app/resources/api/v1/company_resource.rb",
		"app/resources/api/partner/v1/company_resource.rb",
	}
	r := newJsonapiResolver(dir, files)

	v1 := r.resourceClass("api/v1", "companies")
	if v1 == nil || len(v1.relationships) != 1 || v1.immutable {
		t.Errorf("api/v1 must resolve to its own class, got %+v", v1)
	}
	partner := r.resourceClass("api/partner/v1", "companies")
	if partner == nil || !partner.immutable {
		t.Errorf("api/partner/v1 must resolve to its own class, got %+v", partner)
	}
	if other := r.resourceClass("api/channel/v1", "companies"); other != nil {
		t.Errorf("a namespace with no such class resolves to nothing, got %+v", other)
	}
}

// TestJsonapiHandlerReadsTheModelAssociation covers hop 3 and hop 4 together: a
// relationship named for its role rather than its class resolves only through
// the model, and the controller is the name the target's own declaration used —
// never a plural this extractor computed.
func TestJsonapiHandlerReadsTheModelAssociation(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/resources/api/v1/company_resource.rb", "class CompanyResource\n  has_one :manager\n  has_one :owner\n  has_many :steps, through: :flows\nend\n")
	write("app/models/company.rb", "class Company\n  belongs_to :manager, class_name: \"User\"\n  has_many :steps, through: :flows, source: :verdicts\nend\n")
	r := newJsonapiResolver(dir, []string{
		"app/resources/api/v1/company_resource.rb",
		"app/models/company.rb",
	})
	r.declare("api/v1", "users")
	r.declare("api/v1", "owners")
	r.declare("api/v1", "verdicts")
	owner := r.resourceClass("api/v1", "companies")

	cases := []struct{ rel, want string }{
		{"manager", "api/v1/users"},  // the model renames it
		{"owner", "api/v1/owners"},   // nothing renames it, so the name stands
		{"steps", "api/v1/verdicts"}, // through:, whose source: names the target
	}
	for _, c := range cases {
		var found jsonapiRelationship
		for _, rel := range owner.relationships {
			if rel.name == c.rel {
				found = rel
			}
		}
		got := r.handlerFor("api/v1", found, owner, "company")
		if got != c.want {
			t.Errorf("%s: handler = %q, want %q", c.rel, got, c.want)
		}
	}

	// A target nothing declared is a hop that ends in nothing, and the route is
	// emitted without a handler rather than with a guessed one.
	bare := newJsonapiResolver(dir, []string{"app/resources/api/v1/company_resource.rb", "app/models/company.rb"})
	if got := bare.handlerFor("api/v1", jsonapiRelationship{name: "manager"}, owner, "company"); got != "" {
		t.Errorf("an undeclared target must resolve to nothing, got %q", got)
	}
}

// TestParseMount_HashRocketCallKey covers the mount shape the monolith writes and
// upstream's reader drops: the hash-rocket form whose KEY builds a Rack app inline.
//
// `mount Flipper::UI.app(Flipper) => "/admin/flipper"` names its app through a call,
// not a bare constant. parseMount unwraps exactly that call on the `at:` side already;
// on the key side it required a constant, so the declaration yielded no constant, and
// with no constant the whole mount — path included — was dropped. /admin/flipper
// disappeared from the monolith's route table.
func TestParseMount_HashRocketCallKey(t *testing.T) {
	src := []byte(`Rails.application.routes.draw do
  mount Flipper::UI.app(Flipper) => "/admin/flipper", :constraints => SuperadminConstraint.new
  mount Sidekiq::Web => "/admin/sidekiq"
  mount Coverband::Reporters::Web.new, at: "/coverage"
end
`)
	ff, _ := parseRouteFile(src, "config/routes.rb", "", "", jsonapiContext{format: jsonapiRouteDasherized})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["method"] == "MOUNT" {
			got[f.Name], _ = f.Props["mounts"].(string)
		}
	}
	for path, want := range map[string]string{
		"/admin/flipper/": "Flipper::UI",
		"/admin/sidekiq/": "Sidekiq::Web",
		"/coverage/":      "Coverband::Reporters::Web",
	} {
		if got[path] != want {
			t.Errorf("mount at %s = %q, want %q (got %v)", path, got[path], want, got)
		}
	}
}
