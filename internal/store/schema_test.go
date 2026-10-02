package store

import (
	"reflect"
	"testing"
)

func TestSchema(t *testing.T) {
	s, _ := openTestStore(t)
	for _, tc := range []struct {
		table   string
		names   []string
		types   []string
		notNull []int
	}{
		{"requests", []string{"script", "method", "url", "started_at", "duration_ms", "status_code", "error"},
			[]string{"TEXT", "TEXT", "TEXT", "INTEGER", "INTEGER", "INTEGER", "TEXT"}, []int{1, 1, 1, 1, 1, 1, 0}},
		{"runs", []string{"script", "started_at", "duration_ms", "ok", "error"},
			[]string{"TEXT", "INTEGER", "INTEGER", "INTEGER", "TEXT"}, []int{1, 1, 1, 1, 0}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := s.db.QueryContext(t.Context(), `SELECT name, type, "notnull" FROM pragma_table_info(?) ORDER BY cid`, tc.table)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := rows.Close(); err != nil {
					t.Error(err)
				}
			}()
			var names, types []string
			var notNull []int
			for rows.Next() {
				var name, columnType string
				var required int
				if err := rows.Scan(&name, &columnType, &required); err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
				types = append(types, columnType)
				notNull = append(notNull, required)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(names, tc.names) || !reflect.DeepEqual(types, tc.types) || !reflect.DeepEqual(notNull, tc.notNull) {
				t.Fatalf("schema = %v %v %v, want %v %v %v", names, types, notNull, tc.names, tc.types, tc.notNull)
			}

			index := "idx_" + tc.table + "_script_started"
			var count int
			if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM pragma_index_list(?) WHERE name = ?`, tc.table, index).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("index %s missing from %s", index, tc.table)
			}
			indexRows, err := s.db.QueryContext(t.Context(), `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := indexRows.Close(); err != nil {
					t.Error(err)
				}
			}()
			var columns []string
			for indexRows.Next() {
				var name string
				if err := indexRows.Scan(&name); err != nil {
					t.Fatal(err)
				}
				columns = append(columns, name)
			}
			if err := indexRows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(columns, []string{"script", "started_at"}) {
				t.Fatalf("index columns = %v", columns)
			}
		})
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO runs (script, started_at, duration_ms, ok) VALUES (?, ?, ?, ?)`, "bad", 0, 0, 2); err == nil {
		t.Fatal("ok column accepted a value other than 0/1")
	}
}
