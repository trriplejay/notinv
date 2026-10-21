package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestSchema(t *testing.T) {
	s, _ := openTestStore(t)
	for _, tc := range []struct {
		table    string
		names    []string
		types    []string
		nullable []string
	}{
		{
			table:    "requests",
			names:    []string{"id", "key", "script", "method", "url", "started_at", "duration_ms", "status_code", "error"},
			types:    []string{"bigint", "text", "text", "text", "text", "bigint", "bigint", "integer", "text"},
			nullable: []string{"NO", "NO", "NO", "NO", "NO", "NO", "NO", "NO", "YES"},
		},
		{
			table:    "runs",
			names:    []string{"id", "key", "script", "started_at", "duration_ms", "ok", "error"},
			types:    []string{"bigint", "text", "text", "bigint", "bigint", "boolean", "text"},
			nullable: []string{"NO", "NO", "NO", "NO", "NO", "NO", "YES"},
		},
	} {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := s.db.QueryContext(t.Context(), `
				SELECT column_name, data_type, is_nullable
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1
				ORDER BY ordinal_position`, tc.table)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := rows.Close(); err != nil {
					t.Error(err)
				}
			}()
			var names, types, nullable []string
			for rows.Next() {
				var name, columnType, isNullable string
				if err := rows.Scan(&name, &columnType, &isNullable); err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
				types = append(types, columnType)
				nullable = append(nullable, isNullable)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(names, tc.names) || !reflect.DeepEqual(types, tc.types) || !reflect.DeepEqual(nullable, tc.nullable) {
				t.Fatalf("schema = names:%v types:%v nullable:%v, want names:%v types:%v nullable:%v",
					names, types, nullable, tc.names, tc.types, tc.nullable)
			}

			for i, name := range names {
				if tc.table == "runs" && name == "ok" {
					if types[i] != "boolean" || nullable[i] != "NO" {
						t.Errorf("runs.ok type=%q nullable=%q, want boolean/NO", types[i], nullable[i])
					}
				}
				if name == "started_at" {
					if types[i] != "bigint" || nullable[i] != "NO" {
						t.Errorf("%s.started_at type=%q nullable=%q, want bigint/NO", tc.table, types[i], nullable[i])
					}
				}
			}

			index := "idx_" + tc.table + "_script_started"
			var indexDef string
			if err := s.db.QueryRowContext(t.Context(), `
				SELECT indexdef
				FROM pg_indexes
				WHERE schemaname = current_schema() AND indexname = $1`, index).Scan(&indexDef); err != nil {
				t.Fatalf("index %s missing from %s: %v", index, tc.table, err)
			}
			if !strings.Contains(indexDef, "(script, started_at)") {
				t.Fatalf("index %s definition = %q, want (script, started_at)", index, indexDef)
			}
		})
	}
}
