package sqlite_test

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/sqlite"
)

var dumpSchema = flag.String("dump-schema", "", "write testdata/schema/`version`.sql from the current migrations")

// An instance that was installed at any release and upgraded must end with
// the schema of one installed today. testdata/schema holds the schema each
// release left behind; a released migration that is edited afterwards, or a
// migrator that treats an old database differently, shows up as a difference.
func TestMigrateFromEveryRelease(t *testing.T) {
	ctx := t.Context()
	fresh := openDB(t, filepath.Join(t.TempDir(), "kazari.db"))
	if *dumpSchema != "" {
		path := filepath.Join("testdata", "schema", *dumpSchema+".sql")
		if err := writeSchemaDump(ctx, fresh.DB, *dumpSchema, path); err != nil {
			t.Fatalf("writeSchemaDump(%q): %v", path, err)
		}
	}
	want, err := readSchema(ctx, fresh.DB)
	if err != nil {
		t.Fatalf("readSchema(fresh): %v", err)
	}

	dumps, err := filepath.Glob(filepath.Join("testdata", "schema", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dumps) == 0 {
		t.Fatal("no schema dumps in testdata/schema")
	}
	for _, dump := range dumps {
		t.Run(strings.TrimSuffix(filepath.Base(dump), ".sql"), func(t *testing.T) {
			script, err := os.ReadFile(dump)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "kazari.db")
			restoreDump(t, path, string(script))

			db := openDB(t, path)
			got, err := readSchema(ctx, db.DB)
			if err != nil {
				t.Fatalf("readSchema(%s): %v", dump, err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("schema after migrating %s differs from a fresh database (-fresh +migrated):\n%s", dump, diff)
			}
		})
	}
}

func TestNormaliseSQL(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "whitespace and comments",
			sql:  "CREATE TABLE t (\n    a INTEGER PRIMARY KEY, -- the id\n    b TEXT /* free\n text */ NOT NULL\n)",
			want: "CREATE TABLE t(a INTEGER PRIMARY KEY,b TEXT NOT NULL)",
		},
		{
			name: "literals are kept as written",
			sql:  "CREATE TABLE t (a TEXT DEFAULT 'x -- y,  (z)', \"b  c\" TEXT CHECK (a <> 'it''s'))",
			want: "CREATE TABLE t(a TEXT DEFAULT 'x -- y,  (z)',\"b  c\" TEXT CHECK(a <> 'it''s'))",
		},
		{
			name: "a constraint is not whitespace",
			sql:  "CREATE TABLE t (a INTEGER CHECK (a > 0))",
			want: "CREATE TABLE t(a INTEGER CHECK(a > 0))",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normaliseSQL(tt.sql); got != tt.want {
				t.Errorf("normaliseSQL(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

func openDB(t *testing.T, path string) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func restoreDump(t *testing.T, path, script string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(t.Context(), script); err != nil {
		t.Fatalf("restoring the dump: %v", err)
	}
}

type schema struct {
	Migrations []string
	Tables     map[string]table
	Triggers   map[string]string
	Views      map[string]string
}

type table struct {
	SQL          string
	WithoutRowID bool
	Strict       bool
	Columns      []column
	ForeignKeys  []string
	Indexes      map[string]index
}

type column struct {
	Name       string
	Type       string
	NotNull    bool
	Default    sql.NullString
	PrimaryKey int
	Hidden     int
}

type index struct {
	SQL     string
	Unique  bool
	Origin  string
	Partial bool
	Columns []string
}

// readSchema describes the schema by what SQLite made of it, so that two
// databases compare equal when they behave the same. The normalised SQL
// covers what the pragmas do not report: CHECK constraints, collations,
// generated columns, partial index conditions.
func readSchema(ctx context.Context, db *sql.DB) (schema, error) {
	s := schema{Tables: map[string]table{}, Triggers: map[string]string{}, Views: map[string]string{}}

	err := query(ctx, db, `SELECT version, name FROM schema_migration ORDER BY version`, nil, func(scan func(...any) error) error {
		var version int
		var name string
		if err := scan(&version, &name); err != nil {
			return err
		}
		s.Migrations = append(s.Migrations, fmt.Sprintf("%d %s", version, name))
		return nil
	})
	if err != nil {
		return schema{}, err
	}

	var tables []string
	err = query(ctx, db, `SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND type IN ('table', 'trigger', 'view')`, nil, func(scan func(...any) error) error {
		var kind, name, text string
		if err := scan(&kind, &name, &text); err != nil {
			return err
		}
		switch kind {
		case "table":
			tables = append(tables, name)
			s.Tables[name] = table{SQL: normaliseSQL(text)}
		case "trigger":
			s.Triggers[name] = normaliseSQL(text)
		case "view":
			s.Views[name] = normaliseSQL(text)
		}
		return nil
	})
	if err != nil {
		return schema{}, err
	}

	for _, name := range tables {
		tbl := s.Tables[name]
		tbl.Indexes = map[string]index{}

		err := db.QueryRowContext(ctx, `SELECT wr, strict FROM pragma_table_list(?) WHERE schema = 'main'`, name).Scan(&tbl.WithoutRowID, &tbl.Strict)
		if err != nil {
			return schema{}, fmt.Errorf("table_list(%s): %w", name, err)
		}

		err = query(ctx, db, `SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?) ORDER BY cid`, []any{name}, func(scan func(...any) error) error {
			var c column
			if err := scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PrimaryKey, &c.Hidden); err != nil {
				return err
			}
			tbl.Columns = append(tbl.Columns, c)
			return nil
		})
		if err != nil {
			return schema{}, fmt.Errorf("table_xinfo(%s): %w", name, err)
		}

		type foreignKey struct {
			from, to                      []string
			parent, update, delete, match string
		}
		keys := map[int]*foreignKey{}
		err = query(ctx, db, `SELECT id, "table", "from", coalesce("to", ''), on_update, on_delete, "match" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, []any{name}, func(scan func(...any) error) error {
			var id int
			var k foreignKey
			var from, to string
			if err := scan(&id, &k.parent, &from, &to, &k.update, &k.delete, &k.match); err != nil {
				return err
			}
			if keys[id] == nil {
				keys[id] = &k
			}
			keys[id].from = append(keys[id].from, from)
			keys[id].to = append(keys[id].to, to)
			return nil
		})
		if err != nil {
			return schema{}, fmt.Errorf("foreign_key_list(%s): %w", name, err)
		}
		for _, k := range keys {
			tbl.ForeignKeys = append(tbl.ForeignKeys, fmt.Sprintf("(%s) REFERENCES %s(%s) ON UPDATE %s ON DELETE %s MATCH %s",
				strings.Join(k.from, ","), k.parent, strings.Join(k.to, ","), k.update, k.delete, k.match))
		}
		sort.Strings(tbl.ForeignKeys)

		var indexes []string
		err = query(ctx, db, `SELECT l.name, l."unique", l.origin, l.partial, coalesce(m.sql, '')
			FROM pragma_index_list(?) AS l LEFT JOIN sqlite_master AS m ON m.type = 'index' AND m.name = l.name`, []any{name}, func(scan func(...any) error) error {
			var iname, text string
			var idx index
			if err := scan(&iname, &idx.Unique, &idx.Origin, &idx.Partial, &text); err != nil {
				return err
			}
			idx.SQL = normaliseSQL(text)
			tbl.Indexes[iname] = idx
			indexes = append(indexes, iname)
			return nil
		})
		if err != nil {
			return schema{}, fmt.Errorf("index_list(%s): %w", name, err)
		}
		for _, iname := range indexes {
			idx := tbl.Indexes[iname]
			err := query(ctx, db, `SELECT coalesce(name, '<expression>'), "desc", coll FROM pragma_index_xinfo(?) WHERE key ORDER BY seqno`, []any{iname}, func(scan func(...any) error) error {
				var col, coll string
				var desc bool
				if err := scan(&col, &desc, &coll); err != nil {
					return err
				}
				col += " COLLATE " + coll
				if desc {
					col += " DESC"
				}
				idx.Columns = append(idx.Columns, col)
				return nil
			})
			if err != nil {
				return schema{}, fmt.Errorf("index_xinfo(%s): %w", iname, err)
			}
			tbl.Indexes[iname] = idx
		}
		s.Tables[name] = tbl
	}
	return s, nil
}

func query(ctx context.Context, db *sql.DB, q string, args []any, row func(scan func(...any) error) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// normaliseSQL removes what does not change the meaning of a statement as
// sqlite_master stores it: comments, runs of white space, and the spaces
// around brackets and commas. Quoted text is kept as written.
func normaliseSQL(s string) string {
	var b strings.Builder
	space := false
	last := byte(0)
	emit := func(c byte) {
		tight := c == '(' || c == ')' || c == ',' || last == '(' || last == ','
		if space && b.Len() > 0 && !tight {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
		last = c
	}
	closing := map[byte]byte{'\'': '\'', '"': '"', '`': '`', '[': ']'}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
		case c == '-' && strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			i += end - 1
			space = true
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				end = len(s) - i - 4
			}
			i += end + 3
			space = true
		case closing[c] != 0:
			end := strings.IndexByte(s[i+1:], closing[c])
			if end < 0 {
				end = len(s) - i - 2
			}
			emit(c)
			b.WriteString(s[i+1 : i+end+2])
			last = closing[c]
			i += end + 1
		default:
			emit(c)
		}
	}
	return b.String()
}

// writeSchemaDump writes the statements that recreate the schema of db and
// the list of migrations it has applied.
func writeSchemaDump(ctx context.Context, db *sql.DB, version, path string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "-- The schema of a Kazari %s database, for TestMigrateFromEveryRelease.\n", version)
	b.WriteString("-- Written with -dump-schema when the version was released; never edited.\n\n")
	err := query(ctx, db, `SELECT sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY rowid`, nil, func(scan func(...any) error) error {
		var text string
		if err := scan(&text); err != nil {
			return err
		}
		b.WriteString(text + ";\n\n")
		return nil
	})
	if err != nil {
		return err
	}
	err = query(ctx, db, `SELECT version, name FROM schema_migration ORDER BY version`, nil, func(scan func(...any) error) error {
		var v int
		var name string
		if err := scan(&v, &name); err != nil {
			return err
		}
		fmt.Fprintf(&b, "INSERT INTO schema_migration (version, name, applied_at) VALUES (%d, '%s', 0);\n", v, strings.ReplaceAll(name, "'", "''"))
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
