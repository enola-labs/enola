package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExtractStorage_CreateTable(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/tables.go": `package db

const CreateUsersTable = ` + "`" + `
CREATE TABLE IF NOT EXISTS users (
    id INT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(255) NOT NULL UNIQUE,
    email VARCHAR(255) NOT NULL UNIQUE
);` + "`" + `

const CreateOrdersTable = ` + "`" + `
CREATE TABLE orders (
    id INT AUTO_INCREMENT PRIMARY KEY,
    user_id INT NOT NULL
);` + "`" + `
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	foundUsers := false
	foundOrders := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["storage_kind"] == "table" && s.Props["operation"] == "CREATE" {
			foundUsers = true
		}
		if s.Name == "orders" && s.Props["storage_kind"] == "table" && s.Props["operation"] == "CREATE" {
			foundOrders = true
		}
	}
	if !foundUsers {
		t.Error("expected storage fact for CREATE TABLE users")
	}
	if !foundOrders {
		t.Error("expected storage fact for CREATE TABLE orders")
	}
}

func TestExtractStorage_SelectQuery(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/user.go": `package repo

import "database/sql"

func GetUsers(db *sql.DB) {
	query := "SELECT id, username FROM users WHERE active = 1"
	_ = query
}
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "SELECT" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for SELECT FROM users")
	}
}

func TestExtractStorage_InsertUpdateDelete(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/orders.go": `package repo

const insertOrder = "INSERT INTO orders (user_id, total) VALUES (?, ?)"
const updateOrder = "UPDATE orders SET total = ? WHERE id = ?"
const deleteOrder = "DELETE FROM orders WHERE id = ?"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	ops := make(map[string]bool)
	for _, s := range storage {
		if s.Name == "orders" {
			ops[s.Props["operation"].(string)] = true
		}
	}
	if !ops["INSERT"] {
		t.Error("expected storage fact for INSERT INTO orders")
	}
	if !ops["UPDATE"] {
		t.Error("expected storage fact for UPDATE orders")
	}
	if !ops["DELETE"] {
		t.Error("expected storage fact for DELETE FROM orders")
	}
}

func TestExtractStorage_RawStringLiteral(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/query.go": "package repo\n\nconst q = `SELECT * FROM bookings WHERE user_id = ?`\n",
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "bookings" && s.Props["operation"] == "SELECT" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for SELECT FROM bookings in raw string")
	}
}

func TestExtractStorage_Dedup(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/user.go": `package repo

const q1 = "SELECT id FROM users WHERE active = 1"
const q2 = "SELECT name FROM users WHERE id = ?"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	selectCount := 0
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "SELECT" {
			selectCount++
		}
	}
	if selectCount != 1 {
		t.Errorf("expected 1 deduplicated SELECT storage fact for users, got %d", selectCount)
	}
}

func TestExtractStorage_NoStorage(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/util.go": `package util

func Add(a, b int) int {
	return a + b
}
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)
	if len(storage) != 0 {
		t.Errorf("expected 0 storage facts for non-storage file, got %d", len(storage))
	}
}

