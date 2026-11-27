import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, nextTick } from 'vue'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { api } from '@/api/client'
import { EngineType, SessionState, ValueKind } from '@/api/types'
import CommandPalette from '@/components/CommandPalette.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import ConnectionModal from '@/components/ConnectionModal.vue'
import SettingsModal from '@/components/SettingsModal.vue'
import { useShortcuts } from '@/composables/useShortcuts'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { connectionEndpointSummary, engineLabel } from '@/utils/format'
import { makeConnectResult, makeQueryTab } from './testHelpers'

enableAutoUnmount(afterEach)

describe('Global Esc modal priority & focus trap (useShortcuts / BaseModal)', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('closes modals in z-order on Escape and NEVER cancels a running query while any modal is open', async () => {
    const uiStore = useUiStore()
    const wsStore = useWorkspaceStore()

    const cancelSpy = vi.spyOn(api, 'cancelExecution').mockResolvedValue({
      tabId: 'q-1',
      execId: 'exec-1',
      status: 'canceled',
      sessionState: SessionState.SessionAliveIdle,
      message: '已取消',
    })

    wsStore.tabs.push(
      makeQueryTab({
        id: 'q-1',
        sql: 'SELECT pg_sleep(30);',
        running: true,
        activeExecId: 'exec-1',
        startedAt: Date.now(),
      }),
    )
    wsStore.activeTabId = 'q-1'

    const Host = defineComponent({
      setup() {
        useShortcuts()
        return () => null
      },
    })
    mount(Host)

    // Open multiple modals at once: TableDdl + Connection + Settings + CommandPalette + ValueInspector + ConfirmDialog
    uiStore.openTableDdl({ connectionId: 'conn-1', database: 'app_db', schema: 'public', table: 'users' })
    uiStore.openNewConnectionModal()
    uiStore.openSettingsModal()
    uiStore.openCommandPalette()
    uiStore.inspectCell(
      { name: 'id', databaseType: 'INT', kind: ValueKind.KindInt, nullable: false },
      { kind: ValueKind.KindInt, text: '1' },
      0,
    )
    const confirmPromise = uiStore.requestConfirm({ title: '确认', message: '继续？' })

    // Expected top-to-bottom close order across the 6 stacked modal layers
    const modalLayers: Array<{ name: string; isOpen: () => boolean }> = [
      { name: 'ConfirmDialog', isOpen: () => uiStore.confirmDialog !== null },
      { name: 'ValueInspector', isOpen: () => uiStore.inspectedCell !== null },
      { name: 'CommandPalette', isOpen: () => uiStore.commandPaletteOpen },
      { name: 'SettingsModal', isOpen: () => uiStore.settingsModalOpen },
      { name: 'ConnectionModal', isOpen: () => uiStore.connectionModalOpen },
      { name: 'TableDdlModal', isOpen: () => uiStore.ddlTarget !== null },
    ]

    for (let i = 0; i < modalLayers.length; i++) {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
      if (i === 0) {
        expect(await confirmPromise).toBe(false)
      }
      expect(modalLayers[i].isOpen()).toBe(false)
      if (i + 1 < modalLayers.length) {
        expect(modalLayers[i + 1].isOpen()).toBe(true)
      }
      expect(cancelSpy).not.toHaveBeenCalled()
    }
    expect(uiStore.hasOpenModal).toBe(false)

    // 7th Esc (now zero modals are open) cancels the running query!
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))
    expect(cancelSpy).toHaveBeenCalledTimes(1)
    expect(cancelSpy).toHaveBeenCalledWith('q-1', 'exec-1')
  })

  it('closes CommandPalette when Escape fires on its inner search input without bubbling into query cancel', async () => {
    const uiStore = useUiStore()
    const wsStore = useWorkspaceStore()
    const cancelSpy = vi.spyOn(api, 'cancelExecution').mockResolvedValue({
      tabId: 'q-1',
      execId: 'exec-1',
      status: 'canceled',
      sessionState: SessionState.SessionAliveIdle,
      message: '已取消',
    })

    wsStore.tabs.push(makeQueryTab({ id: 'q-1', running: true, activeExecId: 'exec-1', startedAt: Date.now() }))
    wsStore.activeTabId = 'q-1'

    const Host = defineComponent({
      components: { CommandPalette },
      setup() {
        useShortcuts()
        return {}
      },
      template: '<CommandPalette />',
    })
    const wrapper = mount(Host, { attachTo: document.body })

    uiStore.openCommandPalette()
    await nextTick()
    await nextTick()
    const input = wrapper.find('input')
    expect(input.exists()).toBe(true)
    expect(document.activeElement).toBe(input.element)

    // Dispatch Escape from inside the CommandPalette input with bubbles: true
    input.element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }))
    await nextTick()

    expect(uiStore.commandPaletteOpen).toBe(false)
    expect(cancelSpy).not.toHaveBeenCalled()
  })

  it('traps Tab focus inside ConfirmDialog, focuses Cancel by default, and restores previous focus on close', async () => {
    const uiStore = useUiStore()
    const outsideBtn = document.createElement('button')
    outsideBtn.textContent = 'Editor Focus Proxy'
    document.body.appendChild(outsideBtn)
    try {
      outsideBtn.focus()
      expect(document.activeElement).toBe(outsideBtn)

      const wrapper = mount(ConfirmDialog, { attachTo: document.body })

      const confirmPromise = uiStore.requestConfirm({
        title: '生产确认',
        message: '确认执行？',
        tone: 'danger',
        confirmLabel: '确认执行',
        cancelLabel: '取消执行',
      })
      await nextTick()
      await nextTick()

      const cancelBtn = wrapper.find('[data-testid="confirm-dialog-cancel-btn"]').element as HTMLButtonElement
      const confirmBtn = wrapper.find('[data-testid="confirm-dialog-confirm-btn"]').element as HTMLButtonElement
      // Cancel button MUST receive initial focus so accidental Enter never confirms a destructive SQL
      expect(document.activeElement).toBe(cancelBtn)

      // Tabbing past the last focusable button wraps around to the first button inside modal (never leaks out)
      const dialog = wrapper.find('[role="dialog"]').element as HTMLElement
      confirmBtn.focus()
      dialog.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }))
      expect(dialog.contains(document.activeElement)).toBe(true)
      expect(document.activeElement).not.toBe(outsideBtn)

      // Closing dialog restores focus to the previously focused element
      uiStore.resolveConfirm(false)
      expect(await confirmPromise).toBe(false)
      await nextTick()
      await nextTick()
      expect(document.activeElement).toBe(outsideBtn)
    } finally {
      outsideBtn.remove()
    }
  })
})

