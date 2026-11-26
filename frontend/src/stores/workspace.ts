import { defineStore } from 'pinia'
import { computed, markRaw, ref } from 'vue'
import { api, parseAppError } from '@/api/client'
import {
  ErrorCode,
  SessionState,
  type AppError,
  type BrowseTableResponse,
  type ExecuteSQLResponse,
  type QueryHistoryItem,
  type StatementResult,
} from '@/api/types'
import { useConnectionsStore } from './connections'
import { useSchemaStore } from './schema'
import { useSettingsStore } from './settings'
import { useUiStore } from './ui'

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
  /** Baseline SQL (initial template or last executed SQL) used to decide if tab has unsaved edits. */
  cleanSql: string
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

function makeId(prefix: string): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return `${prefix}-${crypto.randomUUID()}`
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

export const useWorkspaceStore = defineStore('workspace', () => {
  const tabs = ref<WorkspaceTab[]>([])
  const activeTabId = ref<string>('')
  let querySeq = 1

  const activeTab = computed<WorkspaceTab | null>(() => {
    return tabs.value.find((t) => t.id === activeTabId.value) ?? null
  })

  function findBrowseTab(tabId: string): BrowseTab | undefined {
    return tabs.value.find((t): t is BrowseTab => t.id === tabId && t.kind === 'browse')
  }

  function findQueryTab(tabId: string): QueryTab | undefined {
    return tabs.value.find((t): t is QueryTab => t.id === tabId && t.kind === 'query')
  }

  function isQueryTabDirty(tab: QueryTab): boolean {
    return tab.running || tab.sql !== tab.cleanSql
  }

  function hasDirtyOrRunningTabs(connectionId: string): boolean {
    return tabs.value.some((t) => t.kind === 'query' && t.connectionId === connectionId && isQueryTabDirty(t))
  }

  function setActiveTab(tabId: string) {
    activeTabId.value = tabId
    useUiStore().closeInspector()
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
    const tab = findBrowseTab(tabId)
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
      tab.response = resp ? markRaw(resp) : null
    } catch (err) {
      tab.error = parseAppError(err)
    } finally {
      tab.loading = false
    }
  }

  function setBrowseFilter(tabId: string, filterWhere: string) {
    const tab = findBrowseTab(tabId)
    if (!tab) return
    tab.filterWhere = filterWhere
  }

  async function applyBrowseFilter(tabId: string) {
    const tab = findBrowseTab(tabId)
    if (!tab) return
    tab.page = 1
    await refreshBrowseTab(tabId)
  }

  async function clearBrowseFilter(tabId: string) {
    const tab = findBrowseTab(tabId)
    if (!tab) return
    tab.filterWhere = ''
    tab.page = 1
    await refreshBrowseTab(tabId)
  }

  async function setBrowsePage(tabId: string, page: number) {
    const tab = findBrowseTab(tabId)
    if (!tab || page < 1) return
    tab.page = page
    await refreshBrowseTab(tabId)
  }

  async function setBrowsePageSize(tabId: string, pageSize: number) {
    const tab = findBrowseTab(tabId)
    if (!tab || pageSize <= 0) return
    tab.pageSize = pageSize
    tab.page = 1
    await refreshBrowseTab(tabId)
  }

  async function toggleBrowseSort(tabId: string, colName: string) {
    const tab = findBrowseTab(tabId)
    if (!tab) return
    if (tab.sortColumn !== colName) {
      tab.sortColumn = colName
      tab.sortDir = 'asc'
    } else if (tab.sortDir === 'asc') {
      tab.sortDir = 'desc'
    } else {
      tab.sortColumn = ''
      tab.sortDir = ''
    }
    tab.page = 1
    await refreshBrowseTab(tabId)
  }

  function toggleBrowseExecutedSql(tabId: string) {
    const tab = findBrowseTab(tabId)
    if (!tab) return
    tab.showExecutedSql = !tab.showExecutedSql
  }

  // --- Dedicated-Connection Query Tab (Track B + Execute-Once Snapshot) ---

  async function syncSchemaIfUnset(tab: QueryTab, hasSchemas: boolean, connEpoch: number) {
    if (!hasSchemas || !tab.database) return
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    try {
      const schemas = await schemaStore.loadSchemas(tab.connectionId, tab.database)
      if (!schemas || !connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tab.id)) {
        return
      }
      if (!tab.schema && schemas.length > 0) {
        const chosen = schemaStore.resolveDefaultSchema(schemas)
        if (chosen) {
          // Synchronize backend session search_path with UI display — never guess locally!
          const info = await api.switchTabSchema(tab.id, chosen)
          if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tab.id)) {
            return
          }
          tab.schema = info.currentSchema || chosen
          tab.sessionState = info.sessionState
        }
      }
    } catch {
      // non-fatal; user can still execute SQL
    }
  }

  async function openQueryTab(
    connectionId: string,
    database = '',
    schema = '',
    initialSql = '',
    customTitle = '',
  ): Promise<QueryTab | null> {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    let active = connStore.getActive(connectionId)
    if (!active) {
      const connected = await connStore.connect(connectionId)
      if (!connected) return null
      active = connected
    }

    const connEpoch = connStore.getConnectionEpoch(connectionId)
    if (!connStore.isConnectionCurrent(connectionId, connEpoch)) {
      return null
    }

    const conn = connStore.getConnection(connectionId)
    const targetDb = schemaStore.resolveEffectiveDatabase(connectionId, database)
    const targetSchema = schema

    const id = makeId('query')
    const tab: QueryTab = {
      id,
      kind: 'query',
      title: customTitle || `查询 ${querySeq++}`,
      connectionId,
      database: targetDb,
      schema: targetSchema,
      sql: initialSql,
      cleanSql: initialSql,
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

    const abortPendingTab = (closeBackend: boolean) => {
      if (closeBackend) {
        void api.closeTab(tab.id).catch(() => {})
      }
      const idx = tabs.value.findIndex((t) => t.id === tab.id)
      if (idx >= 0) {
        tabs.value.splice(idx, 1)
        if (activeTabId.value === tab.id) {
          activeTabId.value = tabs.value[Math.max(0, idx - 1)]?.id ?? ''
        }
      }
    }

    // Open dedicated Track B physical connection immediately so user sees confirmed SessionState & ConnID
    let sessionOpened = false
    try {
      const info = await api.openTabSession(tab.id, connectionId, targetDb, targetSchema)
      sessionOpened = true
      if (!connStore.isConnectionCurrent(connectionId, connEpoch) || !findQueryTab(tab.id)) {
        abortPendingTab(true)
        return null
      }
      tab.database = info.currentDb || targetDb
      tab.schema = info.currentSchema || targetSchema
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || ''
      tab.readOnly = info.readOnly
    } catch (err) {
      if (!connStore.isConnectionCurrent(connectionId, connEpoch) || !findQueryTab(tab.id)) {
        abortPendingTab(false)
        return null
      }
      const appErr = parseAppError(err)
      tab.sessionState = appErr.sessionState || SessionState.SessionLost
      tab.bannerError = appErr
    }

    await syncSchemaIfUnset(tab, Boolean(active.capabilities.hasSchemas), connEpoch)
    if (!connStore.isConnectionCurrent(connectionId, connEpoch)) {
      abortPendingTab(sessionOpened)
      return null
    }
    if (!findQueryTab(tab.id)) {
      return null
    }

    if (tab.database || !active.capabilities.hasDatabases) {
      void schemaStore.loadCompletionCatalog(connectionId, tab.database, tab.schema)
    }

    return tab
  }

  function setQuerySql(tabId: string, sql: string) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    tab.sql = sql
  }

  function setQueryActiveResult(tabId: string, idx: number) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    tab.activeResultIdx = idx
  }

  function setQueryHistoryOpen(tabId: string, open: boolean) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    tab.historyOpen = open
    if (open) {
      void loadTabHistory(tabId)
    }
  }

  function applyHistorySql(tabId: string, sql: string) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    tab.sql = sql
    tab.historyOpen = false
  }

  function applyExecutionResponse(tab: QueryTab, resp: ExecuteSQLResponse, executedBufferSql?: string) {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    const prevDb = tab.database
    const prevSchema = tab.schema

    // Strictly reflect backend-authoritative session state (never guess!)
    tab.sessionState = resp.sessionState
    if (resp.currentDb) tab.database = resp.currentDb
    if (resp.currentSchema !== undefined && resp.currentSchema !== '') {
      tab.schema = resp.currentSchema
    }
    tab.totalMs = resp.totalMs
    if (executedBufferSql !== undefined && resp.executedAll) {
      tab.cleanSql = executedBufferSql
    }

    const stmts: QueryStatementView[] = (resp.statements ?? []).map((s) => ({
      ...s,
      columns: s.columns ? markRaw(s.columns) : null,
      rows: s.rows ? markRaw(s.rows) : null,
      snapshotExpired: false,
      pagingLoading: false,
    }))
    tab.statements = stmts

    if (resp.error) {
      tab.bannerError = resp.error
    }

    const firstResultIdx = stmts.findIndex((s) => s.isResultSet)
    tab.activeResultIdx = firstResultIdx >= 0 ? firstResultIdx : stmts.length > 0 ? 0 : -1

    // Refresh autocomplete catalog & schema list when SQL execution switched DB/Schema (e.g. USE other_db)
    if (tab.database !== prevDb || tab.schema !== prevSchema) {
      const active = connStore.getActive(tab.connectionId)
      if (!active) return
      if (tab.database !== prevDb && active.capabilities.hasSchemas && tab.database) {
        void schemaStore.loadSchemas(tab.connectionId, tab.database)
      }
      if (tab.database || active.capabilities.hasDatabases === false) {
        void schemaStore.loadCompletionCatalog(tab.connectionId, tab.database, tab.schema)
      }
    }
  }

  async function executeQuery(
    tabId: string,
    opts: { sqlOverride?: string; runMode: 'current' | 'all' | 'selection'; cursorOffset: number },
  ) {
    const tab = findQueryTab(tabId)
    if (!tab || tab.running) return

    const rawSql = opts.sqlOverride ?? tab.sql
    if (!rawSql.trim()) return
    const executedBufferSql = opts.sqlOverride === undefined ? rawSql : undefined

    const settings = useSettingsStore()
    const connStore = useConnectionsStore()
    const uiStore = useUiStore()

    const runRequest = async (confirmToken?: string): Promise<ExecuteSQLResponse | null> => {
      const execId = makeId('exec')
      tab.running = true
      tab.activeExecId = execId
      tab.cancelPending = false
      tab.startedAt = Date.now()
      tab.bannerError = null
      tab.noticeMessage = ''

      try {
        return await api.executeSQL({
          tabId: tab.id,
          execId,
          connectionId: tab.connectionId,
          database: tab.database,
          schema: tab.schema,
          sql: rawSql,
          cursorOffset: opts.cursorOffset,
          runMode: opts.runMode,
          pageSize: settings.defaultPageSize,
          ...(confirmToken ? { confirmToken } : {}),
        })
      } catch (err) {
        const appErr = parseAppError(err)
        tab.bannerError = appErr
        if (appErr.sessionState) {
          tab.sessionState = appErr.sessionState
        }
        return null
      } finally {
        tab.running = false
        tab.activeExecId = ''
        tab.cancelPending = false
      }
    }

    const resp = await runRequest()
    if (!resp) return

    // Backend single-source-of-truth high-risk production SQL confirmation gate (docs/03 §4.2)
    if (resp.requiresConfirm && resp.highRiskStatements && resp.highRiskStatements.length > 0 && resp.confirmToken) {
      const conn = connStore.getConnection(tab.connectionId)
      const preview = resp.highRiskStatements
        .map((r) => r.sql)
        .join(';\n')
        .slice(0, 600)
      const confirmed = await uiStore.requestConfirm({
        title: '生产环境高危 SQL 执行确认',
        message: `当前连接「${conn?.name ?? tab.connectionId}」已标记为生产环境，检测到以下高危语句，确认仍要继续执行吗？`,
        tone: 'danger',
        bullets: Array.from(new Set(resp.highRiskStatements.map((r) => r.label))),
        codePreview: preview,
        confirmLabel: '确认在生产环境执行',
        cancelLabel: '取消执行',
      })
      if (!findQueryTab(tabId)) return
      if (!confirmed) {
        tab.noticeMessage = '已取消生产环境高危 SQL 执行'
        return
      }
      const confirmedResp = await runRequest(resp.confirmToken)
      if (confirmedResp) {
        applyExecutionResponse(tab, confirmedResp, executedBufferSql)
      }
      return
    }

    applyExecutionResponse(tab, resp, executedBufferSql)
  }

  /**
   * Pagination on editor SQL results strictly calls FetchResultPage against the Execute-Once snapshot.
   * NEVER re-runs the SQL statement!
   */
  async function fetchSnapshotPage(tabId: string, stmtIdx: number, page: number, pageSize?: number) {
    const tab = findQueryTab(tabId)
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
      stmt.columns = resp.columns ? markRaw(resp.columns) : null
      stmt.rows = resp.rows ? markRaw(resp.rows) : null
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
    const tab = findQueryTab(tabId)
    if (!tab || !tab.running || !tab.activeExecId) return
    const uiStore = useUiStore()
    tab.cancelPending = true
    try {
      const res = await api.cancelExecution(tab.id, tab.activeExecId)
      if (res.sessionState) {
        tab.sessionState = res.sessionState
      }
      tab.noticeMessage = res.message
      if (res.status === 'session_lost') {
        uiStore.notify('warning', '取消未获确认，原会话已标记结束', res.message)
      }
    } catch (err) {
      const appErr = parseAppError(err)
      tab.cancelPending = false
      uiStore.notify('error', '发送取消指令失败', appErr.message)
    }
  }

  async function forceAbortQuery(tabId: string) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    const uiStore = useUiStore()
    try {
      const info = await api.forceAbortSession(tab.id)
      tab.sessionState = info.sessionState
      tab.running = false
      tab.activeExecId = ''
      tab.bannerError = {
        code: ErrorCode.ErrSessionLost,
        message: '已强制中断当前物理连接，原会话的未提交事务与临时表已失效',
        retrySafe: false,
        sessionState: SessionState.SessionLost,
      }
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      if (appErr.sessionState) {
        tab.sessionState = appErr.sessionState
      }
      uiStore.notify('error', '强制中断会话失败', appErr.message)
    } finally {
      tab.cancelPending = false
    }
  }

  async function reconnectQueryTab(tabId: string) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    const uiStore = useUiStore()
    try {
      const info = await api.reconnectTabSession(tab.id, tab.database, tab.schema)
      tab.database = info.currentDb || tab.database
      tab.schema = info.currentSchema || tab.schema
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || ''
      tab.bannerError = null
      tab.noticeMessage = '已建立全新物理会话'
      uiStore.notify('success', '已建立新会话', info.serverConnId ? `连接 ID: #${info.serverConnId}` : undefined)
    } catch (err) {
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      uiStore.notify('error', '重新连接失败', appErr.message)
    }
  }

  async function switchQueryDatabase(tabId: string, targetDb: string) {
    const tab = findQueryTab(tabId)
    if (!tab || !targetDb) return
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const uiStore = useUiStore()
    const connEpoch = connStore.getConnectionEpoch(tab.connectionId)
    try {
      const info = await api.switchTabDatabase(tab.id, targetDb)
      if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tabId)) {
        return
      }
      tab.database = info.currentDb || targetDb
      tab.schema = info.currentSchema || ''
      tab.sessionState = info.sessionState
      tab.serverConnId = info.serverConnId || tab.serverConnId
      tab.bannerError = null

      const active = connStore.getActive(tab.connectionId)
      await syncSchemaIfUnset(tab, Boolean(active?.capabilities.hasSchemas), connEpoch)
      if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tabId)) {
        return
      }
      void schemaStore.loadCompletionCatalog(tab.connectionId, tab.database, tab.schema)
    } catch (err) {
      if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tabId)) {
        return
      }
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      uiStore.notify('warning', '切换数据库失败', appErr.message)
    }
  }

  async function switchQuerySchema(tabId: string, targetSchema: string) {
    const tab = findQueryTab(tabId)
    if (!tab || !targetSchema) return
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const uiStore = useUiStore()
    const connEpoch = connStore.getConnectionEpoch(tab.connectionId)
    try {
      const info = await api.switchTabSchema(tab.id, targetSchema)
      if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tabId)) {
        return
      }
      tab.schema = info.currentSchema || targetSchema
      tab.sessionState = info.sessionState
      tab.bannerError = null
      void schemaStore.loadCompletionCatalog(tab.connectionId, tab.database, tab.schema)
    } catch (err) {
      if (!connStore.isConnectionCurrent(tab.connectionId, connEpoch) || !findQueryTab(tabId)) {
        return
      }
      const appErr = parseAppError(err)
      tab.bannerError = appErr
      uiStore.notify('warning', '切换 Schema 失败', appErr.message)
    }
  }

  async function loadTabHistory(tabId: string) {
    const tab = findQueryTab(tabId)
    if (!tab) return
    try {
      tab.history = await api.listHistory(tab.connectionId, 80)
    } catch {
      tab.history = []
    }
  }

  async function closeTab(tabId: string, opts?: { confirmDirty?: boolean }) {
    const idx = tabs.value.findIndex((t) => t.id === tabId)
    if (idx < 0) return
    const tab = tabs.value[idx]

    if (opts?.confirmDirty && tab.kind === 'query' && isQueryTabDirty(tab)) {
      const uiStore = useUiStore()
      const msg = tab.running
        ? `标签「${tab.title}」正在执行查询，关闭将立即释放物理连接并中止执行，确认关闭吗？`
        : `标签「${tab.title}」包含尚未执行的修改内容，关闭将同步释放其物理连接与结果快照，确认关闭吗？`
      const ok = await uiStore.requestConfirm({
        title: '关闭查询标签',
        message: msg,
        tone: tab.running ? 'danger' : 'warning',
        confirmLabel: '关闭标签',
        cancelLabel: '保留',
      })
      if (!ok) return
    }

    const currentIdx = tabs.value.findIndex((t) => t.id === tabId)
    if (currentIdx < 0) return
    tabs.value.splice(currentIdx, 1)

    if (activeTabId.value === tabId) {
      const next = tabs.value[currentIdx] || tabs.value[currentIdx - 1]
      activeTabId.value = next ? next.id : ''
      useUiStore().closeInspector()
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

  function openQuickQuery() {
    const connStore = useConnectionsStore()
    const uiStore = useUiStore()
    const tab = activeTab.value
    if (tab) {
      void openQueryTab(tab.connectionId, tab.database, tab.schema)
      return
    }
    const firstActive = Object.keys(connStore.activeMap)[0]
    if (firstActive) {
      void openQueryTab(firstActive)
      return
    }
    const firstSaved = connStore.connections[0]
    if (firstSaved) {
      void openQueryTab(firstSaved.id)
      return
    }
    uiStore.openNewConnectionModal()
  }

  return {
    tabs,
    activeTabId,
    activeTab,
    isQueryTabDirty,
    hasDirtyOrRunningTabs,
    setActiveTab,
    openTableBrowse,
    refreshBrowseTab,
    setBrowseFilter,
    applyBrowseFilter,
    clearBrowseFilter,
    setBrowsePage,
    setBrowsePageSize,
    toggleBrowseSort,
    toggleBrowseExecutedSql,
    openQueryTab,
    setQuerySql,
    setQueryActiveResult,
    setQueryHistoryOpen,
    applyHistorySql,
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
    openQuickQuery,
  }
})
