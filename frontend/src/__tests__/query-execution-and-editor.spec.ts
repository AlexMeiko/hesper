import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { EditorView } from '@codemirror/view'
import { api } from '@/api/client'
import { EngineType, ErrorCode, SessionState } from '@/api/types'
import App from '@/App.vue'
import SqlEditor from '@/components/SqlEditor.vue'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore, type QueryTab } from '@/stores/workspace'
import { makeConn, makeConnectResult, makeQueryTab } from './testHelpers'

describe('Query execution safeguards, editor UTF-8 offset & tab lifecycle', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('handles backend requiresConfirm with confirmToken binding and tracks dirty state via executedAll', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()
    const uiStore = useUiStore()

    connStore.connections = [makeConn({ id: 'prod-1', name: '生产主库', envColor: 'red' })]
    connStore.activeMap['prod-1'] = makeConnectResult({ connectionId: 'prod-1' })

    const execSpy = vi.spyOn(api, 'executeSQL').mockImplementation(async (req) => {
      if (req.runMode === 'current' && req.sql.includes(';')) {
        return {
          tabId: req.tabId,
          execId: req.execId,
          sessionState: SessionState.SessionAliveIdle,
          currentDb: 'app_db',
          totalMs: 2,
          statements: [],
          executedAll: false,
        }
      }
      if (req.confirmToken !== 'tok-delete-orders') {
        return {
          tabId: req.tabId,
          execId: req.execId,
          sessionState: SessionState.SessionAliveIdle,
          currentDb: 'app_db',
          totalMs: 0,
          statements: [],
          requiresConfirm: true,
          confirmToken: 'tok-delete-orders',
          highRiskStatements: [
            { kind: 'delete_no_where', label: '无 WHERE 条件的 DELETE（全表删除）', sql: 'DELETE FROM orders' },
          ],
        }
      }
      return {
        tabId: req.tabId,
        execId: req.execId,
        sessionState: SessionState.SessionAliveIdle,
        currentDb: 'app_db',
        totalMs: 6,
        statements: [],
        executedAll: true,
      }
    })

    wsStore.tabs.push(
      makeQueryTab({
        id: 'q-prod',
        connectionId: 'prod-1',
        sql: 'SELECT 1; DELETE FROM orders;',
        cleanSql: '',
      }),
    )

    // 0. Running only one statement in a multi-statement buffer (executedAll=false) MUST keep tab dirty
    await wsStore.executeQuery('q-prod', { runMode: 'current', cursorOffset: 0 })
    expect(wsStore.isQueryTabDirty(wsStore.tabs[0] as QueryTab)).toBe(true)

    // 1. User cancels confirmation -> backend is called without confirmToken and NOT re-sent
    const run1 = wsStore.executeQuery('q-prod', { runMode: 'all', cursorOffset: 0 })
    await flushPromises()
    expect(uiStore.confirmDialog).not.toBeNull()
    expect(uiStore.confirmDialog?.title).toContain('生产环境')
    expect(uiStore.confirmDialog?.bullets?.[0]).toContain('DELETE')
    expect((wsStore.tabs[0] as QueryTab).running).toBe(false)
    uiStore.resolveConfirm(false)
    await run1
    expect(execSpy).toHaveBeenCalledTimes(2)
    expect(execSpy.mock.calls[1][0].confirmToken).toBeUndefined()

    // 2. User confirms -> backend is re-called with exact backend-issued confirmToken and cleanSql updates (executedAll=true)
    const run2 = wsStore.executeQuery('q-prod', { runMode: 'all', cursorOffset: 0 })
    await flushPromises()
    expect(uiStore.confirmDialog).not.toBeNull()
    uiStore.resolveConfirm(true)
    await run2
    expect(execSpy).toHaveBeenCalledTimes(4)
    expect(execSpy.mock.calls[3][0].confirmToken).toBe('tok-delete-orders')
    expect(wsStore.isQueryTabDirty(wsStore.tabs[0] as QueryTab)).toBe(false)
  })

  it('reloads completion catalog automatically when SQL execution switches database (e.g. USE other_db)', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()

    connStore.connections = [makeConn({ id: 'mysql-1', driver: EngineType.EngineMySQL, envColor: 'green' })]
    connStore.activeMap['mysql-1'] = makeConnectResult({
      connectionId: 'mysql-1',
      detectedType: EngineType.EngineMySQL,
      currentDb: 'db_a',
      capabilities: { ...makeConnectResult().capabilities, hasSchemas: false },
    })

    vi.spyOn(api, 'executeSQL').mockResolvedValue({
      tabId: 'q-use',
      execId: 'exec-1',
      sessionState: SessionState.SessionAliveIdle,
      currentDb: 'db_b',
      totalMs: 3,
      statements: [],
      executedAll: true,
    })
    const catSpy = vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue({
      database: 'db_b',
      tables: { orders: ['id', 'total'] },
    })

    wsStore.tabs.push(
      makeQueryTab({
        id: 'q-use',
        connectionId: 'mysql-1',
        database: 'db_a',
        schema: '',
        sql: 'USE db_b;',
      }),
    )

    await wsStore.executeQuery('q-use', { runMode: 'all', cursorOffset: 0 })
    await flushPromises()
    expect(wsStore.tabs[0].database).toBe('db_b')
    expect(catSpy).toHaveBeenCalledWith('mysql-1', 'db_b', '')
  })

  it('converts CodeMirror UTF-16 cursor head to UTF-8 byte offset when SQL contains CJK characters', async () => {
    const prefix = '-- 中文注释\nSELECT 1;'
    const tail = 'DELETE FROM t'
    const doc = prefix + tail
    const wrapper = mount(SqlEditor, {
      props: {
        modelValue: doc,
        dialect: EngineType.EnginePostgres,
        theme: 'light',
      },
      attachTo: document.body,
    })
    try {
      await nextTick()

      const exposed = wrapper.vm as unknown as {
        getExecutionContext: () => { sqlOverride?: string; cursorOffset: number; runMode: 'current' | 'selection' }
      }
      // Initial cursor at 0 -> 0 UTF-8 bytes
      expect(exposed.getExecutionContext()).toEqual({ runMode: 'current', cursorOffset: 0 })

      // Move real CodeMirror selection right after "-- 中文注释\nSELECT 1;" (UTF-16 offset 17 -> UTF-8 byte 25)
      const cmEl = wrapper.find('.cm-editor').element as HTMLElement
      const view = EditorView.findFromDOM(cmEl)
      expect(view).not.toBeNull()

      view!.dispatch({ selection: { anchor: prefix.length } })
      expect(prefix.length).toBe(17)
      expect(exposed.getExecutionContext()).toEqual({ runMode: 'current', cursorOffset: 25 })

      // Non-empty selection returns selected SQL directly in selection mode
      view!.dispatch({ selection: { anchor: prefix.length, head: doc.length } })
      expect(exposed.getExecutionContext()).toEqual({
        runMode: 'selection',
        sqlOverride: tail,
        cursorOffset: 0,
      })
    } finally {
      wrapper.unmount()
    }
  })

  it('syncs fallback schema with backend switchTabSchema instead of guessing locally when openTabSession returns empty schema', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()

    connStore.connections = [makeConn({ id: 'pg-1' })]
    connStore.activeMap['pg-1'] = makeConnectResult({ connectionId: 'pg-1' })

    vi.spyOn(api, 'openTabSession').mockResolvedValue({
      tabId: 't1',
      connectionId: 'pg-1',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      serverConnId: '12',
      readOnly: false,
    })
    vi.spyOn(api, 'listSchemas').mockResolvedValue(['analytics', 'public'])
    const switchSchemaSpy = vi.spyOn(api, 'switchTabSchema').mockResolvedValue({
      tabId: 't1',
      connectionId: 'pg-1',
      currentDb: 'app_db',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      serverConnId: '12',
      readOnly: false,
    })
    vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue({ database: 'app_db', schema: 'public', tables: {} })

    const tab = await wsStore.openQueryTab('pg-1', 'app_db')
    expect(tab?.schema).toBe('public')
    expect(switchSchemaSpy).toHaveBeenCalledTimes(1)
    expect(switchSchemaSpy).toHaveBeenCalledWith(tab!.id, 'public')
  })

  it('handles forceAbortQuery rejection cleanly without leaving cancelPending stuck', async () => {
    const wsStore = useWorkspaceStore()
    vi.spyOn(api, 'forceAbortSession').mockRejectedValue({
      code: ErrorCode.ErrInternal,
      message: '底层句柄已关闭',
      retrySafe: false,
    })

    wsStore.tabs.push(
      makeQueryTab({
        id: 'q-abort',
        sql: 'SELECT 1;',
        running: true,
        activeExecId: 'exec-9',
        cancelPending: true,
        startedAt: Date.now(),
      }),
    )

    await wsStore.forceAbortQuery('q-abort')
    const tab = wsStore.tabs[0]
    expect(tab.kind === 'query' && tab.cancelPending).toBe(false)
    expect(tab.kind === 'query' && tab.bannerError?.message).toBe('底层句柄已关闭')
  })

  it('preserves mounted tab views via v-show and only confirms closing modified/running query tabs (not untouched templates)', async () => {
    vi.spyOn(api, 'listConnections').mockResolvedValue([])
    vi.spyOn(api, 'closeTab').mockResolvedValue()

    const wsStore = useWorkspaceStore()
    const uiStore = useUiStore()

    wsStore.tabs.push(
      // Modified SQL (differs from cleanSql) -> dirty!
      makeQueryTab({ id: 'q-dirty', title: '查询 A', sql: 'SELECT 2;', cleanSql: 'SELECT 1;' }),
      // Untouched template SQL (equals cleanSql) -> clean!
      makeQueryTab({ id: 'q-template', title: '查询 users', sql: 'SELECT *\nFROM users\nLIMIT 100;\n', cleanSql: 'SELECT *\nFROM users\nLIMIT 100;\n' }),
    )
    wsStore.activeTabId = 'q-dirty'

    const wrapper = mount(App, {
      global: {
        stubs: {
          SqlEditor: true,
          ExplorerSidebar: true,
        },
      },
    })

    const queryViews = wrapper.findAllComponents({ name: 'QueryTabView' })
    expect(queryViews.length).toBe(2)

    // Switch active tab: BOTH instances stay mounted (zero state destruction!)
    wsStore.setActiveTab('q-template')
    await nextTick()
    expect(wrapper.findAllComponents({ name: 'QueryTabView' }).length).toBe(2)

    // Closing modified tab prompts confirmation
    const closePromise = wsStore.closeTab('q-dirty', { confirmDirty: true })
    expect(uiStore.confirmDialog).not.toBeNull()
    uiStore.resolveConfirm(false)
    await closePromise
    expect(wsStore.tabs.length).toBe(2)

    // Closing untouched template tab closes immediately WITHOUT nagging confirmation
    await wsStore.closeTab('q-template', { confirmDirty: true })
    expect(uiStore.confirmDialog).toBeNull()
    expect(wsStore.tabs.length).toBe(1)

    wrapper.unmount()
  })
})
