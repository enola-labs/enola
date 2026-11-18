package rubyextractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// fixtureStructureSQL is the pg_dump shape, with one table whose company_id
// carries the companies foreign key and one whose company_id does not — the
// compliant/violating pair the company-FK rule verdicts over.
const fixtureStructureSQL = `--
-- PostgreSQL database dump
--

SET statement_timeout = 0;

CREATE TABLE public.companies (
    id bigint NOT NULL,
    name character varying,
    "check" boolean DEFAULT false,
    created_at timestamp(6) without time zone NOT NULL,
    CONSTRAINT companies_name_check CHECK ((char_length((name)::text) > 0))
);

CREATE TABLE public.employments (
    id bigint NOT NULL,
    company_id bigint NOT NULL,
    title character varying
);

CREATE TABLE public.audit_rows (
    id bigint NOT NULL,
    company_id bigint
);

ALTER TABLE ONLY public.employments
    ADD CONSTRAINT employments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.employments
    ADD CONSTRAINT fk_rails_1d35bd72fd FOREIGN KEY (company_id) REFERENCES public.companies(id);

ALTER TABLE ONLY public.audit_rows
    ADD CONSTRAINT audit_rows_pkey PRIMARY KEY (id);
`

func writeStructureSQL(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "db", "structure.sql"), []byte(fixtureStructureSQL), 0o644); err != nil {
		t.Fatal(err)
	}
}

func factByName(ff []facts.Fact, kind, name string) *facts.Fact {
	for i := range ff {
		if ff[i].Kind == kind && ff[i].Name == name {
			return &ff[i]
		}
	}
	return nil
}

func TestExtractStructureSQL_TablesColumnsAndForeignKeys(t *testing.T) {
	repo := t.TempDir()
	writeStructureSQL(t, repo)
	got := applyStructureSQL(repo, nil)
	if len(got) != 3 {
		t.Fatalf("facts = %d, want the three declared tables: %+v", len(got), got)
	}

	companies := factByName(got, facts.KindStorage, "companies")
	if companies == nil || companies.File != structureSQLPath {
		t.Fatalf("companies fact = %+v", companies)
	}
	// Sorted census, quoted reserved-word column included, the table-level
	// CONSTRAINT clause excluded.
	if cols, _ := companies.Props["columns"].(string); cols != "check created_at id name" {
		t.Errorf("companies columns = %q", cols)
	}
	if _, hasFK := companies.Props["fk_constraints"]; hasFK {
		t.Errorf("companies must carry no fk census: %+v", companies.Props)
	}

	employments := factByName(got, facts.KindStorage, "employments")
	if cols, _ := employments.Props["columns"].(string); cols != "company_id id title" {
		t.Errorf("employments columns = %q", cols)
	}
	if fks, _ := employments.Props["fk_constraints"].(string); fks != "company_id->companies" {
		t.Errorf("employments fk_constraints = %q", fks)
	}

	// The violating table: the column without the constraint.
	audit := factByName(got, facts.KindStorage, "audit_rows")
	if cols, _ := audit.Props["columns"].(string); cols != "company_id id" {
		t.Errorf("audit_rows columns = %q", cols)
	}
	if _, hasFK := audit.Props["fk_constraints"]; hasFK {
		t.Errorf("audit_rows must carry no fk census: %+v", audit.Props)
	}
}

// A table an ActiveRecord model already claims gets its census folded onto the
// model's storage fact — one table, one storage identity.
func TestExtractStructureSQL_ModelClaimedTableGetsCensusNotASecondFact(t *testing.T) {
	repo := t.TempDir()
	writeStructureSQL(t, repo)
	allFacts := []facts.Fact{{
		Kind: facts.KindStorage,
		Name: "Employment",
		File: "app/models/employment.rb",
		Props: map[string]any{
			"storage_kind": "model",
			"table":        "employments",
			"table_source": "derived",
		},
	}}
	got := applyStructureSQL(repo, allFacts)
	if factByName(got, facts.KindStorage, "employments") != nil {
		t.Fatal("a model-claimed table must not become a second storage fact")
	}
	model := allFacts[0]
	if cols, _ := model.Props["columns"].(string); cols != "company_id id title" {
		t.Errorf("model fact columns = %q, want the folded census", cols)
	}
	if fks, _ := model.Props["fk_constraints"].(string); fks != "company_id->companies" {
		t.Errorf("model fact fk_constraints = %q", fks)
	}
}