// English prose is not SQL. `FROM <word>` is an ordinary English phrase, and every one of
// these literals is real text from this repository's own help and error strings — each of
// which used to become a `storage` fact for a table named `the`, `what`, `one` or `in`.
//
// This is the regression guard for a stricter promise than the other tests here make. A
// false FINDING is an opinion a reader can weigh; a false FACT is part of the layer
// everything else is computed from, and enola's central claim is that its facts are derived
// rather than guessed. A regex reading prose as SQL is guessing.
func TestExtractStorage_ProseIsNotSQL(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/cli/help.go": `package cli

const (
	shapeNote  = "the shape below comes from the parent commits each snapshot recorded"
	orderNote  = "it says what happened next, never what came from what"
	coverNote  = "telling a genuinely isolated service apart from one whose edges were lost"
	graphNote  = "the revision it descends from in the graph, so those rows report movement"
	whereNote  = "the delta from the previous snapshot, where the numbers moved"
	updateNote = "update the baseline before you start, then check the diff after"
)
`,
	})

	if storage := findFactsByKind(ff, facts.KindStorage); len(storage) != 0 {
		var names []string
		for _, s := range storage {
			names = append(names, s.Name)
		}
		t.Errorf("prose produced %d storage fact(s) %v — a fact enola cannot derive must not be invented",
			len(storage), names)
	}
}

// The other half of the same promise: real SQL must still be read, including the shapes the
// gate could plausibly have broken — a multi-table join, a subquery, and a query spread over
// several lines.
func TestExtractStorage_RealSQLSurvivesTheProseGate(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/report.go": `package repo

const joined = ` + "`" + `
SELECT u.id, o.total
FROM users u
JOIN orders o ON o.user_id = u.id
WHERE u.active = 1
` + "`" + `

const subquery = "SELECT * FROM accounts WHERE id IN (SELECT account_id FROM ledger)"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)
	got := map[string]bool{}
	for _, s := range storage {
		if s.Props["operation"] == "SELECT" {
			got[s.Name] = true
		}
	}
	// The multi-line query's FROM table and its JOIN target, and the subquery's inner
	// table. A query names every table it reads, and a reader that stopped at FROM was
	// describing a two-table join as a one-table read.
	for _, want := range []string{"users", "orders", "accounts", "ledger"} {
		if !got[want] {
			t.Errorf("SELECT over %q was not extracted (got %v)", want, got)
		}
	}
}

// The reason JOIN is read from inside the FROM CLAUSE rather than from the literal.
//
// Grafana ships a 100 KB JSON schema as one Go string. Somewhere in it is the word SELECT,
// and somewhere else — thousands of characters away, in unrelated prose — is "join time".
// A literal-wide scan reports a table called `time`. Bounding the search to the span
// between FROM and the clause that ends it means a distant word cannot attach to a query it
// has nothing to do with.
func TestExtractStorage_DistantWordsDoNotJoinAQuery(t *testing.T) {
	filler := ""
	for i := 0; i < 200; i++ {
		filler += "documentation prose that goes on for a while and mentions nothing structural. "
	}
	ff := extractAll(t, map[string]string{
		"pkg/schema/schema.go": `package schema

const manifest = ` + "`" + `
{"description":"you may SELECT a panel type here","fields":{"x":1}}
` + filler + `
you can join time series together before rendering them
` + "`" + `
`,
	})

	for _, s := range findFactsByKind(ff, facts.KindStorage) {
		t.Errorf("a distant word was attached to an unrelated SELECT: table %q from %s", s.Name, s.File)
	}
}

// A comment inside a query is prose surrounded by SQL, so it defeats every gate: the
// literal really is SQL and the words in the comment are still English. Real example, from a
// Grafana migration, which produced a table called `was`.
func TestExtractStorage_CommentsInsideAQueryAreNotRead(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/migrate.go": `package db

const sqlite = ` + "`" + `
-- For SQLite we need to recreate the table with primary key. CREATE TABLE was
-- generated by ".schema file" command after running migration.
CREATE TABLE file_new (path TEXT PRIMARY KEY);
` + "`" + `
`,
	})

	got := map[string]bool{}
	for _, s := range findFactsByKind(ff, facts.KindStorage) {
		got[s.Name] = true
	}
	if got["was"] {
		t.Error("a word from a SQL comment was read as a table")
	}
	if !got["file_new"] {
		t.Errorf("the real CREATE TABLE was lost along with the comment (got %v)", got)
	}
}

// `sql = "CREATE TABLE IF NOT EXISTS "` — the table name is appended separately, so the
// literal ENDS after the keywords. Go's regexp is RE2 and has no atomic groups, so the
// optional `IF NOT EXISTS` clause backtracks and the capture lands on the keyword itself.
// Real code: Grafana's xorm does this in two places.
func TestExtractStorage_CreateTablePrefixIsNotATable(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/dialect.go": `package db

func prefix() string {
	sql := "CREATE TABLE IF NOT EXISTS "
	return sql
}
`,
	})

	for _, s := range findFactsByKind(ff, facts.KindStorage) {
		t.Errorf("a SQL keyword was read as a table name: %q", s.Name)
	}
}

func TestExtractStorage_AlterTable(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/migrations.go": `package db

const migration = "ALTER TABLE users ADD COLUMN phone VARCHAR(20)"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "ALTER" && s.Props["storage_kind"] == "table" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for ALTER TABLE users")
	}
}

