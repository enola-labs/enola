package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests in this file run the built binary the way a consumer that loads the
// snapshot into its own store does (docs/INTEGRATING.md): `enola --generate
// <repo>` as a subprocess, then nothing but the three contract artifacts.
//
// internal/facts/wireformat_test.go pins the field NAMES by marshaling structs,
// and the goldens pin extractor output per language. Neither runs the binary,
// and neither checks what a reader relies on ACROSS the artifacts: that a
// target_id names a fact that is in the file, that the receipt counts the file
// it sits beside, that an unchanged tree keeps its snapshot_id. Those are the
// assertions here. They are invariants rather than goldens: an extractor change
// that adds, removes or renames facts must not have to touch this file.

const consumerFixtures = "../../internal/engine/testdata/repos"

type consumerSnapshot struct {
	dir      string
	facts    []map[string]any
	ids      map[string]map[string]any // fact id -> one record carrying it
	receipt  map[string]any
	insights []map[string]any
}

// generateAsConsumer runs `enola --generate <target>` with the environment pair
// INTEGRATING.md step 2 prescribes, and returns stderr. Stdout is discarded: it
// is not part of the contract.
func generateAsConsumer(t *testing.T, bin, home, workDir, target string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--generate", target)
	cmd.Dir = workDir
	cmd.Env = append(sandboxEnv(home), "ENOLA_NO_PROMPTS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("enola --generate %s: %v\n%s", target, err, stderr.String())
	}
	return stderr.String()
}

func copyFixture(t *testing.T, name, dst string) string {
	t.Helper()
	src := filepath.Join(consumerFixtures, filepath.FromSlash(name))
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copying fixture %s: %v", name, err)
	}
	return dst
}

// decodeJSON decodes with UseNumber, so an integer field that started arriving
// as 1.0 or "1" is distinguishable from 1.
func decodeJSON(t *testing.T, data []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("decoding %.80q: %v", data, err)
	}
}

func readConsumerSnapshot(t *testing.T, repo string) consumerSnapshot {
	t.Helper()
	snap := consumerSnapshot{dir: filepath.Join(repo, ".enola"), ids: map[string]map[string]any{}}
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(snap.dir, name))
		if err != nil {
			t.Fatalf("contract artifact missing after a successful run: %v", err)
		}
		return data
	}

	for i, line := range bytes.Split(bytes.TrimRight(read("facts.jsonl"), "\n"), []byte("\n")) {
		var fact map[string]any
		decodeJSON(t, line, &fact)
		if fact == nil {
			t.Fatalf("facts.jsonl line %d is not a JSON object: %.80q", i+1, line)
		}
		snap.facts = append(snap.facts, fact)
		if id, ok := fact["id"].(string); ok {
			snap.ids[id] = fact
		}
	}

	decodeJSON(t, read("receipt.json"), &snap.receipt)

	// A reader iterates insights.json; `null` for "no findings" would be a type error.
	raw := bytes.TrimSpace(read("insights.json"))
	if !bytes.HasPrefix(raw, []byte("[")) {
		t.Fatalf("insights.json must be a JSON array, got %.40q", raw)
	}
	decodeJSON(t, raw, &snap.insights)
	return snap
}

