import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  api,
  ErrorCode,
  parseAppError,
  SessionState,
  type AppError,
  type BrowseTableResponse,
  type CellValue,
  type ColumnMeta,
  type QueryHistoryItem,
  type StatementResult,
} from '@/api/client'
import { useConnectionsStore } from './connections'
import { useSettingsStore } from './settings'

export interface BrowseTab {
  id: string
  kind: 'browse'
  title: string
  connectionId: string
  database: string
  schema: string
  table: string
  page: number
  pageSize: number
  sortColumn: string
  sortDir: '' | 'asc' | 'desc'
  filterWhere: string
  loading: boolean
  showExecutedSql: boolean
  response: BrowseTableResponse | null
  error: AppError | null
}

export interface QueryStatementView extends StatementResult {
  snapshotExpired?: boolean
  expiredMessage?: string
  pagingLoading?: boolean
}

export interface QueryTab {
  id: string
  kind: 'query'
  title: string
  connectionId: string
  database: string
  schema: string
  sql: string
  sessionState: SessionState
  serverConnId: string
  readOnly: boolean
  running: boolean
  activeExecId: string
  cancelPending: boolean
  startedAt: number
  totalMs: number
  statements: QueryStatementView[]
  activeResultIdx: number // -1 = Messages/Summary tab, >=0 = statement index
  bannerError: AppError | null
  noticeMessage: string
  historyOpen: boolean
  history: QueryHistoryItem[]
}

export type WorkspaceTab = BrowseTab | QueryTab

export interface InspectedCell {
  column: ColumnMeta
  cell: CellValue
  rowIndex: number
}

export interface DdlTarget {
  connectionId: string
  database: string
  schema: string
  table: string
}

function makeId(prefix: string): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return `${prefix}-${crypto.randomUUID()}`
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

