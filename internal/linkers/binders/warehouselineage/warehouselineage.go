// Package warehouselineage links the tables a warehouse loads to where their rows
// come from.
//
// A dbt project reads what it does not build through a source: `source('raw',
// 'orders')`. The extractor records the source as a storage node, but the rows
// behind it are written somewhere else: another dbt project's model, a Kafka
// topic or schema a connector lands in the warehouse, or a service's database
// copied by change data capture. Each of those lives in its own repository and
// names the table its own way, so no extractor can join them, and a walk from a
// service's table, an event or a model stopped at the warehouse's edge.
//
// The binder runs after cross-repo linking, over the whole store, and adds a
// depends_on edge from each source to what loads it, trying in order:
//
//  1. A table another dbt project builds, by name (`identifier` if set). Several
//     candidates are narrowed to the ones under a directory named like the
//     source's schema; anything still ambiguous is left unlinked.
//  2. An Avro record or a Kafka topic whose name is the source's, separators
//     aside: a sink writes `contracts.shop.orders.order_placed` as the table
//     `contracts.shop_orders_order_placed` or `shop_orders_order_placed`, and the
//     namespace's first segment is often dropped (`cdc.shop.orders` lands as
//     `shop_orders`).
//  3. A change-data-capture table name, `<database>_<table>` or
//     `<database>__<table>`, whose database names a loaded repository that
//     declares the table.
//
// A change-data-capture record or topic, `cdc.<database>.<table>`, is also linked
// to the table its database's repository declares, so a walk from that table
// reaches the change feed and what reads it.
//
// The bias of every cross-repo rule holds: a missing edge beats a wrong one. A
// name that resolves to more than one candidate links nothing.
package warehouselineage

