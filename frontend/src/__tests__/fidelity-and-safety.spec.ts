import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { EngineType, ErrorCode, SessionState, ValueKind, type CellValue, type ColumnMeta } from '@/api/types'
import DataGrid from '@/components/DataGrid.vue'
import SessionBanner from '@/components/SessionBanner.vue'
import ValueInspector from '@/components/ValueInspector.vue'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import {
  cellToPlainText,
  estimateColumnWidth,
  formatExportCaveat,
  inspectExportFidelity,
  rowsToInsertSQL,
  rowsToJSON,
  tokenizeWhitespace,
} from '@/utils/format'
import { buildQuickSelectSQL } from '@/utils/tableMeta'
import { mockVirtualViewport } from './testHelpers'

describe('DataGrid fidelity, virtualization, scroll & export (Criterion 3)', () => {
  let restoreViewport: (() => void) | null = null

  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    restoreViewport = mockVirtualViewport(1000, 480)
  })

  afterEach(() => {
    restoreViewport?.()
    restoreViewport = null
    vi.restoreAllMocks()
  })

  const columns: ColumnMeta[] = [
    { name: 'id', databaseType: 'BIGINT UNSIGNED', kind: ValueKind.KindBigInt, nullable: false, isPrimaryKey: true },
    { name: 'signed_big', databaseType: 'BIGINT', kind: ValueKind.KindBigInt, nullable: false },
    { name: 'amount', databaseType: 'DECIMAL(38,10)', kind: ValueKind.KindDecimal, nullable: false },
    { name: 'note', databaseType: 'VARCHAR(100)', kind: ValueKind.KindString, nullable: true },
  ]

  const rows: CellValue[][] = [
    [
      { kind: ValueKind.KindBigInt, text: '18446744073709551615' },
      { kind: ValueKind.KindBigInt, text: '-9223372036854775808' },
      { kind: ValueKind.KindDecimal, text: '1234567890123456789012345678.1234567890' },
      { kind: ValueKind.KindNull },
    ],
    [
      { kind: ValueKind.KindBigInt, text: '9007199254740993' },
      { kind: ValueKind.KindBigInt, text: '9007199254740995' },
      { kind: ValueKind.KindDecimal, text: '-0.0000000001' },
      { kind: ValueKind.KindString, text: '' },
    ],
    [
      { kind: ValueKind.KindBigInt, text: '1' },
      { kind: ValueKind.KindBigInt, text: '2' },
      { kind: ValueKind.KindDecimal, text: '10.00' },
      { kind: ValueKind.KindString, text: '   ' },
    ],
    [
      { kind: ValueKind.KindBigInt, text: '3' },
      { kind: ValueKind.KindBigInt, text: '4' },
      { kind: ValueKind.KindDecimal, text: '20.00' },
      { kind: ValueKind.KindString, text: 'a\tb\nc\u00A0\u200B' },
    ],
  ]

  it('preserves exact BIGINT and DECIMAL precision and auto-expands column width beyond 240px for long decimals', () => {
    const decimalWidth = estimateColumnWidth(columns[2], rows, 2)
    expect(decimalWidth).toBeGreaterThanOrEqual(320)
    expect(decimalWidth).toBeLessThanOrEqual(420)

    const bigIntWidth = estimateColumnWidth(columns[0], rows, 0)
    expect(bigIntWidth).toBeGreaterThanOrEqual(175)

    const wrapper = mount(DataGrid, {
      props: { columns, rows },
    })

    const text = wrapper.text()
    expect(text).toContain('18446744073709551615')
    expect(text).toContain('-9223372036854775808')
    expect(text).toContain('9007199254740993')
    expect(text).toContain('1234567890123456789012345678.1234567890')
    expect(text).toContain('-0.0000000001')

    const decimalHeader = wrapper.find('[data-col-idx="2"]')
    expect(decimalHeader.attributes('style')).toContain(`width: ${decimalWidth}px`)
    wrapper.unmount()
  })

  it('distinguishes NULL, empty string "", and whitespace cells cleanly with toggleable invisible characters', async () => {
    const wrapper = mount(DataGrid, {
      props: { columns, rows },
    })

    const nullCells = wrapper.findAll('[data-cell-kind="null"]')
    const emptyCells = wrapper.findAll('[data-cell-kind="empty-string"]')
    expect(nullCells.length).toBe(1)
    expect(nullCells[0].text()).toBe('NULL')
    expect(emptyCells.length).toBe(1)
    expect(emptyCells[0].text()).toBe('""')

    // Default: whitespace visualization toggle is OFF
    expect(wrapper.findAll('[data-ws-token="space"]').length).toBe(0)

    // Toggle invisible whitespace ON
    await wrapper.find('[data-testid="toggle-whitespace-btn"]').trigger('click')
    expect(useSettingsStore().showWhitespace).toBe(true)

    // Now the 3-space string and tab/newline/NBSP/ZWSP tokens are visibly rendered
    expect(wrapper.findAll('[data-ws-token="space"]').length).toBe(3)
    expect(wrapper.findAll('[data-ws-token="tab"]').length).toBe(1)
    expect(wrapper.findAll('[data-ws-token="newline"]').length).toBe(1)
    expect(wrapper.findAll('[data-ws-token="special"]').length).toBe(2)

    // Raw text helpers & JSON export MUST remain 100% untouched (no visual glyphs leaked)
    expect(cellToPlainText(rows[2][3])).toBe('   ')
    expect(cellToPlainText(rows[3][3])).toBe('a\tb\nc\u00A0\u200B')
    wrapper.unmount()
  })

  it('renders visible truncated badge when CellValue.truncated is true and reports honest export caveats', () => {
    const cols: ColumnMeta[] = [
      { name: 'payload', databaseType: 'TEXT', kind: ValueKind.KindString, nullable: true },
      { name: 'blob_col', databaseType: 'BLOB', kind: ValueKind.KindBinary, nullable: true },
    ]
    const sampleRows: CellValue[][] = [
      [
        { kind: ValueKind.KindString, text: 'hello\tworld', truncated: true, streamRef: 'ref-1' },
        { kind: ValueKind.KindBinary, text: 'deadbeef', byteLength: 4096, streamRef: 'ref-2' },
      ],
      [{ kind: ValueKind.KindNull }, { kind: ValueKind.KindString, text: '' }],
    ]

    const wrapper = mount(DataGrid, { props: { columns: cols, rows: sampleRows } })
    const badges = wrapper.findAll('[data-cell-truncated="true"]')
    expect(badges.length).toBe(1)
    expect(badges[0].text()).toBe('截断')

    const summary = inspectExportFidelity(cols, sampleRows)
    expect(summary.truncatedCount).toBe(1)
    expect(summary.binaryPreviewCount).toBe(1)
    expect(summary.hasNullAndEmpty).toBe(true)
    expect(summary.hasTsvNormalizedWhitespace).toBe(true)

    const tsvCaveat = formatExportCaveat(summary, 'tsv')
    expect(tsvCaveat).toContain('截断')
    expect(tsvCaveat).toContain('BLOB')
    expect(tsvCaveat).toContain('制表符/换行')
    expect(tsvCaveat).toContain('NULL 与空串')
    wrapper.unmount()
  })

  it('tokenizes spaces, fullwidth spaces, tabs, CRLF/LF, NBSP and ZWSP accurately', () => {
    const tokens = tokenizeWhitespace(' \u3000\t\r\n\u00A0\u200Bhi', true)
    expect(tokens.map((t) => t.text)).toEqual(['·', '□', '→', '␍↵', '[NBSP]', '[ZWSP]', 'hi'])
    expect(tokens[3].lineBreakAfter).toBe(true)
  })

  it('exports JSON with exact string precision and generates dialect-aware INSERT SQL', () => {
    const parsed = JSON.parse(rowsToJSON(columns, rows)) as Record<string, unknown>[]
    expect(parsed[0].id).toBe('18446744073709551615')
    expect(parsed[0].amount).toBe('1234567890123456789012345678.1234567890')
    expect(parsed[0].note).toBeNull()
    expect(parsed[1].id).toBe('9007199254740993')
    expect(parsed[1].note).toBe('')
    expect(parsed[2].note).toBe('   ')

    const sampleCols: ColumnMeta[] = [
      { name: 'id', databaseType: 'INT', kind: ValueKind.KindInt, nullable: false },
      { name: 'active', databaseType: 'BOOL', kind: ValueKind.KindBool, nullable: false },
      { name: 'note', databaseType: 'TEXT', kind: ValueKind.KindString, nullable: true },
    ]
    const sampleRows: CellValue[][] = [
      [
        { kind: ValueKind.KindInt, text: '42' },
        { kind: ValueKind.KindBool, text: 'true' },
        { kind: ValueKind.KindString, text: "O'Brien" },
      ],
    ]

    // PostgreSQL uses double quotes (never MySQL backticks!)
    const pgSql = rowsToInsertSQL({ schema: 'public', table: 'users' }, sampleCols, sampleRows, EngineType.EnginePostgres)
    expect(pgSql).toBe(`INSERT INTO "public"."users" ("id", "active", "note") VALUES (42, TRUE, 'O''Brien');`)
    expect(pgSql).not.toContain('`')

    // Dotted and whitespace-padded table names (e.g. SQLite "order.items", " orders ") MUST stay intact in SELECT and INSERT!
    const sqliteDotSelect = buildQuickSelectSQL('', 'order.items', EngineType.EngineSQLite)
    expect(sqliteDotSelect).toContain('FROM "order.items"')
    const sqliteDotInsert = rowsToInsertSQL({ schema: '', table: 'order.items' }, sampleCols, sampleRows, EngineType.EngineSQLite)
    expect(sqliteDotInsert).toBe(`INSERT INTO "order.items" ("id", "active", "note") VALUES (42, TRUE, 'O''Brien');`)

    const sqliteSpaceSelect = buildQuickSelectSQL('', ' orders ', EngineType.EngineSQLite)
    expect(sqliteSpaceSelect).toContain('FROM " orders "')
    const sqliteSpaceInsert = rowsToInsertSQL({ schema: '', table: ' orders ' }, sampleCols, sampleRows, EngineType.EngineSQLite)
    expect(sqliteSpaceInsert).toBe(`INSERT INTO " orders " ("id", "active", "note") VALUES (42, TRUE, 'O''Brien');`)

    const pgDotInsert = rowsToInsertSQL({ schema: 'public', table: 'order.items' }, sampleCols, sampleRows, EngineType.EnginePostgres)
    expect(pgDotInsert).toBe(`INSERT INTO "public"."order.items" ("id", "active", "note") VALUES (42, TRUE, 'O''Brien');`)

    // MySQL uses backticks
    const mysqlSql = rowsToInsertSQL({ table: 'users' }, sampleCols, sampleRows, EngineType.EngineMySQL)
    expect(mysqlSql).toBe("INSERT INTO `users` (`id`, `active`, `note`) VALUES (42, TRUE, 'O''Brien');")

    // Missing table name emits explicit placeholder warning comment
    const adhocSql = rowsToInsertSQL(null, sampleCols, sampleRows, EngineType.EnginePostgres)
    expect(adhocSql).toContain('-- 注意：当前结果来自自定义 SQL')
    expect(adhocSql).toContain('INSERT INTO "table_name"')
  })

  it('opens scrollable floating ValueInspector modal via uiStore and shows full multiline text', () => {
    const uiStore = useUiStore()
    const longText = Array.from({ length: 40 }, (_, i) => `Line ${i + 1}: 12345678901234567890`).join('\n')
    uiStore.inspectCell(
      { name: 'bio', databaseType: 'TEXT', kind: ValueKind.KindString, nullable: true },
      { kind: ValueKind.KindString, text: longText },
      0,
    )

    const wrapper = mount(ValueInspector)
    expect(wrapper.find('[data-testid="value-inspector-modal"]').exists()).toBe(true)
    const content = wrapper.find('[data-testid="value-inspector-content"]')
    expect(content.text()).toContain('Line 1: 12345678901234567890')
    expect(content.text()).toContain('Line 40: 12345678901234567890')
    wrapper.unmount()
  })

  it('virtualizes columns in DataGrid for wide tables instead of mounting every column', () => {
    const wideCols: ColumnMeta[] = Array.from({ length: 80 }, (_, i) => ({
      name: `col_${i}`,
      databaseType: 'VARCHAR(64)',
      kind: ValueKind.KindString,
      nullable: true,
    }))
    const wideRows: CellValue[][] = [
      wideCols.map((_, i) => ({ kind: ValueKind.KindString, text: `val_${i}` })),
    ]

    const wrapper = mount(DataGrid, { props: { columns: wideCols, rows: wideRows } })
    const renderedHeaders = wrapper.findAll('[data-col-idx]')
    expect(renderedHeaders.length).toBeGreaterThan(0)
    expect(renderedHeaders.length).toBeLessThan(30)
    wrapper.unmount()
  })

  it('resets saved scroll position when DataGrid receives new results in a background tab, while preserving scroll for unchanged results', async () => {
    const cols: ColumnMeta[] = [
      { name: 'id', databaseType: 'BIGINT', kind: ValueKind.KindInt, nullable: false },
      { name: 'note', databaseType: 'TEXT', kind: ValueKind.KindString, nullable: true },
    ]
    const initialRows: CellValue[][] = Array.from({ length: 40 }, (_, i) => [
      { kind: ValueKind.KindInt, text: String(i + 1) },
      { kind: ValueKind.KindString, text: `row_${i + 1}` },
    ])

    const wrapper = mount(DataGrid, {
      props: { columns: cols, rows: initialRows, active: true },
    })

    const viewport = wrapper.find('.overflow-auto').element as HTMLElement
    viewport.scrollTop = 260
    viewport.scrollLeft = 90
    await wrapper.find('.overflow-auto').trigger('scroll')

    // 1. Deactivate and reactivate with the SAME result set -> restores (260, 90)
    await wrapper.setProps({ active: false })
    viewport.scrollTop = 0
    viewport.scrollLeft = 0
    await wrapper.setProps({ active: true })
    await nextTick()
    expect(viewport.scrollTop).toBe(260)
    expect(viewport.scrollLeft).toBe(90)

    // 2. Deactivate, replace rows in background, then reactivate -> MUST reset to (0, 0) instead of stale (260, 90)
    await wrapper.setProps({ active: false })
    const newRows: CellValue[][] = [
      [
        { kind: ValueKind.KindInt, text: '999' },
        { kind: ValueKind.KindString, text: 'fresh_result' },
      ],
    ]
    await wrapper.setProps({ rows: newRows })
    await wrapper.setProps({ active: true })
    await nextTick()
    expect(viewport.scrollTop).toBe(0)
    expect(viewport.scrollLeft).toBe(0)

    wrapper.unmount()
  })
})

