package facts

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestFactID_IsStableAndDistinct pins the two properties a consumer keys nodes
// on: the same fact always hashes the same, and facts that differ in any of the
// four identity fields hash differently.
func TestFactID_IsStableAndDistinct(t *testing.T) {
	base := FactID("repo", KindSymbol, "app.Run", "app/main.go")
	if len(base) != 2*idBytes {
		t.Fatalf("id is %d characters, want %d", len(base), 2*idBytes)
	}
	if strings.Trim(base, "0123456789abcdef") != "" {
		t.Errorf("id %q is not lowercase hex", base)
	}
	if again := FactID("repo", KindSymbol, "app.Run", "app/main.go"); again != base {
		t.Errorf("same inputs produced %q then %q; the id must be a pure function", base, again)
	}

	// One field different at a time. The file case is P1-1's whole point: two
	// functions sharing a name in different files must not collapse.
	for _, tc := range []struct{ what, repo, kind, name, file string }{
		{"repo", "other", KindSymbol, "app.Run", "app/main.go"},
		{"kind", "repo", KindModule, "app.Run", "app/main.go"},
		{"name", "repo", KindSymbol, "app.Walk", "app/main.go"},
		{"file", "repo", KindSymbol, "app.Run", "app/other.go"},
	} {
		if got := FactID(tc.repo, tc.kind, tc.name, tc.file); got == base {
			t.Errorf("a different %s produced the same id %q", tc.what, got)
		}
	}
}

// TestFactID_FieldsCannotRunTogether is why the fields are NUL-separated.
// Concatenated, ("ab", "") and ("a", "b") would be one hash, and two unrelated
// facts would merge into a single node in every consumer's graph — the exact
// defect ids exist to remove, arriving through the hash instead of the name.
func TestFactID_FieldsCannotRunTogether(t *testing.T) {
	if FactID("a", "b", "c", "d") == FactID("ab", "", "c", "d") {
		t.Error(`FactID("a","b",…) collides with FactID("ab","",…): fields are running together`)
	}
	if FactID("", "", "", "ab") == FactID("", "", "a", "b") {
		t.Error("a name/file boundary shift produced the same id")
	}
}

// TestFactID_MatchesIdentityMethod keeps the two spellings from drifting: the
// serialization path calls the scratch form, everything else calls the method.
func TestFactID_MatchesIdentityMethod(t *testing.T) {
	f := Fact{Kind: KindSymbol, Name: "app.Run", File: "app/main.go", Repo: "repo"}
	if got, want := f.Identity(), FactID(f.Repo, f.Kind, f.Name, f.File); got != want {
		t.Errorf("Identity() = %q, FactID(...) = %q", got, want)
	}
	id, scratch := factIDInto(nil, f.Repo, f.Kind, f.Name, f.File)
	if id != f.Identity() {
		t.Errorf("factIDInto = %q, Identity() = %q", id, f.Identity())
	}
	// The scratch buffer is reused across millions of calls; a stale tail must
	// not reach the hash.
	long, scratch := factIDInto(scratch, "repo", KindSymbol, strings.Repeat("x", 500), "f.go")
	short, _ := factIDInto(scratch, "repo", KindSymbol, "x", "f.go")
	if short != FactID("repo", KindSymbol, "x", "f.go") {
		t.Error("reusing a scratch buffer changed the id: the previous call's bytes leaked in")
	}
	_ = long
}

// TestTargetFactFor_ResolvesOnlyWhenUnambiguous covers each way a target name is
// answered: one match, several matches that are the same fact identity, several
// that are not, and no match at all.
func TestTargetFactFor_ResolvesOnlyWhenUnambiguous(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Unique", File: "a.go", Repo: "r"},
		// Same identity twice — the same thing recorded at two lines. Merging
		// them is correct, so this still resolves.
		Fact{Kind: KindSymbol, Name: "Twice", File: "b.go", Line: 1, Repo: "r"},
		Fact{Kind: KindSymbol, Name: "Twice", File: "b.go", Line: 9, Repo: "r"},
		// Same name in two files — genuinely two facts, so no id is honest.
		Fact{Kind: KindSymbol, Name: "Split", File: "c.go", Repo: "r"},
		Fact{Kind: KindSymbol, Name: "Split", File: "d.go", Repo: "r"},
	)
	ff := s.FactsRef()

	for _, tc := range []struct {
		name    string
		target  string
		resolve bool
	}{
		{"one fact", "Unique", true},
		{"one identity over two facts", "Twice", true},
		{"two identities", "Split", false},
		{"no fact at all", "fmt.Sprintf", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.targetFactFor(tc.target, "r")
			if tc.resolve && got < 0 {
				t.Fatalf("target %q did not resolve, want an id", tc.target)
			}
			if !tc.resolve && got >= 0 {
				t.Fatalf("target %q resolved to %q, want no id", tc.target, ff[got].Identity())
			}
		})
	}
}

