package resultbuf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hesper/internal/dto"
)

// Options configures ResultBuffer thresholds, spill limits, and snapshot TTL.
type Options struct {
	SpillDir       string
	MaxMemRows     int           // Spill to temp file beyond this row count (default 5000)
	MaxMemBytes    int64         // Spill to temp file beyond this byte count (default 8 MiB)
	MaxTotalRows   int64         // Hard cap per Execute-Once snapshot (default 50000)
	MaxTotalBytes  int64         // Hard cap per Execute-Once snapshot (default 64 MiB)
	MaxGlobalBytes int64         // Global quota across all open snapshots (default 512 MiB)
	IdleTTL        time.Duration // Snapshot idle expiry (default 30m)
}

func (o Options) withDefaults() Options {
	if o.MaxMemRows <= 0 {
		o.MaxMemRows = 5000
	}
	if o.MaxMemBytes <= 0 {
		o.MaxMemBytes = 8 * 1024 * 1024
	}
	if o.MaxTotalRows <= 0 {
		o.MaxTotalRows = 50000
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 64 * 1024 * 1024
	}
	if o.MaxGlobalBytes <= 0 {
		o.MaxGlobalBytes = 512 * 1024 * 1024
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = 30 * time.Minute
	}
	if o.SpillDir == "" {
		o.SpillDir = filepath.Join(os.TempDir(), "hesper-results")
	}
	return o
}

// StreamBlob stores a lazy-loaded binary or oversized text cell payload.
type StreamBlob struct {
	ContentType string
	Filename    string
	Data        []byte
	ResultID    string
	TabID       string
}

type snapshot struct {
	id               string
	tabID            string
	connID           string
	columns          []dto.ColumnMeta
	memRows          [][]dto.CellValue
	spilled          bool
	spillPath        string
	rowOffsets       []int64 // byte offsets in spill file for O(1) random page seeks
	totalRows        int64
	totalBytes       int64
	truncatedByLimit bool
	limitReason      string
	createdAt        time.Time
	lastAccessed     time.Time
	streamRefs       []string
}

// Manager owns all Execute-Once query snapshots and lazy cell stream blobs.
// Transparently switches between in-memory slices and spilled temp files;
// the frontend is completely unaware of storage tier.
type Manager struct {
	mu          sync.Mutex
	opts        Options
	snapshots   map[string]*snapshot
	blobs       map[string]*StreamBlob
	globalBytes int64
	seq         uint64
	nowFn       func() time.Time
}

func NewManager(opts Options) *Manager {
	opts = opts.withDefaults()
	_ = os.MkdirAll(opts.SpillDir, 0o700)
	return &Manager{
		opts:      opts,
		snapshots: make(map[string]*snapshot),
		blobs:     make(map[string]*StreamBlob),
		nowFn:     time.Now,
	}
}

// SetClock overrides time source for deterministic expiry tests.
func (m *Manager) SetClock(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFn = fn
}

// Builder incrementally appends streamed rows into one Execute-Once snapshot.
type Builder struct {
	mgr       *Manager
	snap      *snapshot
	spillFile *os.File
	spillBuf  *bufio.Writer
	spillPos  int64
}

func (m *Manager) NewBuilder(tabID, connID string, columns []dto.ColumnMeta) *Builder {
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("res-%d-%d", m.nowFn().UnixNano(), m.seq)
	now := m.nowFn()
	m.mu.Unlock()

	return &Builder{
		mgr: m,
		snap: &snapshot{
			id:           id,
			tabID:        tabID,
			connID:       connID,
			columns:      columns,
			createdAt:    now,
			lastAccessed: now,
		},
	}
}

// RegisterBlob stores an oversized or binary cell payload tied to this snapshot's lifecycle.
func (b *Builder) RegisterBlob(contentType, filename string, data []byte) string {
	b.mgr.mu.Lock()
	defer b.mgr.mu.Unlock()
	b.mgr.seq++
	ref := fmt.Sprintf("blob-%d-%d", b.mgr.nowFn().UnixNano(), b.mgr.seq)
	cp := make([]byte, len(data))
	copy(cp, data)
	b.mgr.blobs[ref] = &StreamBlob{
		ContentType: contentType,
		Filename:    filename,
		Data:        cp,
		ResultID:    b.snap.id,
		TabID:       b.snap.tabID,
	}
	b.snap.streamRefs = append(b.snap.streamRefs, ref)
	return ref
}