import (
	"context"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder links warehouse sources to their producers.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

// Name identifies the binder.
func (b *Binder) Name() string { return "warehouse-lineage" }

// Stage runs it after cross-repo linking: every rule joins two repositories.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// PropLineage records which rule linked a source: "model", "named" or "cdc".
const PropLineage = "lineage"

const (
	engineSource = "dbt_source"
	engineModel  = "dbt_model"
	cdcPrefix    = "cdc."
)

// cdcEnvSuffixes name the environment a database copy serves; the copy is the
// same owner's.
var cdcEnvSuffixes = []string{"_sandbox", "_staging", "_production", "_prod", "_dev", "_test"}

type node struct{ repo, name string }

type index struct {
	repos     map[string]bool
	nameRepos map[string]map[string]bool
	// dbt-built tables by table name.
	built map[string][]facts.Fact
	// Avro records and topics by normalized name; stripped drops the first
	// segment of names with three or more.
	records, topics, strippedRecords, strippedTopics map[string]map[string]bool
	// A repository's tables: table name -> the storage nodes holding it.
	tables map[string]map[string][]string
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	ix := build(all)
	adds := map[node][]string{}
	how := map[node]string{}
	add := func(from node, target, rule string) {
		for _, t := range adds[from] {
			if t == target {
				return
			}
		}
		adds[from] = append(adds[from], target)
		if how[from] == "" {
			how[from] = rule
		}
	}
	for _, f := range all {
		switch {
		case f.Kind == facts.KindStorage && f.PropString("engine") == engineSource:
			if target, rule := ix.sourceProducer(f); target != "" {
				add(node{f.Repo, f.Name}, target, rule)
			}
		case isRecord(f):
			if target := ix.cdcTable(strings.TrimPrefix(f.Name, "avro.")); target != "" {
				add(node{f.Repo, f.Name}, target, "cdc")
			}
		case isTopic(f):
			if target := ix.cdcTable(f.Name); target != "" {
				add(node{f.Repo, f.Name}, target, "cdc")
			}
		}
	}
	if len(adds) == 0 {
		return nil
	}
	store.UpdateWhere(func(f *facts.Fact) {
		k := node{f.Repo, f.Name}
		targets := adds[k]
		if len(targets) == 0 || (f.Kind != facts.KindStorage && f.Kind != facts.KindSymbol) {
			return
		}
		for _, t := range targets {
			if !has(f, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
			}
		}
		if f.Kind == facts.KindStorage && f.PropString("engine") == engineSource && f.PropString(PropLineage) == "" {
			f.SetProp(PropLineage, how[k])
		}
	})
	return nil
}

func isRecord(f facts.Fact) bool {
	return f.Kind == facts.KindSymbol && f.PropString("language") == "avro" && f.PropString("avro_kind") == "record"
}

func isTopic(f facts.Fact) bool {
	return f.Kind == facts.KindStorage && f.PropString("storage_kind") == facts.StorageKindTopic && !facts.IsTestPath(f.File)
}

func build(all []facts.Fact) *index {
	ix := &index{
		repos: map[string]bool{}, nameRepos: map[string]map[string]bool{},
		built:   map[string][]facts.Fact{},
		records: map[string]map[string]bool{}, topics: map[string]map[string]bool{},
		strippedRecords: map[string]map[string]bool{}, strippedTopics: map[string]map[string]bool{},
		tables: map[string]map[string][]string{},
	}
	put := func(m map[string]map[string]bool, key, target string) {
		if m[key] == nil {
			m[key] = map[string]bool{}
		}
		m[key][target] = true
	}
	for _, f := range all {
		if f.Repo != "" {
			ix.repos[f.Repo] = true
		}
		// Only modules and symbols get a node per repository (as the graph splits
		// them); a storage name is one node however many repositories hold it.
		if f.Name != "" && f.Repo != "" && (f.Kind == facts.KindModule || f.Kind == facts.KindSymbol) {
			if ix.nameRepos[f.Name] == nil {
				ix.nameRepos[f.Name] = map[string]bool{}
			}
			ix.nameRepos[f.Name][f.Repo] = true
		}
		switch {
		case isRecord(f) && strings.Contains(strings.TrimPrefix(f.Name, "avro."), "."):
			// A record outside any namespace, or a one-segment topic, is too
			// generic a name to say what a table holds.
			full := strings.TrimPrefix(f.Name, "avro.")
			put(ix.records, normalize(full), f.Name)
			if s, ok := stripFirst(full); ok {
				put(ix.strippedRecords, normalize(s), f.Name)
			}
		case isTopic(f) && strings.Contains(f.Name, "."):
			put(ix.topics, normalize(f.Name), f.Name)
			if s, ok := stripFirst(f.Name); ok {
				put(ix.strippedTopics, normalize(s), f.Name)
			}
		case f.Kind == facts.KindStorage && f.PropString("engine") == engineModel:
			ix.built[strings.ToLower(f.Name)] = append(ix.built[strings.ToLower(f.Name)], f)
		case f.Kind == facts.KindStorage && f.Repo != "" && !strings.HasPrefix(f.PropString("engine"), "dbt_"):
			table := ""
			switch f.PropString("storage_kind") {
			case "model":
				table = f.PropString("table")
			case "table":
				table = f.Name
			}
			if table == "" {
				continue
			}
			if ix.tables[f.Repo] == nil {
				ix.tables[f.Repo] = map[string][]string{}
			}
			key := strings.ToLower(table)
			if !contains(ix.tables[f.Repo][key], f.Name) {
				ix.tables[f.Repo][key] = append(ix.tables[f.Repo][key], f.Name)
			}
		}
	}
	return ix
}

// sourceProducer returns what loads a dbt source, and the rule that found it.
func (ix *index) sourceProducer(f facts.Fact) (string, string) {
	src, table, ok := strings.Cut(f.Name, ".")
	if !ok {
		return "", ""
	}
	if id := f.PropString("identifier"); id != "" {
		table = id
	}
	if t := ix.builtTable(f, table); t != "" {
		return t, "model"
	}
	for _, lookup := range []struct {
		m   map[string]map[string]bool
		key string
	}{
		{ix.records, normalize(src + "." + table)}, {ix.records, normalize(table)}, {ix.strippedRecords, normalize(table)},
		{ix.topics, normalize(src + "." + table)}, {ix.topics, normalize(table)}, {ix.strippedTopics, normalize(table)},
	} {
		if one, ok := single(lookup.m[lookup.key]); ok {
			return one, "named"
		}
	}
	if t := ix.dbTable(table); t != "" {
		return t, "cdc"
	}
	return "", ""
}

// builtTable resolves a source to the table another dbt project builds.
func (ix *index) builtTable(source facts.Fact, table string) string {
	var cands []facts.Fact
	for _, c := range ix.built[strings.ToLower(table)] {
		if c.Repo == source.Repo && c.PropString("dbt_project") == source.PropString("dbt_project") {
			continue // a project reads its own models through ref, not source
		}
		cands = append(cands, c)
	}
	if len(cands) > 1 {
		if schema := source.PropString("schema"); schema != "" {
			var under []facts.Fact
			for _, c := range cands {
				if strings.Contains("/"+c.File, "/"+schema+"/") {
					under = append(under, c)
				}
			}
			cands = under
		}
	}
	if len(cands) != 1 {
		return ""
	}
	return ix.target(cands[0].Repo, cands[0].Name)
}

// cdcTable resolves a change-data-capture name, cdc.<database>.<table>.
func (ix *index) cdcTable(name string) string {
	rest, ok := strings.CutPrefix(name, cdcPrefix)
	if !ok {
		return ""
	}
	db, table, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(table, ".") {
		return ""
	}
	return ix.tableIn(db, table)
}

// dbTable resolves a landed change-data-capture table, <database>__<table> or
// <database>_<table>, trying every split point: the database is the part that
// names a loaded repository declaring the rest as a table. Several resolving
// splits link nothing.
func (ix *index) dbTable(name string) string {
	if i := strings.Index(name, "__"); i > 0 {
		if t := ix.tableIn(name[:i], name[i+2:]); t != "" {
			return t
		}
	}
	found := ""
	for i := strings.IndexByte(name, '_'); i > 0; {
		if t := ix.tableIn(name[:i], name[i+1:]); t != "" {
			if found != "" && found != t {
				return ""
			}
			found = t
		}
		next := strings.IndexByte(name[i+1:], '_')
		if next < 0 {
			break
		}
		i += next + 1
	}
	return found
}

// tableIn resolves a table of the repository a database name stands for: the
// name as written or with underscores as hyphens, with or without an
// environment suffix.
func (ix *index) tableIn(db, table string) string {
	if db == "" || table == "" {
		return ""
	}
	for _, repo := range dbRepos(db) {
		if !ix.repos[repo] {
			continue
		}
		names := ix.tables[repo][strings.ToLower(table)]
		if len(names) != 1 {
			continue
		}
		return ix.target(repo, names[0])
	}
	return ""
}

func dbRepos(db string) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range []string{db, trimEnvSuffix(db)} {
		for _, n := range []string{name, strings.ReplaceAll(name, "_", "-")} {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func trimEnvSuffix(db string) string {
	for _, suf := range cdcEnvSuffixes {
		if trimmed, ok := strings.CutSuffix(db, suf); ok && trimmed != "" {
			return trimmed
		}
	}
	return db
}

// target names a node of repo as an edge target: the bare name, or the repo's
// own node when several repositories declare the name.
func (ix *index) target(repo, name string) string {
	if len(ix.nameRepos[name]) > 1 {
		return facts.ScopedName(repo, name)
	}
	return name
}

func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '-':
			return '_'
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// stripFirst drops the first segment of a dotted name of three or more segments.
func stripFirst(name string) (string, bool) {
	if strings.Count(name, ".") < 2 {
		return "", false
	}
	_, rest, _ := strings.Cut(name, ".")
	return rest, true
}

func single(set map[string]bool) (string, bool) {
	if len(set) != 1 {
		return "", false
	}
	for k := range set {
		return k, true
	}
	return "", false
}

func has(f *facts.Fact, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == facts.RelDependsOn && r.Target == target {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
