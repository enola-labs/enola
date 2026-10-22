package rubydex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// The calls whose receiver is the lexical enclosing class are the ones the
// Prism reference script types and the Rubydex facts leave to it. The engine
// resolves their receiver, so they are written under the script's names with
// the receiver spelled as Ruby resolves it: an explicit Board.find inside
// Board is the singleton, never Board#find. A new on a constant is an
// instantiation, carrying the message sent to it in the same breath.
func TestCollect_WritesTheEnclosingClassCallsUnderThePrismNames(t *testing.T) {
	lib := requireLibrary(t)
	repo := writeFiles(t, map[string]string{
		"Gemfile": "source \"https://rubygems.org\"\n",
		"app/models/board.rb": "module Billing\n" +
			"  class Board\n" +
			"    has_many :cards\n" +
			"    def self.force(x)\n" +
			"      force(x)\n" +
			"    end\n" +
			"    def total\n" +
			"      sum + self.sum\n" +
			"      Board.find(1)\n" +
			"      Card.new(1).save\n" +
			"      Card.new\n" +
			"      foo.bar\n" +
			"    end\n" +
			"    def sum; end\n" +
			"    def self.find(id); end\n" +
			"  end\n" +
			"end\n",
		"app/models/card.rb": "module Billing\n  class Card\n    def initialize(*); end\n    def save; end\n  end\nend\n",
	})
	result := Collect(context.Background(), lib, repo, nil)
	if result.Refusal != "" {
		t.Fatalf("refused: %s", result.Refusal)
	}
	type shape struct {
		line            int
		relation, level string
		oneShot         any
	}
	want := map[string][]shape{
		"prism-call: Billing::Board -> Billing::Board.has_many":   {{3, "calls", "lexical-self", nil}},
		"prism-call: Billing::Board#total -> Billing::Board#sum":  {{8, "calls", "lexical-self", nil}, {8, "calls", "lexical-self", nil}},
		"prism-call: Billing::Board#total -> Billing::Board.find": {{9, "calls", "constant-receiver", nil}},
		"prism-call: Billing::Board#total -> Billing::Board#foo":  {{12, "calls", "lexical-self", nil}},
		"prism-new: Billing::Board#total -> Billing::Card":        {{10, "instantiates", "constant-receiver", "save"}, {11, "instantiates", "constant-receiver", nil}},
	}
	got := map[string][]shape{}
	for _, f := range result.Prism {
		if f.Kind != facts.KindDependency || len(f.Relations) != 1 {
			t.Fatalf("%s: every fact is a dependency with one relation: %+v", f.Name, f)
		}
		if f.Props["engine"] != "rubydex" {
			t.Errorf("%s: carries engine %v, want rubydex", f.Name, f.Props["engine"])
		}
		if f.File != "app/models/board.rb" {
			t.Errorf("%s: file %s", f.Name, f.File)
		}
		got[f.Name] = append(got[f.Name], shape{f.Line, f.Relations[0].Kind, f.Props["resolution_level"].(string), f.Props["one_shot_call"]})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prism facts differ\n got: %v\nwant: %v", got, want)
	}
	for _, f := range result.Facts {
		if f.Name == "rubydex-call: Billing::Board#total -> Billing::Board#sum" {
			t.Errorf("the Rubydex facts are unchanged: an enclosing-class call is still left out of them")
		}
	}
	causes := map[string]int{}
	for _, cause := range result.PrismCensus.SkipCauses {
		causes[cause.Cause] = cause.Count
	}
	if causes["receiver resolves to no constant"] != 3 {
		t.Errorf("prism census = %+v, want the three calls on an untyped receiver (+, save, bar) counted, never written", result.PrismCensus)
	}
	if result.PrismCensus.FilesSeen != 2 || result.PrismCensus.DeclarationsParsed != len(result.Prism) {
		t.Errorf("prism census = %+v, want two files and one declaration per fact", result.PrismCensus)
	}
}

// The reference fixture's one same-class call is the one the Rubydex facts
// counted as skipped; it is now the Prism side's fact, and the Rubydex facts
// and census stay as they were.
func TestCollect_TheSkippedSameClassCallIsThePrismSidesFact(t *testing.T) {
	lib := requireLibrary(t)
	result := Collect(context.Background(), lib, writeFixture(t), nil)
	var names []string
	for _, f := range result.Prism {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"prism-call: Invoice#total -> Invoice#sum"}) {
		t.Fatalf("prism facts = %v, want the one same-class call", names)
	}
}

// Ruby evaluates a default parameter on the instance, and the engine types an
// implicit call there as the class's singleton, since it reads parameters
// before it enters the method. The lexical context and the engine disagree, so
// nothing is written and the disagreement is counted.
func TestCollect_ACallInADefaultParameterIsNotWrittenOnTheClass(t *testing.T) {
	lib := requireLibrary(t)
	repo := writeFiles(t, map[string]string{
		"Gemfile": "source \"https://rubygems.org\"\n",
		"lib/base.rb": "class Base\n" +
			"  def self.settings; end\n" +
			"  def settings; end\n" +
			"  def route!(base = settings)\n" +
			"    settings\n" +
			"  end\n" +
			"end\n",
	})
	result := Collect(context.Background(), lib, repo, nil)
	var names []string
	for _, f := range result.Prism {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"prism-call: Base#route! -> Base#settings"}) {
		t.Fatalf("prism facts = %v, want only the body's call, on the instance", names)
	}
	causes := map[string]int{}
	for _, cause := range result.PrismCensus.SkipCauses {
		causes[cause.Cause] = cause.Count
	}
	if causes["implicit receiver typed as the class inside an instance method"] != 1 {
		t.Errorf("prism census = %+v, want the default parameter's call counted", result.PrismCensus)
	}
}

// A range operator ends in a dot but sends nothing: the call after `..` or
// `...` is a bare call, so it is lexical-self like any other.
func TestCollect_ACallAfterARangeOperatorHasNoReceiver(t *testing.T) {
	lib := requireLibrary(t)
	repo := writeFiles(t, map[string]string{
		"Gemfile": "source \"https://rubygems.org\"\n",
		"app/lib/source.rb": "class Source\n" +
			"  def span\n" +
			"    (low..high)\n" +
			"    where(at: ..limit)\n" +
			"    where(at: [nil, ...limit])\n" +
			"    self.limit\n" +
			"  end\n" +
			"  def where(*); end\n" +
			"  def low; end\n" +
			"  def high; end\n" +
			"  def limit; end\n" +
			"end\n",
	})
	result := Collect(context.Background(), lib, repo, nil)
	levels := map[string]string{}
	for _, f := range result.Prism {
		levels[f.Name+":"+strconv.Itoa(f.Line)] = f.Props["resolution_level"].(string)
	}
	want := map[string]string{
		"prism-call: Source#span -> Source#low:3":   "lexical-self",
		"prism-call: Source#span -> Source#high:3":  "lexical-self",
		"prism-call: Source#span -> Source#where:4": "lexical-self",
		"prism-call: Source#span -> Source#limit:4": "lexical-self",
		"prism-call: Source#span -> Source#where:5": "lexical-self",
		"prism-call: Source#span -> Source#limit:5": "lexical-self",
		"prism-call: Source#span -> Source#limit:6": "lexical-self",
	}
	if !reflect.DeepEqual(levels, want) {
		t.Errorf("levels differ\n got: %v\nwant: %v", levels, want)
	}
}