// Append adds one row to the snapshot. Returns false if capacity limit was reached
// (caller should immediately stop reading and close the DB RowStream).
func (b *Builder) Append(row []dto.CellValue) (bool, error) {
	opts := b.mgr.opts
	if b.snap.totalRows >= opts.MaxTotalRows {
		b.snap.truncatedByLimit = true
		b.snap.limitReason = fmt.Sprintf("已达到预览行数上限 (%d 行)，如需完整数据请增加筛选条件或导出", opts.MaxTotalRows)
		return false, nil
	}
	rowBytes := estimateRowBytes(row)
	if b.snap.totalRows > 0 && b.snap.totalBytes+rowBytes > opts.MaxTotalBytes {
		b.snap.truncatedByLimit = true
		b.snap.limitReason = fmt.Sprintf("已达到预览容量上限 (%d MiB)，如需完整数据请缩小查询范围", opts.MaxTotalBytes/(1024*1024))
		return false, nil
	}

	if !b.snap.spilled && (len(b.snap.memRows) >= opts.MaxMemRows || b.snap.totalBytes+rowBytes > opts.MaxMemBytes) {
		if err := b.spillToDisk(); err != nil {
			return false, err
		}
	}

	if b.snap.spilled {
		if err := b.writeSpillRow(row); err != nil {
			return false, err
		}
	} else {
		b.snap.memRows = append(b.snap.memRows, row)
	}

	b.snap.totalRows++
	b.snap.totalBytes += rowBytes
	return true, nil
}

func (b *Builder) spillToDisk() error {
	if err := os.MkdirAll(b.mgr.opts.SpillDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(b.mgr.opts.SpillDir, b.snap.id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	b.spillFile = f
	b.spillBuf = bufio.NewWriterSize(f, 64*1024)
	b.snap.spilled = true
	b.snap.spillPath = path
	b.snap.rowOffsets = make([]int64, 0, len(b.snap.memRows)+256)

	for _, r := range b.snap.memRows {
		if err := b.writeSpillRow(r); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
	}
	b.snap.memRows = nil
	return nil
}

func (b *Builder) writeSpillRow(row []dto.CellValue) error {
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	b.snap.rowOffsets = append(b.snap.rowOffsets, b.spillPos)
	if _, err := b.spillBuf.Write(raw); err != nil {
		return err
	}
	if err := b.spillBuf.WriteByte('\n'); err != nil {
		return err
	}
	b.spillPos += int64(len(raw)) + 1
	return nil
}

// Finalize seals the snapshot, registers it in Manager, enforces global LRU quota, and returns its ResultID.
func (b *Builder) Finalize() (string, SnapshotMeta, error) {
	if b.spillBuf != nil {
		if err := b.spillBuf.Flush(); err != nil {
			b.Abort()
			return "", SnapshotMeta{}, err
		}
	}
	if b.spillFile != nil {
		if err := b.spillFile.Close(); err != nil {
			b.Abort()
			return "", SnapshotMeta{}, err
		}
		b.spillFile = nil
		b.spillBuf = nil
	}

	m := b.mgr
	m.mu.Lock()
	defer m.mu.Unlock()

	m.evictExpiredLocked()
	m.snapshots[b.snap.id] = b.snap
	m.globalBytes += b.snap.totalBytes
	m.evictOverQuotaLocked(b.snap.id)

	meta := SnapshotMeta{
		ResultID:         b.snap.id,
		TotalBuffered:    b.snap.totalRows,
		TruncatedByLimit: b.snap.truncatedByLimit,
		LimitReason:      b.snap.limitReason,
		SpilledToDisk:    b.snap.spilled,
	}
	return b.snap.id, meta, nil
}

// Abort discards an unfinished snapshot and deletes any temp spill file or registered blobs.
func (b *Builder) Abort() {
	if b.spillFile != nil {
		_ = b.spillFile.Close()
		b.spillFile = nil
		b.spillBuf = nil
	}
	if b.snap.spillPath != "" {
		_ = os.Remove(b.snap.spillPath)
	}
	b.mgr.mu.Lock()
	defer b.mgr.mu.Unlock()
	for _, ref := range b.snap.streamRefs {
		delete(b.mgr.blobs, ref)
	}
}

// SnapshotMeta summarizes a finalized Execute-Once snapshot.
type SnapshotMeta struct {
	ResultID         string
	TotalBuffered    int64
	TruncatedByLimit bool
	LimitReason      string
	SpilledToDisk    bool
}

// FetchPage reads a slice from an existing snapshot WITHOUT touching any database.
// Returns ERR_RESULT_EXPIRED if the snapshot has expired or been released (never re-runs SQL).
func (m *Manager) FetchPage(resultID string, page, pageSize int) (*dto.FetchResultPageResponse, *dto.AppError) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 200
	}
	if pageSize > 5000 {
		pageSize = 5000
	}

	m.mu.Lock()
	m.evictExpiredLocked()
	snap, ok := m.snapshots[resultID]
	if !ok {
		m.mu.Unlock()
		return nil, dto.NewAppError(
			dto.ErrResultExpired,
			"查询结果快照已过期或已释放，请重新执行 SQL",
			"snapshot not found: "+resultID,
		)
	}
	snap.lastAccessed = m.nowFn()
	// Copy immutable header metadata before releasing lock for file reads.
	cols := snap.columns
	totalRows := snap.totalRows
	truncated := snap.truncatedByLimit
	limitReason := snap.limitReason
	spilled := snap.spilled
	spillPath := snap.spillPath

	startIdx := int64(page-1) * int64(pageSize)
	endIdx := startIdx + int64(pageSize)
	if startIdx > totalRows {
		startIdx = totalRows
	}
	if endIdx > totalRows {
		endIdx = totalRows
	}

	var rows [][]dto.CellValue
	var offsets []int64
	if !spilled {
		rows = make([][]dto.CellValue, 0, endIdx-startIdx)
		for i := startIdx; i < endIdx; i++ {
			rows = append(rows, snap.memRows[i])
		}
		m.mu.Unlock()
	} else {
		if startIdx < endIdx {
			offsets = make([]int64, endIdx-startIdx)
			copy(offsets, snap.rowOffsets[startIdx:endIdx])
		}
		m.mu.Unlock()

		var err error
		rows, err = readSpillSlice(spillPath, offsets)
		if err != nil {
			return nil, dto.NewAppError(dto.ErrResultExpired, "读取本地结果快照失败，请重新执行 SQL", err.Error())
		}
	}

	return &dto.FetchResultPageResponse{
		ResultID:         resultID,
		Columns:          cols,
		Rows:             rows,
		Page:             page,
		PageSize:         pageSize,
		TotalBuffered:    totalRows,
		HasMorePages:     endIdx < totalRows,
		TruncatedByLimit: truncated,
		LimitReason:      limitReason,
	}, nil
}