// A hint in a block comment between the keywords does not hide the statement.
func TestExtractStorage_BlockCommentHintBetweenKeywords(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/cleanup.go": `package db

const purge = "DELETE /*+ BATCHED */ FROM shelves WHERE id IN (?)"
const lookup = "SELECT /* by title */ id FROM books /* primary */ WHERE title = ?"
`,
	})
	got := map[string]string{}
	for _, s := range findFactsByKind(ff, facts.KindStorage) {
		got[s.Name], _ = s.Props["operation"].(string)
	}
	if got["shelves"] != "DELETE" {
		t.Errorf("DELETE with a hint before FROM: got %v", got)
	}
	if got["books"] != "SELECT" {
		t.Errorf("SELECT with comments around the FROM clause: got %v", got)
	}
}

// A query that names its table through a package constant, with fmt.Sprintf or +,
// in another file of the package, is read as the text it builds. A part built at
// run time is not a table, and a format with no constant is left to its literal.
func TestExtractStorage_TableNamedByAConstant(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/shelf/tables.go": `package shelf

const (
	shelvesTable = "shelves"
	booksTable   = "books"
	loansTable   = "loa" + "ns"
	idColumn     = "id"
)
`,
		"pkg/shelf/queries.go": `package shelf

import (
	"fmt"
	"strings"
)

func placeholders(n int) string { return strings.Repeat("?,", n) }

func queries(n int) []string {
	return []string{
		fmt.Sprintf("SELECT %s, title FROM %s WHERE shelf_id IN (%s)", idColumn, booksTable, placeholders(n)),
		fmt.Sprintf("DELETE /*+ batched */ FROM %s "+
			"WHERE %s = ?", shelvesTable, idColumn),
		"UPDATE " + loansTable + " SET returned = 1 WHERE " + idColumn + " = ?",
		fmt.Sprintf("SELECT 1 FROM %s", placeholders(n)),
	}
}
`,
	})
	got := map[string]string{}
	for _, s := range findFactsByKind(ff, facts.KindStorage) {
		got[s.Name], _ = s.Props["operation"].(string)
	}
	want := map[string]string{"books": "SELECT", "shelves": "DELETE", "loans": "UPDATE"}
	for table, op := range want {
		if got[table] != op {
			t.Errorf("%s: operation %q, want %q (got %v)", table, got[table], op, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tables %v, want exactly %v", got, want)
	}
}

// A function depends on the tables its queries name: a query in its body, or one held
// in a package-level constant it refers to. A query outside any function names no user.
func TestExtractStorage_FunctionDependsOnItsTables(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"shelf/repo.go": `package shelf

import "fmt"

const booksTable = "books"

const listLoans = "SELECT id FROM loans WHERE returned = 0"

const migration = "CREATE TABLE archive (id INT)"

type Repo struct{}

func (r *Repo) Titles() string {
	return fmt.Sprintf("SELECT title FROM %s", booksTable)
}

func (r *Repo) Open() string { return listLoans }

func count() string { return "SELECT COUNT(*) FROM books JOIN loans ON loans.book_id = books.id" }
`,
	})
	for name, want := range map[string][]string{
		"shelf.Repo.Titles": {"books"},
		"shelf.Repo.Open":   {"loans"},
		"shelf.count":       {"books", "loans"},
	} {
		sym, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected a fact for %s", name)
		}
		for _, table := range want {
			if !hasRelation(sym, facts.RelDependsOn, table) {
				t.Errorf("%s should depend on %s; relations: %v", name, table, sym.Relations)
			}
		}
	}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if hasRelation(f, facts.RelDependsOn, "archive") {
			t.Errorf("%s depends on archive, which only a constant nothing refers to names", f.Name)
		}
	}
}