// TestTargetFactFor_PrefersTheReferencingRepo — in a multi-repo snapshot a name
// occurring in two repositories almost always means the local one, and resolving
// it to the other repository's fact would draw a cross-repo edge nobody measured.
func TestTargetFactFor_PrefersTheReferencingRepo(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "Shared", File: "a.go", Repo: "left"},
		Fact{Kind: KindSymbol, Name: "Shared", File: "b.go", Repo: "right"},
		Fact{Kind: KindSymbol, Name: "OnlyRight", File: "b.go", Repo: "right"},
	)
	ff := s.FactsRef()

	got := s.targetFactFor("Shared", "left")
	if got < 0 || ff[got].Repo != "left" {
		t.Errorf("a reference from 'left' resolved to %v, want the fact in 'left'", got)
	}

	// With nothing local to prefer, the reference points outward and the whole
	// snapshot is considered.
	got = s.targetFactFor("OnlyRight", "left")
	if got < 0 || ff[got].Repo != "right" {
		t.Errorf("a reference with no local candidate resolved to %v, want the fact in 'right'", got)
	}
}

// TestWriteJSONL_EmitsIDs is the end-to-end shape check: every line carries an
// id, resolvable targets carry a target_id naming a fact that exists, and
// unresolvable ones carry none rather than an empty string.
func TestWriteJSONL_EmitsIDs(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "app", File: "app", Repo: "r"},
		Fact{Kind: KindSymbol, Name: "app.Run", File: "app/main.go", Repo: "r", Relations: []Relation{
			{Kind: RelDeclares, Target: "app"},
			{Kind: RelCalls, Target: "fmt.Println"}, // external: no fact
		}},
	)

	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		id, ok := m["id"].(string)
		if !ok || id == "" {
			t.Fatalf("line has no id: %s", l)
		}
		ids[id] = true
		lines = append(lines, m)

		// id must be LAST. WriteJSONL sorts whole lines, so an id anywhere
		// earlier would re-order the file and rewrite every stored history
		// revision as a side effect.
		if !strings.HasSuffix(l, `"id":"`+id+`"}`) {
			t.Errorf("id is not the last field, so the line sort key changed: %s", l)
		}
	}

	for _, m := range lines {
		rels, _ := m["relations"].([]any)
		for _, r := range rels {
			rel := r.(map[string]any)
			target := rel["target"].(string)
			tid, has := rel["target_id"]
			switch target {
			case "app":
				if !has {
					t.Error("a target naming a fact in the snapshot got no target_id")
				} else if !ids[tid.(string)] {
					t.Errorf("target_id %q names no fact in this snapshot", tid)
				}
			case "fmt.Println":
				if has {
					t.Errorf("an external target got target_id %v; it must be omitted", tid)
				}
			}
		}
	}
}

