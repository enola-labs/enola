package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// writeFiles writes body at each path under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A cluster's output does not depend on its members' order, and a refresh of one
// member writes what a full generate of the same trees writes, over a cluster that
// gives every binder something to do: two Rails apps sharing class names (one binds a
// callback the other does not), two Go services using go-redis, and the fixtures of
// every other language. A binder that keys a repository's names across repositories,
// or adds again what it added before when a restored graph is bound a second time,
// fails here. Every other fixture is in it twice, so every explainer meets names two
// repositories share.
func TestCluster_RefreshIsExactAcrossFixtures(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a cluster of every fixture four times")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	src := filepath.Join("testdata", "repos")
	base := t.TempDir()
	if keep := os.Getenv("ENOLA_FIXTURE_CLUSTER_OUT"); keep != "" {
		base = keep // to read the outputs after a failure
	}

	var members []string
	add := func(name, from string) string {
		t.Helper()
		dst := filepath.Join(base, name)
		if err := os.CopyFS(dst, os.DirFS(from)); err != nil {
			t.Fatal(err)
		}
		members = append(members, dst)
		return dst
	}
	shop := add("shop", filepath.Join(src, "ruby_sample"))
	admin := add("admin", filepath.Join(src, "ruby_sample"))
	writeFiles(t, admin, map[string]string{"app/models/customer_record.rb": "class CustomerRecord < ApplicationRecord\n" +
		"  include Trackable\n  before_save :track!\n\n  def display_name\n    name.upcase\n  end\nend\n",
		// The class reopened in another file, as a Rails app's extensions do: one
		// repository holding two facts of one name, which binders tell apart only by
		// the order the store holds them in.
		"app/models/ee/customer_record.rb": "class CustomerRecord < ApplicationRecord\n" +
			"  after_commit :audit\n\n  def audit\n    display_name\n  end\nend\n"})
	redisRepo := func(name string) {
		dir := filepath.Join(base, name)
		writeFiles(t, dir, map[string]string{
			"go.mod": "module example.com/" + name + "\n\ngo 1.21\n\nrequire github.com/redis/go-redis/v9 v9.0.0\n",
			"cache.go": "package main\n\nimport (\n\t\"context\"\n\n\t\"github.com/redis/go-redis/v9\"\n)\n\n" +
				"func lookup(ctx context.Context, c *redis.Client, k string) string {\n\tv, _ := c.Get(ctx, k).Result()\n\treturn v\n}\n\nfunc main() {}\n",
		})
		members = append(members, dir)
	}
	redisRepo("cache-a")
	redisRepo("cache-b")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		// A multi-repository fixture's repositories are members of their own, so the
		// cross-repository binders (HTTP, gRPC, messaging, GraphQL) have pairs to link.
		if e.IsDir() && (strings.Contains(e.Name(), "multirepo") || e.Name() == "ts_custom_client_cluster") {
			subs, err := os.ReadDir(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, sub := range subs {
				if sub.IsDir() {
					add(e.Name()+"-"+sub.Name(), filepath.Join(src, e.Name(), sub.Name()))
					add(e.Name()+"-"+sub.Name()+"-copy", filepath.Join(src, e.Name(), sub.Name()))
				}
			}
			continue
		}
		if e.IsDir() && e.Name() != "ruby_sample" {
			// Twice: every name of every language recurs in another repository.
			add(e.Name(), filepath.Join(src, e.Name()))
			add(e.Name()+"-copy", filepath.Join(src, e.Name()))
		}
	}

	generate := func(order []string, dir string) {
		t.Helper()
		cfg := config.Default()
		cfg.Repos = order
		cfg.Output.ClusterDir = dir
		eng, err := bootstrap.NewEngineFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.GenerateCluster(context.Background(), order, false); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	refresh := func(dir, member string) {
		t.Helper()
		cfg := config.Default()
		cfg.Repos = members
		cfg.Output.ClusterDir = dir
		eng, err := bootstrap.NewEngineFromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		eng.SetRestoreDocTexts(true)
		if err := bootstrap.RestoreSnapshot(eng, members); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.GenerateSnapshot(context.Background(), member, true); err != nil {
			t.Fatal(err)
		}
		if err := eng.WriteGraph(); err != nil {
			t.Fatal(err)
		}
	}
	same := func(a, b, what string) {
		t.Helper()
		for _, name := range []string{"facts.jsonl", "insights.json", "flows.json", "docs.jsonl"} {
			x, _ := os.ReadFile(filepath.Join(a, name))
			y, _ := os.ReadFile(filepath.Join(b, name))
			if len(x) == 0 || !bytes.Equal(x, y) {
				t.Errorf("%s: %s differs%s", what, name, lineDiff(x, y))
			}
		}
	}

	forward := filepath.Join(base, "out-forward")
	generate(members, forward)
	reversed := make([]string, len(members))
	for i, m := range members {
		reversed[len(members)-1-i] = m
	}
	reverse := filepath.Join(base, "out-reverse")
	generate(reversed, reverse)
	same(forward, reverse, "member order")

	// Refresh shop unchanged: the restored graph bound again is the same graph.
	refresh(forward, shop)
	same(forward, reverse, "refresh without a change")

	// Refresh cache-a after an edit, then a full generate of the same trees.
	writeFiles(t, filepath.Join(base, "cache-a"), map[string]string{"extra.go": "package main\n\nfunc unused() {}\n"})
	refresh(forward, filepath.Join(base, "cache-a"))
	full := filepath.Join(base, "out-full")
	generate(members, full)
	same(forward, full, "refresh against a full generate")
}

// lineDiff names a few lines only one side has, to say what differs.
func lineDiff(a, b []byte) string {
	count := func(data []byte) map[string]int {
		m := map[string]int{}
		var arr []json.RawMessage
		if json.Unmarshal(data, &arr) == nil {
			for _, x := range arr {
				var buf bytes.Buffer
				_ = json.Compact(&buf, x)
				m[buf.String()]++
			}
			return m
		}
		for _, l := range bytes.Split(data, []byte("\n")) {
			m[string(l)]++
		}
		return m
	}
	ca, cb := count(a), count(b)
	var out []string
	for l, n := range ca {
		if cb[l] != n && len(out) < 12 {
			out = append(out, "  only left:  "+trim(l))
		}
	}
	for l, n := range cb {
		if ca[l] != n && len(out) < 24 {
			out = append(out, "  only right: "+trim(l))
		}
	}
	if len(out) == 0 {
		var xa, xb []map[string]any
		if json.Unmarshal(a, &xa) == nil && json.Unmarshal(b, &xb) == nil {
			for i := range xa {
				if i < len(xb) {
					ja, _ := json.Marshal(xa[i])
					jb, _ := json.Marshal(xb[i])
					if !bytes.Equal(ja, jb) {
						return fmt.Sprintf(" (same lines, other order; first at %d: %s %v | %s %v)", i, xa[i]["source"], xa[i]["title"], xb[i]["source"], xb[i]["title"])
					}
				}
			}
		}
		return " (same lines, other order)"
	}
	return "\n" + strings.Join(out, "\n")
}

func trim(s string) string {
	if len(s) > 700 {
		return s[:700]
	}
	return s
}