func TestExtractStructureSQL_MissingFileIsSilence(t *testing.T) {
	if got := applyStructureSQL(t.TempDir(), nil); got != nil {
		t.Fatalf("no structure.sql must mean no facts, got %+v", got)
	}
}

// Unrecognized shapes are skipped, never guessed: a composite foreign key has
// no honest column->table pair form, and a dynamic CREATE line outside the
// pg_dump shape contributes nothing.
func TestExtractStructureSQL_UnrecognizedShapesAreSkipped(t *testing.T) {
	repo := t.TempDir()
	src := "CREATE TABLE public.pairs (\n" +
		"    left_id bigint,\n" +
		"    right_id bigint\n" +
		");\n" +
		"ALTER TABLE ONLY public.pairs\n" +
		"    ADD CONSTRAINT fk_composite FOREIGN KEY (left_id, right_id) REFERENCES public.things(a, b);\n" +
		"CREATE TABLE weird (\n" +
		"    UPPER_CASE bigint\n" +
		");\n"
	if err := os.MkdirAll(filepath.Join(repo, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "db", "structure.sql"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := applyStructureSQL(repo, nil)
	pairs := factByName(got, facts.KindStorage, "pairs")
	if pairs == nil {
		t.Fatal("the well-formed table must still be read")
	}
	if _, hasFK := pairs.Props["fk_constraints"]; hasFK {
		t.Errorf("a composite foreign key must be skipped, got %+v", pairs.Props)
	}
	weird := factByName(got, facts.KindStorage, "weird")
	if weird == nil {
		t.Fatal("the unqualified table name is still a pg shape")
	}
	if _, hasCols := weird.Props["columns"]; hasCols {
		t.Errorf("an uppercase first token is not a recognizable column: %+v", weird.Props)
	}
}

// The mysqldump shape: backquoted names, keys and foreign keys inside the CREATE
// TABLE, and a table closed by ") ENGINE=…;". Each table is its own fact, not the
// first one swallowing the rest of the file.
func TestParseStructureSQL_MySQLDump(t *testing.T) {
	const dump = "CREATE TABLE `shops` (\n" +
		"  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
		"  `name` varchar(255) NOT NULL,\n" +
		"  PRIMARY KEY (`id`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n" +
		"/*!40101 SET character_set_client = @saved_cs_client */;\n" +
		"CREATE TABLE `shelves` (\n" +
		"  `id` bigint(20) NOT NULL AUTO_INCREMENT,\n" +
		"  `shop_id` bigint(20) NOT NULL,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  KEY `index_shelves_on_shop_id` (`shop_id`),\n" +
		"  CONSTRAINT `fk_rails_1` FOREIGN KEY (`shop_id`) REFERENCES `shops` (`id`)\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n"
	tables := parseStructureSQL(dump)
	if len(tables) != 2 || tables["shops"] == nil || tables["shelves"] == nil {
		t.Fatalf("tables = %v, want shops and shelves", tables)
	}
	if got := strings.Join(tables["shops"].columns, " "); got != "id name" {
		t.Errorf("shops columns = %q", got)
	}
	if got := strings.Join(tables["shelves"].columns, " "); got != "id shop_id" {
		t.Errorf("shelves columns = %q", got)
	}
	if got := strings.Join(tables["shelves"].fks, " "); got != "shop_id->shops" {
		t.Errorf("shelves fks = %q", got)
	}
}
