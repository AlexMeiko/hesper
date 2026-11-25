package repository

import (
	"context"
	"path/filepath"
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