func readSpillSlice(path string, offsets []int64) ([][]dto.CellValue, error) {
	if len(offsets) == 0 {
		return [][]dto.CellValue{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(offsets[0], 0); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	rows := make([][]dto.CellValue, 0, len(offsets))
	for i := 0; i < len(offsets); i++ {
		line, err := reader.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err
		}
		var r []dto.CellValue
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// GetBlob retrieves a registered cell binary/large-text blob by stream ref.
func (m *Manager) GetBlob(ref string) (*StreamBlob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[ref]
	return b, ok
}

// ReleaseByTab removes all snapshots and spill files owned by a tab (called on new query or tab close).
func (m *Manager) ReleaseByTab(tabID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, snap := range m.snapshots {
		if snap.tabID == tabID {
			m.deleteSnapshotLocked(id)
			removed++
		}
	}
	return removed
}

// ReleaseByConnection removes all snapshots belonging to a connection.
func (m *Manager) ReleaseByConnection(connID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, snap := range m.snapshots {
		if snap.connID == connID {
			m.deleteSnapshotLocked(id)
			removed++
		}
	}
	return removed
}

// Release removes a single snapshot immediately.
func (m *Manager) Release(resultID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteSnapshotLocked(resultID)
}

// CloseAll removes every snapshot and cleans up all temp spill files.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.snapshots {
		m.deleteSnapshotLocked(id)
	}
}

// ActiveCount returns the number of currently held snapshots (useful for verification & diagnostics).
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictExpiredLocked()
	return len(m.snapshots)
}

// ActiveCountForTab returns the number of snapshots currently held for one tab.
func (m *Manager) ActiveCountForTab(tabID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictExpiredLocked()
	count := 0
	for _, s := range m.snapshots {
		if s.tabID == tabID {
			count++
		}
	}
	return count
}

func (m *Manager) evictExpiredLocked() {
	now := m.nowFn()
	for id, snap := range m.snapshots {
		if now.Sub(snap.lastAccessed) > m.opts.IdleTTL {
			m.deleteSnapshotLocked(id)
		}
	}
}

func (m *Manager) evictOverQuotaLocked(keepID string) {
	for m.globalBytes > m.opts.MaxGlobalBytes && len(m.snapshots) > 1 {
		var oldestID string
		var oldestTime time.Time
		for id, snap := range m.snapshots {
			if id == keepID {
				continue
			}
			if oldestID == "" || snap.lastAccessed.Before(oldestTime) {
				oldestID = id
				oldestTime = snap.lastAccessed
			}
		}
		if oldestID == "" {
			break
		}
		m.deleteSnapshotLocked(oldestID)
	}
}

func (m *Manager) deleteSnapshotLocked(id string) {
	snap, ok := m.snapshots[id]
	if !ok {
		return
	}
	if snap.spillPath != "" {
		_ = os.Remove(snap.spillPath)
	}
	for _, ref := range snap.streamRefs {
		delete(m.blobs, ref)
	}
	m.globalBytes -= snap.totalBytes
	if m.globalBytes < 0 {
		m.globalBytes = 0
	}
	delete(m.snapshots, id)
}

func estimateRowBytes(row []dto.CellValue) int64 {
	var total int64 = 32
	for _, c := range row {
		total += int64(len(c.Text)) + int64(len(c.Kind)) + 24
	}
	return total
}
