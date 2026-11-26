import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { EngineType, ErrorCode, SessionState, ValueKind, type CellValue, type ColumnMeta } from '@/api/client'
import ConnectionModal from '@/components/ConnectionModal.vue'
import DataGrid from '@/components/DataGrid.vue'
import SessionBanner from '@/components/SessionBanner.vue'
import SettingsModal from '@/components/SettingsModal.vue'
import ValueInspector from '@/components/ValueInspector.vue'
import { useConnectionsStore } from '@/stores/connections'
import { useSettingsStore } from '@/stores/settings'
import {
  cellToPlainText,
  connectionEndpointSummary,
  engineLabel,
  estimateColumnWidth,
  rowsToJSON,
  tokenizeWhitespace,
} from '@/utils/format'

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

describe('4-engine workbench support (MySQL, PostgreSQL, MariaDB, SQLite)', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('formats engine labels and SQLite vs network connection endpoints cleanly', () => {
    expect(engineLabel(EngineType.EngineMySQL)).toBe('MySQL')
    expect(engineLabel(EngineType.EnginePostgres)).toBe('PostgreSQL')
    expect(engineLabel(EngineType.EngineMariaDB)).toBe('MariaDB')
    expect(engineLabel(EngineType.EngineSQLite)).toBe('SQLite')

    expect(
      connectionEndpointSummary({
        driver: EngineType.EngineSQLite,
        params: { filePath: '/data/projects/billing.sqlite3' },
      }),
    ).toBe('billing.sqlite3')

    expect(
      connectionEndpointSummary({
        driver: EngineType.EngineMariaDB,
        params: { host: '127.0.0.1', port: 13307 },
      }),
    ).toBe('127.0.0.1:13307')
  })

  it('switches ConnectionModal cleanly between network engines (MariaDB/MySQL/PG) and SQLite file mode', async () => {
    const connStore = useConnectionsStore()
    connStore.openNewConnectionModal()

    const wrapper = mount(ConnectionModal)
    const buttons = wrapper.findAll('button')
    const btnTexts = buttons.map((b) => b.text().trim())
    expect(btnTexts).toContain('MySQL')
    expect(btnTexts).toContain('PostgreSQL')
    expect(btnTexts).toContain('MariaDB')
    expect(btnTexts).toContain('SQLite')

    // Switch to SQLite: hides host/port/password and shows local file picker + engine mode=ro copy
    const sqliteBtn = buttons.find((b) => b.text().trim() === 'SQLite')
    expect(sqliteBtn).toBeDefined()
    await sqliteBtn!.trigger('click')

    expect(wrapper.text()).toContain('SQLite 数据库文件路径')
    expect(wrapper.text()).toContain('选择文件...')
    expect(wrapper.text()).toContain('mode=ro')
    expect(wrapper.text()).not.toContain('主机地址 (Host)')
    expect(wrapper.text()).not.toContain('TLS 加密模式')

    // Switch to MariaDB: restores host/port/TLS and defaults back to port 3306 / root
    const mariaBtn = buttons.find((b) => b.text().trim() === 'MariaDB')
    expect(mariaBtn).toBeDefined()
    await mariaBtn!.trigger('click')

    expect(wrapper.text()).toContain('主机地址 (Host)')
    expect(wrapper.text()).toContain('TLS 加密模式')
    expect(wrapper.text()).toContain('客户端只读保护')
  })

  it('manages theme (system/light/dark) and fontScale via SettingsModal with persistence', async () => {
    const mediaChangeListeners: Array<(ev: MediaQueryListEvent) => void> = []
    vi.stubGlobal(
      'matchMedia',
      vi.fn().mockImplementation((query: string) => ({
        matches: true, // OS prefers dark
        media: query,
        addEventListener: (_: string, cb: (ev: MediaQueryListEvent) => void) => {
          mediaChangeListeners.push(cb)
        },
        removeEventListener: vi.fn(),
      })),
    )

    try {
      const settings = useSettingsStore()
      // Defaults: follow system (dark here) + standard font scale
      expect(settings.themePreference).toBe('system')
      expect(settings.theme).toBe('dark')
      expect(document.documentElement.getAttribute('data-theme')).toBe('dark')
      expect(settings.fontScale).toBe('standard')
      expect(document.documentElement.getAttribute('data-font-scale')).toBe('standard')

      // Simulate OS switching to light while in system mode
      expect(mediaChangeListeners.length).toBeGreaterThan(0)
      mediaChangeListeners.forEach((cb) => cb({ matches: false } as MediaQueryListEvent))
      await Promise.resolve()
      expect(settings.theme).toBe('light')
      expect(document.documentElement.getAttribute('data-theme')).toBe('light')

      // Open SettingsModal and interact with controls
      settings.openSettingsModal()
      const wrapper = mount(SettingsModal)
      expect(wrapper.find('[data-testid="settings-modal"]').exists()).toBe(true)
      expect(wrapper.text()).toContain('跟随系统')
      expect(wrapper.text()).toContain('亮色')
      expect(wrapper.text()).toContain('暗色')

      await wrapper.find('[data-testid="theme-option-dark"]').trigger('click')
      expect(settings.themePreference).toBe('dark')
      expect(settings.theme).toBe('dark')
      expect(localStorage.getItem('sqlm:theme')).toBe('dark')
      expect(document.documentElement.getAttribute('data-theme')).toBe('dark')

      await wrapper.find('[data-testid="theme-option-light"]').trigger('click')
      expect(settings.themePreference).toBe('light')
      expect(settings.theme).toBe('light')
      expect(localStorage.getItem('sqlm:theme')).toBe('light')

      await wrapper.find('[data-testid="font-scale-option-comfortable"]').trigger('click')
      expect(settings.fontScale).toBe('comfortable')
      expect(localStorage.getItem('sqlm:fontScale')).toBe('comfortable')
      expect(document.documentElement.getAttribute('data-font-scale')).toBe('comfortable')

      await wrapper.find('[data-testid="font-scale-option-compact"]').trigger('click')
      expect(settings.fontScale).toBe('compact')
      expect(localStorage.getItem('sqlm:fontScale')).toBe('compact')
      expect(document.documentElement.getAttribute('data-font-scale')).toBe('compact')
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
