package resultbuf

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"sql-manager/internal/dto"
)

func TestResultBufferSpillCapExpiryAndCleanup(t *testing.T) {
	spillDir := t.TempDir()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

	mgr := NewManager(Options{
		SpillDir:       spillDir,
		MaxMemRows:     5,
		MaxMemBytes:    1024 * 1024,
		MaxTotalRows:   12,
		MaxTotalBytes:  1024 * 1024,
		MaxGlobalBytes: 10 * 1024 * 1024,
		IdleTTL:        5 * time.Minute,
	})
	mgr.SetClock(func() time.Time { return now })

	cols := []dto.ColumnMeta{{Name: "n", DatabaseType: "INT", Kind: dto.KindInt}}
	b := mgr.NewBuilder("tab-1", "conn-1", cols)

	for i := 1; i <= 20; i++ {
		ok, err := b.Append([]dto.CellValue{{Kind: dto.KindInt, Text: strconv.Itoa(i)}})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if !ok {
			break
		}
	}
	resID, meta, err := b.Finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !meta.SpilledToDisk {
		t.Fatal("expected snapshot to spill to disk beyond MaxMemRows=5")
	}
	if !meta.TruncatedByLimit || meta.TotalBuffered != 12 {
		t.Fatalf("expected cap at 12 rows truncated=true, got %+v", meta)
	}

	// Verify spill file exists on disk
	spillFile := filepath.Join(spillDir, resID+".jsonl")
	if _, err := os.Stat(spillFile); err != nil {
		t.Fatalf("expected spill file at %s: %v", spillFile, err)
	}

	// Read page 2 (rows 6..10)
	p2, appErr := mgr.FetchPage(resID, 2, 5)
	if appErr != nil {
		t.Fatalf("fetch page 2 failed: %v", appErr)
	}
	if len(p2.Rows) != 5 || p2.Rows[0][0].Text != "6" || p2.Rows[4][0].Text != "10" || !p2.HasMorePages {
		t.Fatalf("unexpected page 2 rows: %+v", p2)
	}

	// Advance clock past IdleTTL -> must return ERR_RESULT_EXPIRED and remove spill file
	now = now.Add(6 * time.Minute)
	_, appErr = mgr.FetchPage(resID, 1, 5)
	if appErr == nil || appErr.Code != dto.ErrResultExpired {
		t.Fatalf("expected ERR_RESULT_EXPIRED, got %+v", appErr)
	}
	if _, err := os.Stat(spillFile); !os.IsNotExist(err) {
		t.Fatalf("expected spill file cleaned up after expiry, err=%v", err)
	}
}
