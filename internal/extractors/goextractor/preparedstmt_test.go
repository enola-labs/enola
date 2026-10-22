package goextractor

import "testing"

// A prepared statement is the result of Prepare, which a library's source does
// not declare here. Untyped, `stmt.ExecContext` in a loop resolved to nothing,
// and a prepared insert per element read as no query. The cursor a Query returns
// is typed the same way, so its reads are pure calls rather than unknowns.
func TestPreparedStatementInALoopIsADatabaseCall(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/repo.go": `package pkg

import (
	"context"
	"database/sql"
)

func Store(ctx context.Context, tx *sql.Tx, ids []int) error {
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO t (id) VALUES (?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func List(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT id FROM t")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int
		_ = rows.Scan(&id)
	}
	return nil
}
`,
	})
	store, _ := findFact(ff, "pkg.Store")
	if !containsStr(strSliceProp(store, "calls_in_scaling_loop"), "database/sql.Stmt.ExecContext") {
		t.Errorf("calls_in_scaling_loop = %v, want database/sql.Stmt.ExecContext", store.Props["calls_in_scaling_loop"])
	}
	if !containsStr(strSliceProp(store, "db_calls"), "database/sql.Stmt.ExecContext") {
		t.Errorf("db_calls = %v, want the prepared statement's ExecContext", store.Props["db_calls"])
	}
	list, _ := findFact(ff, "pkg.List")
	if pure := strSliceProp(list, "pure_calls"); !containsStr(pure, "database/sql.Rows.Next") || !containsStr(pure, "database/sql.Rows.Scan") {
		t.Errorf("pure_calls = %v, want the cursor's Next and Scan", pure)
	}
}

// A ping is I/O, but no query: a connect-retry loop around one is not a query per
// element.
func TestPingIsIONotADatabaseCall(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/conn.go": `package pkg

import (
	"context"
	"database/sql"
)

func Connect(ctx context.Context, db *sql.DB, retries int) error {
	var err error
	for attempt := 0; attempt < retries; attempt++ {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
	}
	return err
}
`,
	})
	f, _ := findFact(ff, "pkg.Connect")
	if v, _ := f.Props["io_direct"].(bool); !v {
		t.Errorf("io_direct = %v, want true: a ping is I/O", f.Props["io_direct"])
	}
	if calls := strSliceProp(f, "db_calls"); len(calls) != 0 {
		t.Errorf("db_calls = %v, want none: a ping is no query", calls)
	}
}