func isFactID(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) != 32 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// wholeNumber reports whether v was written as a JSON integer, and its value.
func wholeNumber(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// assertFactsContract checks the per-record shape and the references between
// records. It returns how many resolved references it followed, so a caller can
// tell a passing run from a run that had nothing to check.
func assertFactsContract(t *testing.T, snap consumerSnapshot) (targetIDs int) {
	t.Helper()
	identity := map[string][4]string{}
	for i, fact := range snap.facts {
		where := "facts.jsonl line " + strconv.Itoa(i+1)
		for _, key := range []string{"kind", "name", "repo"} {
			if !nonEmptyString(fact[key]) {
				t.Errorf("%s: %q must be a non-empty string, got %#v", where, key, fact[key])
			}
		}
		if !isFactID(fact["id"]) {
			t.Errorf("%s: id must be 32 lowercase hex characters, got %#v", where, fact["id"])
			continue
		}
		for _, key := range []string{"line", "end_line"} {
			if v, present := fact[key]; present {
				if _, ok := wholeNumber(v); !ok {
					t.Errorf("%s: %q must be an integer, got %#v", where, key, v)
				}
			}
		}

		// The id is derived from (repo, kind, name, file). Records may share
		// one, and a reader collapses them onto one node, which is only sound
		// while they agree on all four.
		file, _ := fact["file"].(string)
		key := [4]string{fact["repo"].(string), fact["kind"].(string), fact["name"].(string), file}
		id := fact["id"].(string)
		if prev, seen := identity[id]; seen && prev != key {
			t.Errorf("%s: id %s is shared by %v and %v", where, id, prev, key)
		}
		identity[id] = key

		relations, _ := fact["relations"].([]any)
		for _, entry := range relations {
			relation, ok := entry.(map[string]any)
			if !ok || !nonEmptyString(relation["kind"]) || !nonEmptyString(relation["target"]) {
				t.Errorf("%s: relation must be {kind, target, target_id?}, got %#v", where, entry)
				continue
			}
			targetID, present := relation["target_id"]
			if !present {
				continue
			}
			if !isFactID(targetID) || snap.ids[targetID.(string)] == nil {
				t.Errorf("%s: relation %v -> %v carries target_id %#v, which names no fact in the snapshot",
					where, relation["kind"], relation["target"], targetID)
				continue
			}
			targetIDs++
		}
	}
	return targetIDs
}

func assertReceiptContract(t *testing.T, snap consumerSnapshot) {
	t.Helper()
	receipt := snap.receipt
	if v, ok := wholeNumber(receipt["format_version"]); !ok || v != 1 {
		t.Errorf("receipt format_version must be the integer 1, got %#v", receipt["format_version"])
	}
	for _, key := range []string{"snapshot_id", "enola_version", "extractor_version", "generated_at"} {
		if !nonEmptyString(receipt[key]) {
			t.Errorf("receipt %q must be a non-empty string, got %#v", key, receipt[key])
		}
	}
	if v, ok := wholeNumber(receipt["fact_count"]); !ok || int(v) != len(snap.facts) {
		t.Errorf("receipt fact_count = %#v, facts.jsonl has %d records", receipt["fact_count"], len(snap.facts))
	}
	if v, ok := wholeNumber(receipt["insight_count"]); !ok || int(v) != len(snap.insights) {
		t.Errorf("receipt insight_count = %#v, insights.json has %d entries", receipt["insight_count"], len(snap.insights))
	}

	hashes, _ := receipt["output_hashes"].(map[string]any)
	for _, name := range []string{"facts.jsonl", "insights.json"} {
		data, err := os.ReadFile(filepath.Join(snap.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if want := "sha256:" + hex.EncodeToString(sum[:]); hashes[name] != want {
			t.Errorf("receipt output_hashes[%q] = %#v, the file on disk hashes to %s", name, hashes[name], want)
		}
	}

	quality, ok := receipt["quality"].(map[string]any)
	if !ok {
		t.Fatalf("receipt quality must be an object, got %#v", receipt["quality"])
	}
	counts := map[string]int64{}
	for _, key := range []string{"files_seen", "files_parsed", "parse_errors"} {
		v, ok := wholeNumber(quality[key])
		if !ok {
			t.Errorf("receipt quality.%s must be an integer, got %#v", key, quality[key])
		}
		counts[key] = v
	}
	if counts["files_parsed"] > counts["files_seen"] {
		t.Errorf("receipt quality: files_parsed %d exceeds files_seen %d", counts["files_parsed"], counts["files_seen"])
	}
}

// assertInsightsContract returns how many evidence fact_ids it resolved.
func assertInsightsContract(t *testing.T, snap consumerSnapshot) (factIDs int) {
	t.Helper()
	for i, insight := range snap.insights {
		if !nonEmptyString(insight["title"]) || !nonEmptyString(insight["source"]) {
			t.Errorf("insight %d: title and source must be non-empty strings, got %#v / %#v",
				i, insight["title"], insight["source"])
		}
		evidence, _ := insight["evidence"].([]any)
		for _, entry := range evidence {
			item, ok := entry.(map[string]any)
			if !ok {
				t.Errorf("insight %d: evidence entry must be an object, got %#v", i, entry)
				continue
			}
			factID, present := item["fact_id"]
			if !present {
				continue
			}
			if !isFactID(factID) || snap.ids[factID.(string)] == nil {
				t.Errorf("insight %q cites fact_id %#v, which names no fact in the snapshot", insight["title"], factID)
				continue
			}
			factIDs++
		}
	}
	return factIDs
}

func propsOf(fact map[string]any) map[string]any {
	props, _ := fact["props"].(map[string]any)
	return props
}

func TestConsumerContract_SingleRepository(t *testing.T) {
	bin := enolaBinary(t)
	home := t.TempDir()
	repo := copyFixture(t, "python_sample", filepath.Join(t.TempDir(), "shop"))

	// The repository is both the argument and the working directory, so a
	// repository-local mcp-arch.yaml is honored and no other one can be.
	stderr := generateAsConsumer(t, bin, home, repo, repo)

	// A consumer records the resolved-configuration line with its ingestion log.
	var configLine string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "enola:") {
			configLine = line
			break
		}
	}
	if !strings.Contains(configLine, "using built-in defaults") {
		t.Errorf("stderr must report the resolved configuration on an `enola:` line, got %q", configLine)
	}

	snap := readConsumerSnapshot(t, repo)
	assertReceiptContract(t, snap)
	if n := assertFactsContract(t, snap); n == 0 {
		t.Error("no relation in the snapshot carries a target_id; the reference checks ran on nothing")
	}
	if n := assertInsightsContract(t, snap); n == 0 {
		t.Error("no insight evidence carries a fact_id; the reference checks ran on nothing")
	}

	// Every fact of a single-repository snapshot carries the same label, and
	// without a git remote that label is the checkout directory name.
	symbolKinds := map[string]bool{}
	for _, fact := range snap.facts {
		if fact["repo"] != "shop" {
			t.Fatalf("fact %v is labelled repo %#v, want the directory name \"shop\"", fact["name"], fact["repo"])
		}
		if fact["kind"] == "symbol" {
			kind, _ := propsOf(fact)["symbol_kind"].(string)
			symbolKinds[kind] = true
		}
	}
	// A reader derives method ownership from these three values.
	for _, kind := range []string{"class", "method", "function"} {
		if !symbolKinds[kind] {
			t.Errorf("no symbol with symbol_kind %q in a Python fixture that declares one (saw %v)", kind, symbolKinds)
		}
	}

	// snapshot_id is what a consumer keys "nothing changed, skip the load" on.
	generateAsConsumer(t, bin, home, repo, repo)
	if again := readConsumerSnapshot(t, repo); again.receipt["snapshot_id"] != snap.receipt["snapshot_id"] {
		t.Errorf("snapshot_id changed across two runs over an unchanged tree: %v then %v",
			snap.receipt["snapshot_id"], again.receipt["snapshot_id"])
	}
}

func TestConsumerContract_NoFindingsIsAnEmptyArray(t *testing.T) {
	bin := enolaBinary(t)
	repo := filepath.Join(t.TempDir(), "tiny")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	generateAsConsumer(t, bin, t.TempDir(), repo, repo)

	snap := readConsumerSnapshot(t, repo)
	if len(snap.insights) != 0 {
		t.Skipf("the fixture now yields %d insight(s); it no longer exercises the empty case", len(snap.insights))
	}
	assertReceiptContract(t, snap)
	assertFactsContract(t, snap)
}

// A cluster config in the form a consumer generates: absolute, JSON-quoted
// member paths, in a directory that is none of the members. One member is a
// git clone whose directory name differs from its remote's repository name.
func TestConsumerContract_Cluster(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	bin := enolaBinary(t)
	home := t.TempDir()
	checkouts := t.TempDir()
	server := copyFixture(t, "ts_express_multirepo/server", filepath.Join(checkouts, "srv-checkout"))
	consumer := copyFixture(t, "ts_express_multirepo/consumer", filepath.Join(checkouts, "consumer"))

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", server, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Env = sandboxEnv(home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	git("remote", "add", "origin", "https://example.com/acme/server.git")

	configDir := t.TempDir()
	config := filepath.Join(configDir, "cluster.yaml")
	lines := []string{"repos:"}
	for _, member := range []string{server, consumer} {
		quoted, err := json.Marshal(member)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "  - "+string(quoted))
	}
	if err := os.WriteFile(config, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	generateAsConsumer(t, bin, home, configDir, config)

	// The whole linked graph lands in every member's .enola, so a consumer may
	// read any one of them.
	snap := readConsumerSnapshot(t, server)
	other := readConsumerSnapshot(t, consumer)
	serverFacts, err := os.ReadFile(filepath.Join(snap.dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	consumerFacts, err := os.ReadFile(filepath.Join(other.dir, "facts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serverFacts, consumerFacts) {
		t.Error("the cluster's members hold different facts.jsonl files")
	}
	if snap.receipt["snapshot_id"] != other.receipt["snapshot_id"] {
		t.Errorf("the cluster's members disagree on snapshot_id: %v vs %v",
			snap.receipt["snapshot_id"], other.receipt["snapshot_id"])
	}
	assertReceiptContract(t, snap)
	assertReceiptContract(t, other)
	assertFactsContract(t, snap)
	assertInsightsContract(t, snap)

	// A member is labelled by its remote's repository name, else by its
	// directory name. A consumer maps labels back to members with exactly that
	// rule, reading the remote from the member's own receipt.
	labels := map[string]bool{}
	for _, fact := range snap.facts {
		labels[fact["repo"].(string)] = true
	}
	if len(labels) != 2 || !labels["server"] || !labels["consumer"] {
		t.Errorf("repo labels = %v, want exactly server (from the remote) and consumer (from the directory)", labels)
	}
	gitInfo, _ := snap.receipt["git"].(map[string]any)
	if gitInfo["remote"] != "example.com/acme/server" {
		t.Errorf("the git member's receipt git.remote = %#v, want the normalized remote", gitInfo["remote"])
	}

	// A client call carries its calling symbol and the server routes it
	// reaches, each by name and by resolved id.
	var linked int
	for _, fact := range snap.facts {
		props := propsOf(fact)
		if fact["kind"] != "route" || props["role"] != "client" {
			continue
		}
		if callerID, present := props["caller_id"]; present {
			caller := snap.ids[asString(callerID)]
			if caller == nil || caller["kind"] != "symbol" || caller["name"] != props["caller"] {
				t.Errorf("client route %v: caller_id %#v does not name the symbol %#v", fact["name"], callerID, props["caller"])
			}
		}
		matches, _ := props["matched_routes"].([]any)
		for _, entry := range matches {
			match, _ := entry.(map[string]any)
			route := snap.ids[asString(match["id"])]
			if route == nil || route["kind"] != "route" || route["name"] != match["name"] || route["repo"] != match["repo"] {
				t.Errorf("client route %v: matched_routes entry %#v does not name a route fact", fact["name"], entry)
				continue
			}
			if fact["repo"] == "consumer" && route["repo"] == "server" && props["caller_id"] != nil {
				linked++
			}
		}
	}
	if linked == 0 {
		t.Error("no client route in consumer resolved to a route in server with a caller_id; the cluster was not linked")
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
