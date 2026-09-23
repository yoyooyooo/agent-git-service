package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var delegatedMergeCatalogDriverSequence uint64

type delegatedMergeCatalogDriver struct {
	columns []string
	rows    [][]driver.Value
}

func (d *delegatedMergeCatalogDriver) Open(string) (driver.Conn, error) {
	return &delegatedMergeCatalogConn{driver: d}, nil
}

type delegatedMergeCatalogConn struct{ driver *delegatedMergeCatalogDriver }

func (*delegatedMergeCatalogConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (*delegatedMergeCatalogConn) Close() error { return nil }
func (*delegatedMergeCatalogConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}
func (c *delegatedMergeCatalogConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	lower := strings.ToLower(query)
	if !strings.Contains(lower, "show index") && !strings.Contains(lower, "pg_catalog.pg_index") {
		return nil, fmt.Errorf("unexpected catalog query: %s", query)
	}
	return &delegatedMergeCatalogRows{columns: c.driver.columns, rows: c.driver.rows}, nil
}

type delegatedMergeCatalogRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *delegatedMergeCatalogRows) Columns() []string { return r.columns }
func (*delegatedMergeCatalogRows) Close() error        { return nil }
func (r *delegatedMergeCatalogRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

func openDelegatedMergeCatalogDB(t *testing.T, dialect string, columns []string, rows [][]driver.Value) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("delegated_merge_catalog_%d", atomic.AddUint64(&delegatedMergeCatalogDriverSequence, 1))
	sql.Register(name, &delegatedMergeCatalogDriver{columns: columns, rows: rows})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var dialector gorm.Dialector
	switch dialect {
	case "mysql":
		dialector = mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true})
	case "postgres":
		dialector = postgres.New(postgres.Config{Conn: sqlDB, PreferSimpleProtocol: true, WithoutReturning: true})
	default:
		t.Fatalf("unsupported test dialect %s", dialect)
	}
	database, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Discard, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func TestDelegatedMergeMySQLNativeCatalogRejectsInvisiblePrefixAndExpression(t *testing.T) {
	columns := []string{"Table", "Non_unique", "Key_name", "Seq_in_index", "Column_name", "Collation", "Cardinality", "Sub_part", "Packed", "Null", "Index_type", "Comment", "Index_comment", "Visible", "Expression"}
	base := []driver.Value{delegatedPRMergeIntentTable, int64(0), delegatedPRMergeIntentIndexName, int64(1), delegatedPRMergeIntentColumn, "A", int64(1), nil, nil, "YES", "BTREE", "", "", "YES", nil}
	definitions, err := delegatedMergeIndexDefinitions(openDelegatedMergeCatalogDB(t, "mysql", columns, [][]driver.Value{base}))
	if err != nil {
		t.Fatal(err)
	}
	if found, exact := exactDelegatedMergeIndexDefinition(definitions); !found || !exact {
		t.Fatalf("exact MySQL/TiDB native catalog proof rejected: %#v", definitions)
	}
	for name, mutate := range map[string]func([]driver.Value){
		"invisible":  func(row []driver.Value) { row[13] = "NO" },
		"prefix":     func(row []driver.Value) { row[7] = int64(8) },
		"expression": func(row []driver.Value) { row[14] = "lower(delegation_id)" },
	} {
		t.Run(name, func(t *testing.T) {
			row := append([]driver.Value(nil), base...)
			mutate(row)
			definitions, err := delegatedMergeIndexDefinitions(openDelegatedMergeCatalogDB(t, "mysql", columns, [][]driver.Value{row}))
			if err != nil {
				t.Fatal(err)
			}
			if found, exact := exactDelegatedMergeIndexDefinition(definitions); !found || exact {
				t.Fatalf("unsafe MySQL/TiDB catalog artifact accepted: %#v", definitions)
			}
		})
	}
}

func TestDelegatedMergePostgresNativeCatalogRejectsInvalidNotReadyAndExpression(t *testing.T) {
	columns := []string{"table_name", "index_name", "is_unique", "is_valid", "is_ready", "no_predicate", "no_expression", "key_attribute_count", "attribute_count", "ordinality", "column_name", "key_definition"}
	base := []driver.Value{delegatedPRMergeIntentTable, delegatedPRMergeIntentIndexName, true, true, true, true, true, int64(1), int64(1), int64(1), delegatedPRMergeIntentColumn, delegatedPRMergeIntentColumn}
	definitions, err := delegatedMergeIndexDefinitions(openDelegatedMergeCatalogDB(t, "postgres", columns, [][]driver.Value{base}))
	if err != nil {
		t.Fatal(err)
	}
	if found, exact := exactDelegatedMergeIndexDefinition(definitions); !found || !exact {
		t.Fatalf("exact PostgreSQL native catalog proof rejected: %#v", definitions)
	}
	for name, mutate := range map[string]func([]driver.Value){
		"invalid":     func(row []driver.Value) { row[3] = false },
		"not ready":   func(row []driver.Value) { row[4] = false },
		"predicate":   func(row []driver.Value) { row[5] = false },
		"expression":  func(row []driver.Value) { row[6], row[10], row[11] = false, "", "lower(delegation_id)" },
		"include col": func(row []driver.Value) { row[8] = int64(2) },
	} {
		t.Run(name, func(t *testing.T) {
			row := append([]driver.Value(nil), base...)
			mutate(row)
			definitions, err := delegatedMergeIndexDefinitions(openDelegatedMergeCatalogDB(t, "postgres", columns, [][]driver.Value{row}))
			if err != nil {
				t.Fatal(err)
			}
			if found, exact := exactDelegatedMergeIndexDefinition(definitions); !found || exact {
				t.Fatalf("unsafe PostgreSQL catalog artifact accepted: %#v", definitions)
			}
		})
	}
}
