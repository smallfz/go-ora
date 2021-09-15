/* -*- coding: utf-8-unix -*- */

package tests

import (
	"context"
	"database/sql"
	// "fmt"
	_ "github.com/sijms/go-ora"
	"os"
	"testing"
)

func TestQueryNullValues(t *testing.T) {
	dsn := os.Getenv("GOORA_TESTDB")
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
		return
	}
	defer db.Close()

	cmdText := `select 'x' v from dual where 1=2 union all select NULL v from dual`
	cmdText = `select '' v from dual`

	ctx := context.TODO()
	rows, err := db.QueryContext(ctx, cmdText)
	if err != nil {
		t.Fatalf("db.QueryContext: %v", err)
		return
	}

	for rows.Next() {
		nilStr := &sql.NullString{}
		if err := rows.Scan(&nilStr); err != nil {
			t.Fatalf("rows.Scan: %v", err)
		} else {
			// if len(nilStr) > 0 {
			// 	dat := []byte(nilStr)
			// 	fmt.Printf("%v\n", dat)
			// 	t.Fatalf("should be an empty string: `%s`", nilStr)
			// }
			if nilStr != nil && len(nilStr.String) > 0 {
				t.Fatalf("non empty: %v\n", nilStr.String)
			}
		}
	}
}

// [{V 0 false true   CHAR false 128 0 0 1 1 0 0 [] 0 873 1 [] <nil> true}]
// [{V 0 false true  NCHAR false 128 0 0 0 0 0 0 [] 0 1 1 [] <nil> true}]