// TestWriteJSONL_IDsSurviveShuffling — ids must be a property of the facts, not
// of the order they were added in, or two machines extracting the same tree
// would disagree on every node key.
func TestWriteJSONL_IDsSurviveShuffling(t *testing.T) {
	base := awkwardStore().All()
	serialize := func(ff []Fact) string {
		s := NewStore()
		s.Add(ff...)
		var buf bytes.Buffer
		if err := s.WriteJSONL(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	want := serialize(base)
	reversed := make([]Fact, len(base))
	for i := range base {
		reversed[i] = base[len(base)-1-i]
	}
	if got := serialize(reversed); got != want {
		t.Error("reversing the insertion order changed the serialized ids")
	}
}

// TestMarshalInsights_ResolvesEvidence covers each outcome an evidence citation
// can have, including the two that must NOT be treated as failures.
func TestMarshalInsights_ResolvesEvidence(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "app", File: "app", Repo: "r"},
		Fact{Kind: KindSymbol, Name: "app.Run", File: "app/main.go", Repo: "r"},
		// Same name in two files: no honest single answer.
		Fact{Kind: KindSymbol, Name: "Split", File: "a.go", Repo: "r"},
		Fact{Kind: KindSymbol, Name: "Split", File: "b.go", Repo: "r"},
	)
	want := s.FactsRef()[1].Identity()

	out, err := s.MarshalInsights([]Insight{{
		Title: "t", Description: "d", Confidence: 1,
		Evidence: []Evidence{
			{Symbol: "app.Run", Detail: "cited by symbol"},
			{Fact: "app", Detail: "cited by fact"},
			{Symbol: "NoResultFound", Detail: "third-party: nothing to point at"},
			{Symbol: "Split", Detail: "ambiguous"},
			{File: "app/main.go", Detail: "file only, cites no fact"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	ev := got[0]["evidence"].([]any)
	ids := make([]string, len(ev))
	for i, e := range ev {
		id, _ := e.(map[string]any)["fact_id"].(string)
		ids[i] = id
	}
	if ids[0] != want {
		t.Errorf("symbol citation got fact_id %q, want %q", ids[0], want)
	}
	if ids[1] == "" {
		t.Error("a citation by fact name got no fact_id")
	}
	for i, why := range map[int]string{2: "third-party name", 3: "ambiguous name", 4: "no citation at all"} {
		if ids[i] != "" {
			t.Errorf("evidence %d (%s) got fact_id %q, want none", i, why, ids[i])
		}
	}
}

// TestMarshalInsights_LeavesTheDocumentAlone — adding ids must change nothing
// else about insights.json, including the shapes that marshal as null. Callers
// promised those before this existed.
func TestMarshalInsights_LeavesTheDocumentAlone(t *testing.T) {
	s := NewStore()
	s.Add(Fact{Kind: KindModule, Name: "app", File: "app", Repo: "r"})

	for _, tc := range []struct {
		name string
		in   []Insight
		want string
	}{
		{"nil slice", nil, "null"},
		{"empty slice", []Insight{}, "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.MarshalInsights(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("marshalled %q, want %q", out, tc.want)
			}
		})
	}

	// Nil evidence stays null rather than becoming []: the same document as before.
	out, err := s.MarshalInsights([]Insight{{Title: "t", Description: "d", Confidence: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"evidence": null`) {
		t.Errorf("nil evidence did not stay null:\n%s", out)
	}
}

// A matched_routes entry names its target by identity, and the writer adds that
// identity's id: the same id the target fact itself is written with. It does so on a
// copy, and recomputes an id already present rather than trusting it.
func TestWriteJSONL_AddsIDsToMatchedRoutes(t *testing.T) {
	entry := map[string]any{"repo": "gateway", "name": "/v1/x", "file": "gateway/x.ts", "method": "POST", "id": "stale"}
	s := NewStore()
	s.Add(
		Fact{Kind: KindRoute, Name: "/v1/x", File: "gateway/x.ts", Repo: "gateway",
			Props: map[string]any{"role": "server", "method": "POST"}},
		Fact{Kind: KindRoute, Name: "/v1/x", File: "sdk/c.ts", Repo: "sdk",
			Props: map[string]any{"role": "client", "method": "POST", PropMatchedRoutes: []any{entry}}},
	)

	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	var serverID, refID string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m struct {
			ID    string         `json:"id"`
			Repo  string         `json:"repo"`
			Props map[string]any `json:"props"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		if m.Repo == "gateway" {
			serverID = m.ID
			continue
		}
		refs, _ := m.Props[PropMatchedRoutes].([]any)
		if len(refs) != 1 {
			t.Fatalf("client line lost its matched_routes: %s", l)
		}
		refID, _ = refs[0].(map[string]any)["id"].(string)
	}
	if serverID == "" || refID != serverID {
		t.Fatalf("matched_routes id = %q, want the server route's id %q", refID, serverID)
	}
	if entry["id"] != "stale" {
		t.Fatalf("the store's own entry was written: id = %v", entry["id"])
	}
}

// caller names a symbol in the call site's own file, so its id is derived from the
// route's repo and file, and matches the symbol fact's own id.
func TestWriteJSONL_AddsCallerID(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindSymbol, Name: "src/api.OrdersClient.list", File: "web/src/api/orders.ts", Repo: "web"},
		Fact{Kind: KindRoute, Name: "/api/orders", File: "web/src/api/orders.ts", Repo: "web",
			Props: map[string]any{"role": "client", "method": "GET", PropCaller: "src/api.OrdersClient.list", PropCallerID: "stale"}},
	)
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	var symID, callerID string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m struct {
			ID    string         `json:"id"`
			Kind  string         `json:"kind"`
			Props map[string]any `json:"props"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		if m.Kind == KindSymbol {
			symID = m.ID
		} else {
			callerID, _ = m.Props[PropCallerID].(string)
		}
	}
	if symID == "" || callerID != symID {
		t.Fatalf("caller_id = %q, want the symbol's id %q", callerID, symID)
	}
}

// The ids the writer adds (caller_id, matched_routes[].id) are wire-only, like
// target_id: a fact read back from facts.jsonl, by ReadJSONL or ScanJSONL, equals the
// fact that was written, so a diff against a snapshot on disk sees no change.
func TestReadBack_DropsWireOnlyIDs(t *testing.T) {
	route := Fact{Kind: KindRoute, Name: "/api/orders", File: "web/src/api.ts", Repo: "web",
		Props: map[string]any{
			"role": "client", "method": "GET", PropCaller: "src/api.list",
			PropMatchedRoutes: []any{map[string]any{"repo": "api", "name": "/api/orders", "file": "api/h.go", "method": "GET", "confidence": "verified"}},
		}}
	s := NewStore()
	s.Add(route)
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), PropCallerID) {
		t.Fatalf("precondition: the writer adds caller_id: %s", buf.String())
	}

	back := NewStore()
	if err := back.ReadJSONL(strings.NewReader(buf.String())); err != nil {
		t.Fatal(err)
	}
	var scanned []Fact
	if err := ScanJSONL(strings.NewReader(buf.String()), func(f Fact) error { scanned = append(scanned, f); return nil }); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(route.Props)
	for name, got := range map[string]Fact{"ReadJSONL": back.All()[0], "ScanJSONL": scanned[0]} {
		if b, _ := json.Marshal(got.Props); string(b) != string(want) {
			t.Errorf("%s props = %s, want %s", name, b, want)
		}
	}
}
