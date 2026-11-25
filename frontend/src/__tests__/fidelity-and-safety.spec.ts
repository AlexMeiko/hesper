import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { ErrorCode, SessionState, ValueKind, type CellValue, type ColumnMeta } from '@/api/client'
import DataGrid from '@/components/DataGrid.vue'
import SessionBanner from '@/components/SessionBanner.vue'
import ValueInspector from '@/components/ValueInspector.vue'
import { useSettingsStore } from '@/stores/settings'
import { cellToPlainText, estimateColumnWidth, rowsToJSON, tokenizeWhitespace } from '@/utils/format'

describe('DataGrid fidelity, smart auto-width & invisible whitespace (Criterion 3)', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, value: 480 })
    Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, value: 1000 })
  })

  afterEach(() => {
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
  })

  it('tokenizes spaces, fullwidth spaces, tabs, CRLF/LF, NBSP and ZWSP accurately', () => {
    const tokens = tokenizeWhitespace(' \u3000\t\r\n\u00A0\u200Bhi', true)
    expect(tokens.map((t) => t.text)).toEqual(['·', '□', '→', '␍↵', '[NBSP]', '[ZWSP]', 'hi'])
    expect(tokens[3].lineBreakAfter).toBe(true)
  })

  it('exports JSON with exact string precision for BIGINT/DECIMAL and distinct null vs "" vs spaces', () => {
    const parsed = JSON.parse(rowsToJSON(columns, rows)) as Record<string, unknown>[]
    expect(parsed[0].id).toBe('18446744073709551615')
    expect(parsed[0].amount).toBe('1234567890123456789012345678.1234567890')
    expect(parsed[0].note).toBeNull()
    expect(parsed[1].id).toBe('9007199254740993')
    expect(parsed[1].note).toBe('')
    expect(parsed[2].note).toBe('   ')
  })

  it('opens scrollable floating ValueInspector modal on double-click and shows full multiline text', async () => {
    const longText = Array.from({ length: 40 }, (_, i) => `Line ${i + 1}: 12345678901234567890`).join('\n')
    const wrapper = mount(ValueInspector, {
      props: {
        item: {
          column: { name: 'bio', databaseType: 'TEXT', kind: ValueKind.KindString, nullable: true },
          cell: { kind: ValueKind.KindString, text: longText },
          rowIndex: 0,
        },
      },
    })

    expect(wrapper.find('[data-testid="value-inspector-modal"]').exists()).toBe(true)
    const content = wrapper.find('[data-testid="value-inspector-content"]')
    expect(content.text()).toContain('Line 1: 12345678901234567890')
    expect(content.text()).toContain('Line 40: 12345678901234567890')

    // Pressing Escape closes the floating modal
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    expect(wrapper.emitted('close')).toHaveLength(1)
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
  })
})
