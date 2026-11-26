package sqlite

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sql-manager/internal/driver"
	"sql-manager/internal/dto"
)

func TestBuildFileURI_CrossPlatformAuthorityAndEscaping(t *testing.T) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(ON)")

	cases := []struct {
		name           string
		slashPath      string
		wantPrefix     string
		wantParsedPath string
	}{
		{
			name:           "unix absolute path",
			slashPath:      "/tmp/sqlm/gate1_sqlite.db",
			wantPrefix:     "file:///tmp/sqlm/gate1_sqlite.db?",
			wantParsedPath: "/tmp/sqlm/gate1_sqlite.db",
		},
		{
			name:           "windows drive path must use empty authority (three slashes)",
			slashPath:      "C:/Users/runneradmin/AppData/Local/Temp/TestSQLite/gate1_sqlite.db",
			wantPrefix:     "file:///C:/Users/runneradmin/AppData/Local/Temp/TestSQLite/gate1_sqlite.db?",
			wantParsedPath: "/C:/Users/runneradmin/AppData/Local/Temp/TestSQLite/gate1_sqlite.db",
		},
		{
			name:           "windows lowercase drive with special URI characters",
			slashPath:      "d:/my data #1/test?v%20库.sqlite",
			wantPrefix:     "file:///d:/my%20data%20%231/test%3Fv%2520%E5%BA%93.sqlite?",
			wantParsedPath: "/d:/my data #1/test?v%20库.sqlite",
		},
		{
			name:           "windows extended-length drive path",
			slashPath:      "//?/C:/Users/demo/app.db",
			wantPrefix:     "file:///C:/Users/demo/app.db?",
			wantParsedPath: "/C:/Users/demo/app.db",
		},
		{
			name:           "windows UNC path keeps empty authority",
			slashPath:      "//fileserver/share/app.db",
			wantPrefix:     "file:////fileserver/share/app.db?",
			wantParsedPath: "//fileserver/share/app.db",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFileURI(tc.slashPath, q)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("buildFileURI(%q) = %q, want prefix %q", tc.slashPath, got, tc.wantPrefix)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", got, err)
			}
			if parsed.Scheme != "file" {
				t.Fatalf("scheme = %q, want file", parsed.Scheme)
			}
			if parsed.Host != "" {
				t.Fatalf("URI authority MUST be empty for SQLite, got Host=%q in %q", parsed.Host, got)
			}
			if parsed.Path != tc.wantParsedPath {
				t.Fatalf("parsed.Path = %q, want %q", parsed.Path, tc.wantParsedPath)
			}
			if parsed.Query().Get("mode") != "ro" {
				t.Fatalf("mode query param lost in %q", got)
			}
			if gotPragmas := parsed.Query()["_pragma"]; len(gotPragmas) != 1 || gotPragmas[0] != "query_only(ON)" {
				t.Fatalf("unexpected _pragma params %v in %q", gotPragmas, got)
			}
		})
	}
}

func TestBuildDSN_Memory(t *testing.T) {
	rw := buildDSN(":memory:", true, false)
	if !strings.HasPrefix(rw, "file::memory:?") || strings.Contains(rw, "query_only") {
		t.Fatalf("unexpected rw memory DSN: %q", rw)
	}
	ro := buildDSN(":memory:", true, true)
	if !strings.HasPrefix(ro, "file::memory:?") || !strings.Contains(ro, "query_only(ON)") {
		t.Fatalf("unexpected ro memory DSN: %q", ro)
	}
}

func TestOpen_SpecialCharactersPath(t *testing.T) {
	// Use characters valid on both Linux and Windows NTFS (spaces, #, %, Unicode).
	subDir := filepath.Join(t.TempDir(), "dir #1 100% 测试")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(subDir, "my #db %20.sqlite")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t VALUES (1, 'ok');`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed exec: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ctx := context.Background()
	conn, err := NewOpener().Open(ctx, driver.ResolvedConfig{
		ConnectionID: "special-path",
		Engine:       dto.EngineSQLite,
		FilePath:     dbPath,
		ReadOnly:     true,
	})
	if err != nil {
		t.Fatalf("Open special-char SQLite file failed: %v", err)
	}
	defer conn.Close()

	fact, err := conn.ServerFact(ctx)
	if err != nil || fact.ServerVersion == "" {
		t.Fatalf("ServerFact failed: fact=%+v err=%v", fact, err)
	}
}