describe('4-engine workbench & settings modals (MySQL, PostgreSQL, MariaDB, SQLite)', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('formats engine labels and accurate default ports for PostgreSQL (5432), MySQL/MariaDB (3306) and SQLite', () => {
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
        driver: EngineType.EnginePostgres,
        params: { host: 'db.internal' },
      }),
    ).toBe('db.internal:5432')

    expect(
      connectionEndpointSummary({
        driver: EngineType.EngineMariaDB,
        params: { host: '127.0.0.1', port: 13307 },
      }),
    ).toBe('127.0.0.1:13307')
  })

  it('switches ConnectionModal cleanly between network engines (MariaDB/MySQL/PG) and SQLite file mode', async () => {
    const uiStore = useUiStore()
    uiStore.openNewConnectionModal()

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

    // Switch to PostgreSQL: shows optional errorCharset and trims it in requests
    await buttons.find((b) => b.text().trim() === 'PostgreSQL')!.trigger('click')
    await wrapper.find('input[placeholder*="GBK"]').setValue(' GBK ')
    const testSpy = vi.spyOn(api, 'testConnection').mockResolvedValue({
      success: true,
      detectedType: EngineType.EnginePostgres,
      serverVersion: '16.16',
      latencyMs: 3,
      capabilities: makeConnectResult().capabilities,
    })
    const testBtn = wrapper.findAll('button').find((b) => b.text().trim() === '测试连接')!
    await testBtn.trigger('click')
    expect(testSpy.mock.calls[0][0].params.errorCharset).toBe('GBK')

    // Switch to MariaDB: restores host/port/TLS and hides/omits PG-only errorCharset
    await buttons.find((b) => b.text().trim() === 'MariaDB')!.trigger('click')

    expect(wrapper.text()).toContain('主机地址 (Host)')
    expect(wrapper.text()).toContain('TLS 加密模式')
    expect(wrapper.text()).toContain('客户端只读保护')
    expect(wrapper.text()).not.toContain('连接报错编码 (可选)')

    await testBtn.trigger('click')
    expect(testSpy.mock.calls[1][0].params.errorCharset).toBeUndefined()
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

    const settings = useSettingsStore()
    const uiStore = useUiStore()
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
    uiStore.openSettingsModal()
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
  })
})