export const useWorkspaceStore = defineStore('workspace', () => {
  const tabs = ref<WorkspaceTab[]>([])
  const activeTabId = ref<string>('')
  const inspectedCell = ref<InspectedCell | null>(null)
  const ddlTarget = ref<DdlTarget | null>(null)
  const commandPaletteOpen = ref(false)
  let querySeq = 1

  const activeTab = computed<WorkspaceTab | null>(() => {
    return tabs.value.find((t) => t.id === activeTabId.value) ?? null
  })

  function setActiveTab(tabId: string) {
    activeTabId.value = tabId
    inspectedCell.value = null
  }

  function inspectCell(column: ColumnMeta, cell: CellValue, rowIndex: number) {
    inspectedCell.value = { column, cell, rowIndex }
  }

  function closeInspector() {
    inspectedCell.value = null
  }

  function openTableDdl(target: DdlTarget) {
    ddlTarget.value = target
  }

  function closeTableDdl() {
    ddlTarget.value = null
  }

  // --- Live Table Browse Tab (Track A, separate from Execute-Once SQL snapshots) ---

  async function openTableBrowse(connectionId: string, database: string, schema: string, table: string) {
    const settings = useSettingsStore()
    const existing = tabs.value.find(
      (t): t is BrowseTab =>
        t.kind === 'browse' &&
        t.connectionId === connectionId &&
        t.database === database &&
        t.schema === schema &&
        t.table === table,
    )
    if (existing) {
      activeTabId.value = existing.id
      return existing
    }

    const tab: BrowseTab = {
      id: makeId('browse'),
      kind: 'browse',
      title: schema ? `${schema}.${table}` : table,
      connectionId,
      database,
      schema,
      table,
      page: 1,
      pageSize: settings.defaultPageSize,
      sortColumn: '',
      sortDir: '',
      filterWhere: '',
      loading: false,
      showExecutedSql: false,
      response: null,
      error: null,
    }
    tabs.value.push(tab)
    activeTabId.value = tab.id
    await refreshBrowseTab(tab.id)
    return tab
  }

  async function refreshBrowseTab(tabId: string) {
    const tab = tabs.value.find((t): t is BrowseTab => t.id === tabId && t.kind === 'browse')
    if (!tab) return
    tab.loading = true
    tab.error = null
    try {
      const resp = await api.browseTable({
        connectionId: tab.connectionId,
        database: tab.database,
        schema: tab.schema,
        table: tab.table,
        page: tab.page,
        pageSize: tab.pageSize,
        sortColumn: tab.sortColumn,
        sortDir: tab.sortDir,
        filterWhere: tab.filterWhere,
      })
      tab.response = resp
    } catch (err) {
      tab.error = parseAppError(err)
    } finally {
      tab.loading = false
    }
  }

  // --- Dedicated-Connection Query Tab (Track B + Execute-Once Snapshot) ---

  async function openQueryTab(
    connectionId: string,
    database = '',
    schema = '',
    initialSql = '',
    customTitle = '',
  ): Promise<QueryTab | null> {
    const connStore = useConnectionsStore()
    let active = connStore.getActive(connectionId)
    if (!active) {
      const connected = await connStore.connect(connectionId)
      if (!connected) return null
      active = connected
    }

    const conn = connStore.getConnection(connectionId)
    const targetDb = database || active.currentDb || conn?.params.defaultDatabase || ''
    const targetSchema = schema || (active.capabilities.hasSchemas ? 'public' : '')

    const id = makeId('query')
    const tab: QueryTab = {
      id,
      kind: 'query',
      title: customTitle || `查询 ${querySeq++}`,
      connectionId,
      database: targetDb,
      schema: targetSchema,
      sql: initialSql,
      sessionState: SessionState.SessionAliveIdle,
      serverConnId: '',
      readOnly: Boolean(conn?.readOnly),
      running: false,
      activeExecId: '',
      cancelPending: false,
      startedAt: 0,
      totalMs: 0,
      statements: [],
      activeResultIdx: 0,
      bannerError: null,
      noticeMessage: '',
      historyOpen: false,
      history: [],
    }
    tabs.value.push(tab)
    activeTabId.value = tab.id

    // Open dedicated Track B physical connection immediately so user sees confirmed SessionState & ConnID
    try {
      const info = await api.openTabSession(tab.id, connectionId, targetDb, targetSchema)
      tab.database = info.currentDb || targetDb
      tab.schema = info.currentSchema || targetSchema
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || ''
      tab.readOnly = info.readOnly
    } catch (err) {
      const appErr = parseAppError(err)
      tab.sessionState = appErr.sessionState || SessionState.SessionLost
      tab.bannerError = appErr
    }

    // Background fetch completion catalog for CodeMirror 6 SQL autocomplete
    if (tab.database) {
      void connStore.loadCompletionCatalog(connectionId, tab.database, tab.schema)
    }

    return tab
  }

  async function executeQuery(
    tabId: string,
    opts: { sqlOverride?: string; runMode: 'current' | 'all' | 'selection'; cursorOffset: number },
  ) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab || tab.running) return

    const sqlText = (opts.sqlOverride ?? tab.sql).trim()
    if (!sqlText) return

    const settings = useSettingsStore()
    const execId = makeId('exec')

    tab.running = true
    tab.activeExecId = execId
    tab.cancelPending = false
    tab.startedAt = Date.now()
    tab.bannerError = null
    tab.noticeMessage = ''

    try {
      const resp = await api.executeSQL({
        tabId: tab.id,
        execId,
        connectionId: tab.connectionId,
        database: tab.database,
        schema: tab.schema,
        sql: opts.sqlOverride ?? tab.sql,
        cursorOffset: opts.cursorOffset,
        runMode: opts.runMode,
        pageSize: settings.defaultPageSize,
      })

      if (!resp) return

      // Strictly reflect backend-authoritative session state (never guess!)
      tab.sessionState = resp.sessionState
      if (resp.currentDb) tab.database = resp.currentDb
      if (resp.currentSchema !== undefined && resp.currentSchema !== '') {
        tab.schema = resp.currentSchema
      }
      tab.totalMs = resp.totalMs

      const stmts: QueryStatementView[] = (resp.statements ?? []).map((s) => ({
        ...s,
        snapshotExpired: false,
        pagingLoading: false,
      }))
      tab.statements = stmts

      if (resp.error) {
        tab.bannerError = resp.error
      }

      const firstResultIdx = stmts.findIndex((s) => s.isResultSet)
      tab.activeResultIdx = firstResultIdx >= 0 ? firstResultIdx : stmts.length > 0 ? 0 : -1
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      if (appErr.sessionState) {
        tab.sessionState = appErr.sessionState
      }
    } finally {
      tab.running = false
      tab.activeExecId = ''
      tab.cancelPending = false
    }
  }

  /**
   * Pagination on editor SQL results strictly calls FetchResultPage against the Execute-Once snapshot.
   * NEVER re-runs the SQL statement!
   */
  async function fetchSnapshotPage(tabId: string, stmtIdx: number, page: number, pageSize?: number) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab) return
    const stmt = tab.statements[stmtIdx]
    if (!stmt || !stmt.resultId) return

    stmt.pagingLoading = true
    stmt.snapshotExpired = false
    stmt.expiredMessage = undefined

    try {
      const resp = await api.fetchResultPage({
        resultId: stmt.resultId,
        page,
        pageSize: pageSize ?? stmt.pageSize ?? 200,
      })
      if (!resp) return
      stmt.columns = resp.columns
      stmt.rows = resp.rows
      stmt.page = resp.page
      stmt.pageSize = resp.pageSize
      stmt.totalBuffered = resp.totalBuffered
      stmt.hasMorePages = resp.hasMorePages
      stmt.truncatedByLimit = resp.truncatedByLimit
      stmt.limitReason = resp.limitReason
    } catch (err) {
      const appErr = parseAppError(err)
      if (appErr.code === ErrorCode.ErrResultExpired) {
        stmt.snapshotExpired = true
        stmt.expiredMessage = appErr.message
      } else {
        tab.bannerError = appErr
      }
    } finally {
      stmt.pagingLoading = false
    }
  }

  async function cancelQuery(tabId: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab || !tab.running || !tab.activeExecId) return
    const connStore = useConnectionsStore()
    tab.cancelPending = true
    try {
      const res = await api.cancelExecution(tab.id, tab.activeExecId)
      if (res.sessionState) {
        tab.sessionState = res.sessionState
      }
      tab.noticeMessage = res.message
      if (res.status === 'session_lost') {
        connStore.notify('warning', '取消未获确认，原会话已标记结束', res.message)
      }
    } catch (err) {
      const appErr = parseAppError(err)
      connStore.notify('error', '发送取消指令失败', appErr.message)
    }
  }

  async function forceAbortQuery(tabId: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab) return
    const info = await api.forceAbortSession(tab.id)
    tab.sessionState = info.sessionState
    tab.running = false
    tab.cancelPending = false
    tab.activeExecId = ''
    tab.bannerError = {
      code: ErrorCode.ErrSessionLost,
      message: '已强制中断当前物理连接，原会话的未提交事务与临时表已失效',
      retrySafe: false,
      sessionState: SessionState.SessionLost,
    }
  }

  async function reconnectQueryTab(tabId: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab) return
    const connStore = useConnectionsStore()
    try {
      const info = await api.reconnectTabSession(tab.id, tab.database, tab.schema)
      tab.database = info.currentDb || tab.database
      tab.schema = info.currentSchema || tab.schema
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || ''
      tab.bannerError = null
      tab.noticeMessage = '已建立全新物理会话'
      connStore.notify('success', '已建立新会话', info.serverConnId ? `连接 ID: #${info.serverConnId}` : undefined)
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      connStore.notify('error', '重新连接失败', appErr.message)
    }
  }

  async function switchQueryDatabase(tabId: string, targetDb: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab || !targetDb) return
    const connStore = useConnectionsStore()
    try {
      const info = await api.switchTabDatabase(tab.id, targetDb)
      tab.database = info.currentDb || targetDb
      tab.schema = info.currentSchema || ''
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || tab.serverConnId
      tab.bannerError = null
      void connStore.loadCompletionCatalog(tab.connectionId, tab.database, tab.schema)
      const active = connStore.getActive(tab.connectionId)
      if (active?.capabilities.hasSchemas) {
        const schemas = await connStore.loadSchemas(tab.connectionId, tab.database)
        if (!tab.schema && schemas.length > 0) {
          tab.schema = schemas.includes('public') ? 'public' : schemas[0]
        }
      }
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      connStore.notify('warning', '切换数据库失败', appErr.message)
    }
  }

  async function switchQuerySchema(tabId: string, targetSchema: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab || !targetSchema) return
    const connStore = useConnectionsStore()
    try {
      const info = await api.switchTabSchema(tab.id, targetSchema)
      tab.schema = info.currentSchema || targetSchema
      tab.sessionState = info.sessionState
      tab.bannerError = null
      void connStore.loadCompletionCatalog(tab.connectionId, tab.database, tab.schema)
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      connStore.notify('warning', '切换 Schema 失败', appErr.message)
    }
  }

  async function loadTabHistory(tabId: string) {
    const tab = tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
    if (!tab) return
    try {
      tab.history = await api.listHistory(tab.connectionId, 80)
    } catch {
      tab.history = []
    }
  }

  async function closeTab(tabId: string) {
    const idx = tabs.value.findIndex((t) => t.id === tabId)
    if (idx < 0) return
    const tab = tabs.value[idx]
    tabs.value.splice(idx, 1)

    if (activeTabId.value === tabId) {
      const next = tabs.value[idx] || tabs.value[idx - 1]
      activeTabId.value = next ? next.id : ''
      inspectedCell.value = null
    }

    // Release dedicated physical connection + ResultBuffer snapshots on backend!
    if (tab.kind === 'query') {
      try {
        await api.closeTab(tab.id)
      } catch {
        // ignore close errors on already-closed sessions
      }
    }
  }

  async function closeTabsByConnection(connectionId: string) {
    const toClose = tabs.value.filter((t) => t.connectionId === connectionId).map((t) => t.id)
    for (const id of toClose) {
      await closeTab(id)
    }
  }

  return {
    tabs,
    activeTabId,
    activeTab,
    inspectedCell,
    ddlTarget,
    commandPaletteOpen,
    setActiveTab,
    inspectCell,
    closeInspector,
    openTableDdl,
    closeTableDdl,
    openTableBrowse,
    refreshBrowseTab,
    openQueryTab,
    executeQuery,
    fetchSnapshotPage,
    cancelQuery,
    forceAbortQuery,
    reconnectQueryTab,
    switchQueryDatabase,
    switchQuerySchema,
    loadTabHistory,
    closeTab,
    closeTabsByConnection,
  }
})
