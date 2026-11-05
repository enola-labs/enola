package rubyextractor

import (
	"context"
	"testing"
)

// rubyTypedRepo extracts files and returns the facts, failing on a temporary prop
// that left the extractor.
func rubyTypedRepo(t *testing.T, files map[string]string) map[string][]string {
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
	for _, f := range ff {
		if f.PropAny(propTypedCalls) != nil || f.PropAny(propIvarTypes) != nil {
			t.Errorf("%s keeps a typing prop that must not leave the extractor", f.Name)
		}
	}
	return methodCalls(t, files)
}

func TestRubyCallOnAVariableHoldingANewInstanceIsAnEdge(t *testing.T) {
	got := rubyTypedRepo(t, map[string]string{
		"app/engines/engine.rb": `
module Engines
  class Base
    def run; end
  end

  class Engine < Base
    def step(n); end
  end
end
`,
		"app/services/sync.rb": `
class Sync
  def initialize
    @engine = Engines::Engine.new
    @other ||= Engines::Engine.new
  end

  def local
    e = Engines::Engine.new
    e.run
    e.step(1)
  end

  def copied
    e = Engines::Engine.new
    f = e
    f.run
  end

  def from_ivar
    @engine.run
    @other.step(2)
  end

  def nil_first
    e = nil
    e = Engines::Engine.new
    e.run
  end

  def two_types(flag)
    e = flag ? 1 : Engines::Engine.new
    e = Engines::Engine.new if flag
    e.run
  end

  def shadowed(items)
    e = Engines::Engine.new
    items.each { |e| e.run }
  end

  def undeclared
    e = Engines::Engine.new
    e.stop(1)
  end
end

class Child < Sync
  def inherited_ivar
    @engine.run
  end
end

class Mixed
  def a
    @x = Engines::Engine.new
  end

  def b
    @x = 1
  end

  def c
    @x.run
  end
end
`,
	})
	run := "Engines::Base#run"
	for _, m := range []string{"Sync#local", "Sync#copied", "Sync#from_ivar", "Sync#nil_first", "Child#inherited_ivar"} {
		wantCalls(t, got, m, run)
	}
	wantCalls(t, got, "Sync#local", "Engines::Engine#step")
	wantNoCall(t, got, "Sync#local", "e.step")
	wantCalls(t, got, "Sync#from_ivar", "Engines::Engine#step")
	// Not typed: assigned a value of no known type, a block parameter, two types.
	wantNoCall(t, got, "Sync#two_types", run)
	wantNoCall(t, got, "Sync#shadowed", run)
	wantNoCall(t, got, "Mixed#c", run)
	// A method the class does not declare keeps the call as written.
	wantCalls(t, got, "Sync#undeclared", "e.stop")
}

func TestRubyLocalFromAModelFinderIsTypedAsTheModel(t *testing.T) {
	got := rubyTypedRepo(t, map[string]string{
		"app/models/application_record.rb": `
class ApplicationRecord < ActiveRecord::Base
  self.abstract_class = true
end
`,
		"app/models/user.rb": `
class User < ApplicationRecord
  def suspend!(reason); end
  def activate; end
end

class Admin < User
end
`,
		"app/services/registry.rb": `
class Registry
  def self.find(id); end
  def lookup; end
end
`,
		"app/services/moderation.rb": `
class Moderation
  def by_id(id)
    user = User.find(id)
    user.suspend!(:spam)
  end

  def by_email(email)
    @user = User.find_by!(email: email)
    @user.activate
  end

  def sti(id)
    admin = Admin.first
    admin.activate
  end

  def not_a_model(id)
    r = Registry.find(id)
    r.lookup
  end

  def not_a_finder
    u = User.where(active: true)
    u.activate
  end
end
`,
	})
	wantCalls(t, got, "Moderation#by_id", "User#suspend!")
	wantNoCall(t, got, "Moderation#by_id", "user.suspend!")
	wantCalls(t, got, "Moderation#by_email", "User#activate")
	wantCalls(t, got, "Moderation#sti", "User#activate")
	// A class method named like a finder on a class that is no model types nothing,
	// and neither does a finder that returns a relation.
	wantNoCall(t, got, "Moderation#not_a_model", "Registry#lookup")
	wantNoCall(t, got, "Moderation#not_a_finder", "User#activate")
}

func TestRubyCallOnAFactorysResultReachesTheReturnedClass(t *testing.T) {
	got := rubyTypedRepo(t, map[string]string{
		"lib/engine.rb": `
class Engine
  def run; end
  def session
    Session.new
  end
end

class Session
  def commit; end
end
`,
		"lib/engine_factory.rb": `
module EngineFactory
  def self.make_engine
    Engine.new
  end

  def self.maybe(flag)
    return nil unless flag
    Engine.new
  end

  def self.either(flag)
    return 1 if flag
    Engine.new
  end
end

class Pool
  def self.build(size)
    new
  end

  def checkout; end
end

class Builder
  def with(x)
    self
  end

  def build
    Engine.new
  end
end
`,
		"lib/api.rb": `
class Api
  attr_reader :held

  def initialize
    @held = Engine.new
  end

  def imported
    e = EngineFactory.make_engine
    e.run
  end

  def nullable(flag)
    e = EngineFactory.maybe(flag)
    e.run
  end

  def mixed(flag)
    e = EngineFactory.either(flag)
    e.run
  end

  def chained
    EngineFactory.make_engine.session.commit
  end

  def own
    e = local_engine
    e.run
  end

  def memo
    engine.run
  end

  def reader
    held.run
  end

  def built
    Pool.build(2).checkout
  end

  def builder
    Builder.new.with(1).with(2).build.run
  end

  private

  def local_engine
    Engine.new
  end

  def engine
    @engine ||= Engine.new
  end
end
`,
	})
	for _, m := range []string{"Api#imported", "Api#nullable", "Api#own", "Api#memo", "Api#reader", "Api#builder"} {
		wantCalls(t, got, m, "Engine#run")
	}
	wantCalls(t, got, "Api#chained", "Session#commit")
	wantCalls(t, got, "Api#built", "Pool#checkout")
	// A method that returns an Integer on one path declares no type.
	wantNoCall(t, got, "Api#mixed", "Engine#run")
}
