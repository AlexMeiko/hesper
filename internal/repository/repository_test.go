package repository

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sql-manager/internal/entity"
)

func TestLocalDB_CRUDHistoryKVAndEmbeddedSQLiteVersion(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app.db")
	db, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatalf("OpenLocalDB: %v", err)
	}
	defer db.Close()

	ver, err := db.EmbeddedSQLiteVersion(ctx)
	if err != nil {
		t.Fatalf("EmbeddedSQLiteVersion: %v", err)
	}
	t.Logf("Embedded modernc.org/sqlite version: %s", ver)
	if ver == "" {
		t.Fatalf("expected non-empty sqlite version")
	}

	now := time.Now().UnixMilli()
	ent := entity.ConnectionEntity{
		ID:         "conn-1",
		Name:       "MySQL 8.4 Demo",
		Driver:     "mysql",
		EnvColor:   "green",
		ReadOnly:   true,
		ConfigJSON: `{"host":"127.0.0.1","port":13306,"username":"sqlm_ro","defaultDatabase":"sqlm_demo"}`,
		SecretRef:  "conn:conn-1:password",
		SortOrder:  1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.UpsertConnection(ctx, ent); err != nil {
		t.Fatalf("UpsertConnection: %v", err)
	}
	got, err := db.GetConnection(ctx, "conn-1")
	if err != nil || got == nil {
		t.Fatalf("GetConnection: got=%v err=%v", got, err)
	}
	if !got.ReadOnly || got.Name != ent.Name {
		t.Fatalf("unexpected connection: %+v", got)
	}

	list, err := db.ListConnections(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListConnections: len=%d err=%v", len(list), err)
	}

	if err := db.AppendHistory(ctx, entity.QueryHistoryEntity{
		ID:           "hist-1",
		ConnectionID: "conn-1",
		DatabaseName: "sqlm_demo",
		SQLText:      "SELECT 1",
		Status:       "ok",
		DurationMs:   3,
		AffectedRows: 0,
		ExecutedAt:   now,
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	hist, err := db.ListHistory(ctx, "conn-1", 10)
	if err != nil || len(hist) != 1 || hist[0].SQLText != "SELECT 1" {
		t.Fatalf("ListHistory: %+v err=%v", hist, err)
	}

	if err := db.SetKV(ctx, "theme", `"light"`); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	val, ok, err := db.GetKV(ctx, "theme")
	if err != nil || !ok || val != `"light"` {
		t.Fatalf("GetKV: val=%q ok=%v err=%v", val, ok, err)
	}

	if err := db.DeleteConnection(ctx, "conn-1"); err != nil {
		t.Fatalf("DeleteConnection: %v", err)
	}
	after, err := db.GetConnection(ctx, "conn-1")
	if err != nil || after != nil {
		t.Fatalf("expected deleted connection to be nil, got=%v err=%v", after, err)
	}
}

func TestBuildLocalFileURI_CrossPlatform(t *testing.T) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")

	gotWin := buildLocalFileURI("C:/Users/Alex #1/AppData/Roaming/sql-manager/app.db", q)
	if !strings.HasPrefix(gotWin, "file:///C:/Users/Alex%20%231/AppData/Roaming/sql-manager/app.db?") {
		t.Fatalf("unexpected Windows local DB URI: %q", gotWin)
	}
	parsed, err := url.Parse(gotWin)
	if err != nil || parsed.Host != "" {
		t.Fatalf("expected empty authority for Windows local DB URI, got parsed=%+v err=%v", parsed, err)
	}

	gotUnix := buildLocalFileURI("/home/demo/.config/sql-manager/app.db", q)
	if !strings.HasPrefix(gotUnix, "file:///home/demo/.config/sql-manager/app.db?") {
		t.Fatalf("unexpected Unix local DB URI: %q", gotUnix)
	}
}

func TestOpenLocalDB_SpecialCharactersPath(t *testing.T) {
	subDir := filepath.Join(t.TempDir(), "cfg #1 100% 目录")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := OpenLocalDB(filepath.Join(subDir, "app #1.db"))
	if err != nil {
		t.Fatalf("OpenLocalDB in special-char path failed: %v", err)
	}
	defer db.Close()

	ver, err := db.EmbeddedSQLiteVersion(context.Background())
	if err != nil || ver == "" {
		t.Fatalf("EmbeddedSQLiteVersion failed: ver=%q err=%v", ver, err)
	}
}