describe('SessionBanner safety semantics (Criteria 1, 2, 3)', () => {
  it('renders ERR_COMMIT_OUTCOME_UNKNOWN with reconnect-after-check button and ZERO retry button', () => {
    const wrapper = mount(SessionBanner, {
      props: {
        sessionState: SessionState.SessionCommitUnknown,
        error: {
          code: ErrorCode.ErrCommitOutcomeUnknown,
          message: '写入或提交请求发出后连接中断，结果未知',
          retrySafe: false,
          sessionState: SessionState.SessionCommitUnknown,
        },
      },
    })

    const banner = wrapper.find('[data-testid="commit-unknown-banner"]')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toContain('结果未知')

    const buttons = wrapper.findAll('button')
    expect(buttons.length).toBe(1)
    expect(buttons[0].attributes('data-testid')).toBe('commit-unknown-reconnect-btn')
    expect(buttons[0].text()).toContain('我已核对')
    expect(buttons[0].text()).not.toContain('重试')
    expect(buttons[0].text()).not.toContain('重新执行')
    wrapper.unmount()
  })

  it('renders expired snapshot notice with explicit manual rerun button (never auto-reruns)', async () => {
    const wrapper = mount(SessionBanner, {
      props: {
        sessionState: SessionState.SessionAliveIdle,
        snapshotExpired: true,
        expiredMessage: '该次查询快照已过期',
      },
    })

    const banner = wrapper.find('[data-testid="snapshot-expired-banner"]')
    expect(banner.exists()).toBe(true)
    await banner.find('button').trigger('click')
    expect(wrapper.emitted('rerun-expired')).toHaveLength(1)
    wrapper.unmount()
  })

  it('renders SessionAliveFailedTx with one-click ROLLBACK action', async () => {
    const wrapper = mount(SessionBanner, {
      props: {
        sessionState: SessionState.SessionAliveFailedTx,
      },
    })

    const banner = wrapper.find('[data-testid="failed-tx-banner"]')
    expect(banner.exists()).toBe(true)
    await banner.find('button').trigger('click')
    expect(wrapper.emitted('rollback')).toHaveLength(1)
    wrapper.unmount()
  })
})
