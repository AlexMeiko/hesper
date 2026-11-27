import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { api } from '@/api/client'
import { EngineType, ErrorCode, SessionState, type ConnectionDTO, type ConnectResult } from '@/api/types'
import ExplorerSidebar from '@/components/ExplorerSidebar.vue'
import TableBrowseView from '@/components/TableBrowseView.vue'
import { useConnectionsStore } from '@/stores/connections'
import { useSchemaStore } from '@/stores/schema'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import {
  makeConn,
  makeConnectResult,
  makeQueryTab,
  makeSqliteConn,
  makeSqliteConnectResult,
  mockVirtualViewport,
} from './testHelpers'

enableAutoUnmount(afterEach)

function makePgConn(id: string): ConnectionDTO {
  return makeConn({ id, name: id, driver: EngineType.EnginePostgres, envColor: 'green' })
}

function markConnected(
  connStore: ReturnType<typeof useConnectionsStore>,
  conn: ConnectionDTO,
  result: ConnectResult,
) {
  connStore.connections = [conn]
  connStore.activeMap[conn.id] = result
}

describe('schemaStore request deduplication & stale-write protection', () => {
  let restoreViewport: (() => void) | null = null

  beforeEach(() => {
    setActivePinia(createPinia())
    restoreViewport = mockVirtualViewport(280, 600)
  })

  afterEach(() => {
    restoreViewport?.()
    restoreViewport = null
    vi.restoreAllMocks()
  })

  it('deduplicates concurrent loadSchemas and loadCompletionCatalog requests', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    connStore.activeMap['conn-1'] = makeConnectResult({ connectionId: 'conn-1' })

    const schemasSpy = vi.spyOn(api, 'listSchemas').mockResolvedValue(['public', 'analytics'])
    const catSpy = vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue({
      database: 'app_db',
      schema: 'public',
      tables: { users: ['id', 'name'] },
    })

    const [s1, s2] = await Promise.all([
      schemaStore.loadSchemas('conn-1', 'app_db'),
      schemaStore.loadSchemas('conn-1', 'app_db'),
    ])
    expect(schemasSpy).toHaveBeenCalledTimes(1)
    expect(s1).toEqual(['public', 'analytics'])
    expect(s2).toEqual(['public', 'analytics'])

    const [c1, c2] = await Promise.all([
      schemaStore.loadCompletionCatalog('conn-1', 'app_db', 'public'),
      schemaStore.loadCompletionCatalog('conn-1', 'app_db', 'public'),
    ])
    expect(catSpy).toHaveBeenCalledTimes(1)
    expect(c1?.tables?.users).toEqual(['id', 'name'])
    expect(c2?.tables?.users).toEqual(['id', 'name'])
  })

  it('ignores stale force-refresh responses that resolve after a newer refresh', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    connStore.activeMap['conn-1'] = makeConnectResult({ connectionId: 'conn-1' })

    let resolveOld!: (v: string[]) => void
    let resolveNew!: (v: string[]) => void
    let callNum = 0
    vi.spyOn(api, 'listSchemas').mockImplementation(() => {
      callNum++
      if (callNum === 1) return new Promise<string[]>((r) => (resolveOld = r))
      return new Promise<string[]>((r) => (resolveNew = r))
    })

    const pOld = schemaStore.loadSchemas('conn-1', 'race_db', true)
    const pNew = schemaStore.loadSchemas('conn-1', 'race_db', true)
    resolveNew(['fresh_schema'])
    expect(await pNew).toEqual(['fresh_schema'])

    resolveOld(['stale_schema'])
    expect(await pOld).toBeNull()
    expect(schemaStore.schemasMap[schemaStore.dbKey('conn-1', 'race_db')]).toEqual(['fresh_schema'])
  })

  it('returns null for late post-disconnect loadSchemas responses and stops ExplorerSidebar from repopulating objectsMap', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    // 1. Direct return-value contract: late loadSchemas() after clearConnectionCache MUST resolve to null
    connStore.activeMap['conn-1'] = makeConnectResult({ connectionId: 'conn-1' })
    let resolveLate!: (v: string[]) => void
    vi.spyOn(api, 'listSchemas').mockImplementation(() => new Promise<string[]>((r) => (resolveLate = r)))
    const pLate = schemaStore.loadSchemas('conn-1', 'disc_db', true)
    schemaStore.clearConnectionCache('conn-1')
    resolveLate(['ghost_schema'])
    expect(await pLate).toBeNull()
    expect(schemaStore.schemasMap[schemaStore.dbKey('conn-1', 'disc_db')]).toBeUndefined()

    // 2. End-to-end check on ExplorerSidebar: disconnecting while expandDatabase awaits loadSchemas must NOT repopulate objectsMap
    const pgConn = makePgConn('pg-race')
    markConnected(connStore, pgConn, makeConnectResult({ connectionId: 'pg-race', currentDb: 'app_db' }))
    schemaStore.setDatabases('pg-race', [{ name: 'app_db', isSystem: false }])

    let resolveSidebarSchemas!: (v: string[]) => void
    vi.spyOn(api, 'listSchemas').mockImplementation(() => new Promise<string[]>((r) => (resolveSidebarSchemas = r)))
    const listTablesSpy = vi.spyOn(api, 'listTables').mockResolvedValue([
      { database: 'app_db', name: 'ghost_tbl', engine: '', estimatedRows: 0 },
    ])

    const sidebar = mount(ExplorerSidebar)
    await sidebar.find('.cursor-pointer').trigger('click')
    await nextTick()
    const dbRow = sidebar.findAll('.cursor-pointer').find((el) => el.text().includes('app_db'))
    expect(dbRow).toBeDefined()
    await dbRow!.trigger('click')

    schemaStore.clearConnectionCache('pg-race')
    resolveSidebarSchemas(['public'])
    await flushPromises()

    expect(schemaStore.schemasMap[schemaStore.dbKey('pg-race', 'app_db')]).toBeUndefined()
    expect(listTablesSpy).not.toHaveBeenCalled()
    expect(schemaStore.objectsMap[schemaStore.objKey('pg-race', 'app_db', 'public')]).toBeUndefined()
  })
})

describe('Connection lifecycle serialization, disconnect races & ExplorerSidebar', () => {
  let restoreViewport: (() => void) | null = null

  beforeEach(() => {
    setActivePinia(createPinia())
    restoreViewport = mockVirtualViewport(280, 600)
  })

  afterEach(() => {
    restoreViewport?.()
    restoreViewport = null
    vi.restoreAllMocks()
  })

  it('coalesces concurrent connect() calls so callers share the in-flight Promise instead of receiving null', async () => {
    const connStore = useConnectionsStore()
    let resolveConnect!: (res: ConnectResult) => void
    const connectSpy = vi.spyOn(api, 'connect').mockImplementation(
      () => new Promise<ConnectResult>((resolve) => (resolveConnect = resolve)),
    )
    vi.spyOn(api, 'listDatabases').mockResolvedValue([{ name: 'app_db', isSystem: false }])

    const p1 = connStore.connect('conn-1')
    const p2 = connStore.connect('conn-1')
    expect(connectSpy).toHaveBeenCalledTimes(1)

    const result = makeConnectResult({ connectionId: 'conn-1' })
    resolveConnect(result)

    const [r1, r2] = await Promise.all([p1, p2])
    expect(r1).toEqual(result)
    expect(r2).toEqual(result)
  })

  it('aborts in-flight connect() when explicitly disconnected while listing databases', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    // Starts offline (not in activeMap) to test initial connect()
    connStore.connections = [makePgConn('pg-disc')]

    let resolveDbs!: (v: Array<{ name: string; isSystem: boolean }>) => void
    vi.spyOn(api, 'connect').mockResolvedValue(makeConnectResult({ connectionId: 'pg-disc', currentDb: 'app_db' }))
    vi.spyOn(api, 'listDatabases').mockImplementation(() => new Promise((r) => (resolveDbs = r)))
    vi.spyOn(api, 'disconnect').mockResolvedValue()
    const openTabSpy = vi.spyOn(api, 'openTabSession')
    const catalogSpy = vi.spyOn(api, 'fetchCompletionCatalog')

    const pendingOpen = wsStore.openQueryTab('pg-disc', 'app_db')
    await flushPromises()
    await connStore.disconnect('pg-disc')
    resolveDbs([{ name: 'app_db', isSystem: false }])

    expect(await pendingOpen).toBeNull()
    expect(connStore.activeMap['pg-disc']).toBeUndefined()
    expect(schemaStore.databasesMap['pg-disc']).toBeUndefined()
    expect(wsStore.tabs.length).toBe(0)
    expect(openTabSpy).not.toHaveBeenCalled()
    expect(catalogSpy).not.toHaveBeenCalled()
  })

  it('preserves shared schemaStore cache when closing a single query tab while the connection remains active', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    markConnected(connStore, makePgConn('pg-shared'), makeConnectResult({ connectionId: 'pg-shared', currentDb: 'app_db' }))

    vi.spyOn(api, 'openTabSession').mockResolvedValue({
      tabId: 'tab-x',
      connectionId: 'pg-shared',
      currentDb: 'app_db',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      serverConnId: '88',
      readOnly: false,
    })
    vi.spyOn(api, 'closeTab').mockResolvedValue()
    vi.spyOn(api, 'listSchemas').mockResolvedValue(['public'])
    vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue({
      database: 'app_db',
      schema: 'public',
      tables: { users: ['id'] },
    })

    const tabA = await wsStore.openQueryTab('pg-shared', 'app_db', 'public')
    const tabB = await wsStore.openQueryTab('pg-shared', 'app_db', 'public')
    await flushPromises()
    expect(tabA).not.toBeNull()
    expect(tabB).not.toBeNull()
    expect(schemaStore.catalogMap[schemaStore.objKey('pg-shared', 'app_db', 'public')]).toBeDefined()

    await wsStore.closeTab(tabA!.id)
    expect(wsStore.tabs.map((t) => t.id)).toEqual([tabB!.id])
    expect(schemaStore.catalogMap[schemaStore.objKey('pg-shared', 'app_db', 'public')]).toBeDefined()
  })

  it('serializes connect -> disconnect -> reconnect without cross-epoch teardown or wiping fresh state', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    // Starts offline to test full connect#1 -> disconnect#1 -> connect#2 serialization
    connStore.connections = [makePgConn('pg-ser')]

    const events: string[] = []
    let resolveConnect1!: (v: ConnectResult) => void
    let connectCalls = 0
    vi.spyOn(api, 'connect').mockImplementation(() => {
      connectCalls++
      if (connectCalls === 1) {
        events.push('connect#1:start')
        return new Promise<ConnectResult>((r) => (resolveConnect1 = r))
      }
      events.push('connect#2:start')
      return Promise.resolve(
        makeConnectResult({ connectionId: 'pg-ser', serverVersion: '16.2-fresh', currentDb: 'app_db' }),
      )
    })

    let resolveDisc1!: () => void
    let discCalls = 0
    vi.spyOn(api, 'disconnect').mockImplementation(() => {
      discCalls++
      events.push(`disconnect#${discCalls}:start`)
      if (discCalls === 1) {
        return new Promise<void>((r) => (resolveDisc1 = r))
      }
      return Promise.resolve()
    })
    vi.spyOn(api, 'listDatabases').mockResolvedValue([{ name: 'app_db', isSystem: false }])

    const pConn1 = connStore.connect('pg-ser')
    await nextTick()
    const pDisc1 = connStore.disconnect('pg-ser')
    const pConn2 = connStore.connect('pg-ser')

    // Old connect#1 finishes -> queued disconnect#1 starts (NOT connect#2 yet!)
    resolveConnect1(makeConnectResult({ connectionId: 'pg-ser', serverVersion: '16.1-old', currentDb: 'app_db' }))
    expect(await pConn1).toBeNull()
    await flushPromises()
    expect(events).toEqual(['connect#1:start', 'disconnect#1:start'])
    expect(connStore.connectingMap['pg-ser']).toBe(true)

    // disconnect#1 finishes -> connect#2 runs AFTER it, and disconnect#1 finally MUST NOT wipe connect#2 state/cache
    resolveDisc1()
    await pDisc1
    const res2 = await pConn2
    expect(res2?.serverVersion).toBe('16.2-fresh')
    expect(events).toEqual(['connect#1:start', 'disconnect#1:start', 'connect#2:start'])
    expect(discCalls).toBe(1)
    expect(connStore.activeMap['pg-ser']?.serverVersion).toBe('16.2-fresh')
    expect(schemaStore.databasesMap['pg-ser']).toEqual([{ name: 'app_db', isSystem: false }])
    expect(connStore.connectingMap['pg-ser']).toBe(false)
  })

  it('invalidates connection before closing tabs on sidebar disconnect so in-flight openQueryTab aborts without orphan tabs or cache refill', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    markConnected(connStore, makePgConn('pg-orphan'), makeConnectResult({ connectionId: 'pg-orphan', currentDb: 'app_db' }))
    wsStore.tabs.push(makeQueryTab({ id: 'q-existing', connectionId: 'pg-orphan', sql: '', cleanSql: '' }))

    let resolveCloseExisting!: () => void
    const closeTabSpy = vi.spyOn(api, 'closeTab').mockImplementation((tabId) => {
      if (tabId === 'q-existing') {
        return new Promise<void>((r) => (resolveCloseExisting = r))
      }
      return Promise.resolve()
    })
    let resolveNewSession!: (v: Awaited<ReturnType<typeof api.openTabSession>>) => void
    vi.spyOn(api, 'openTabSession').mockImplementation(() => new Promise((r) => (resolveNewSession = r)))
    vi.spyOn(api, 'disconnect').mockResolvedValue()
    const catalogSpy = vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue({
      database: 'app_db',
      schema: 'public',
      tables: { users: ['id'] },
    })

    const sidebar = mount(ExplorerSidebar)
    const pendingOpen = wsStore.openQueryTab('pg-orphan', 'app_db', 'public')
    await nextTick()
    const openingId = wsStore.tabs.find((t) => t.id !== 'q-existing')?.id
    expect(openingId).toBeDefined()

    // Trigger disconnect via sidebar button (invalidates connection before awaiting closeTabsByConnection)
    await sidebar.find('[title="断开连接"]').trigger('click')
    await nextTick()

    // Earlier openQueryTab resolves DURING closeTabsByConnection wait -> MUST abort, close backend session, and never refill catalog
    resolveNewSession({
      tabId: openingId!,
      connectionId: 'pg-orphan',
      currentDb: 'app_db',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      serverConnId: '99',
      readOnly: false,
    })
    expect(await pendingOpen).toBeNull()

    resolveCloseExisting()
    await flushPromises()
    expect(connStore.activeMap['pg-orphan']).toBeUndefined()
    expect(wsStore.tabs.length).toBe(0)
    expect(closeTabSpy).toHaveBeenCalledWith(openingId)
    expect(catalogSpy).not.toHaveBeenCalled()
    expect(schemaStore.catalogMap[schemaStore.objKey('pg-orphan', 'app_db', 'public')]).toBeUndefined()
  })

  it('resets connectingMap when deleteConnection fails while connect() is in flight', async () => {
    const connStore = useConnectionsStore()
    // Starts offline to initiate an in-flight connect()
    connStore.connections = [makePgConn('pg-del')]

    let resolveConnectBeforeDel!: (v: ConnectResult) => void
    vi.spyOn(api, 'connect').mockImplementation(() => new Promise((r) => (resolveConnectBeforeDel = r)))
    vi.spyOn(api, 'disconnect').mockResolvedValue()
    vi.spyOn(api, 'deleteConnection').mockRejectedValue({
      code: ErrorCode.ErrInternal,
      message: '磁盘只读，删除失败',
      retrySafe: false,
    })

    const pConnBeforeDel = connStore.connect('pg-del')
    expect(connStore.connectingMap['pg-del']).toBe(true)
    await nextTick()

    const pDelFail = connStore.deleteConnection('pg-del')
    expect(connStore.connectingMap['pg-del']).toBe(false)

    resolveConnectBeforeDel(makeConnectResult({ connectionId: 'pg-del', currentDb: 'app_db' }))
    expect(await pConnBeforeDel).toBeNull()
    await pDelFail
    expect(connStore.connectingMap['pg-del']).toBe(false)
    expect(connStore.activeMap['pg-del']).toBeUndefined()
  })

  it('virtualizes flattened ExplorerSidebar object trees instead of rendering all 500 table nodes', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    // Starts offline so clicking the sidebar row exercises initial connect + expand
    connStore.connections = [makeSqliteConn('sqlite-1')]
    vi.spyOn(api, 'connect').mockResolvedValue(makeSqliteConnectResult('sqlite-1'))
    vi.spyOn(api, 'listTables').mockResolvedValue(
      Array.from({ length: 500 }, (_, i) => ({
        database: 'main',
        name: `tbl_${String(i).padStart(4, '0')}`,
        engine: '',
        estimatedRows: 0,
      })),
    )
    vi.spyOn(api, 'listViews').mockResolvedValue([{ database: 'main', name: 'v_summary', kind: 'view' }])

    const sidebar = mount(ExplorerSidebar)
    await sidebar.find('.cursor-pointer').trigger('click')
    await flushPromises()

    expect(schemaStore.objectsMap[schemaStore.objKey('sqlite-1', 'main', '')]?.tables.length).toBe(500)
    expect(sidebar.text()).toContain('tbl_0000')
    expect(sidebar.findAll('[title*="双击实时浏览表数据"]').length).toBeGreaterThan(0)
    expect(sidebar.findAll('[title*="双击实时浏览表数据"]').length).toBeLessThan(80)
  })

  it('confirms before disconnecting a connection with dirty query tabs and closes its tabs once confirmed', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()
    const uiStore = useUiStore()

    markConnected(connStore, makeSqliteConn('sqlite-dirty'), makeSqliteConnectResult('sqlite-dirty'))
    wsStore.tabs.push(
      makeQueryTab({
        id: 'q-sqlite-dirty',
        connectionId: 'sqlite-dirty',
        database: 'main',
        schema: '',
        sql: 'UPDATE settings SET v = 2;',
        cleanSql: '',
      }),
    )

    const disconnectSpy = vi.spyOn(api, 'disconnect').mockResolvedValue()
    const closeTabSpy = vi.spyOn(api, 'closeTab').mockResolvedValue()

    const sidebar = mount(ExplorerSidebar)
    await sidebar.find('[title="断开连接"]').trigger('click')
    await nextTick()
    expect(uiStore.confirmDialog).not.toBeNull()
    expect(uiStore.confirmDialog?.title).toBe('断开数据库连接')

    // Cancelling preserves both the connection and the dirty tab
    uiStore.resolveConfirm(false)
    await flushPromises()
    expect(disconnectSpy).not.toHaveBeenCalled()
    expect(connStore.activeMap['sqlite-dirty']).toBeDefined()
    expect(wsStore.tabs.some((t) => t.id === 'q-sqlite-dirty')).toBe(true)

    // Confirming closes all tabs for that connection and disconnects cleanly
    await sidebar.find('[title="断开连接"]').trigger('click')
    await nextTick()
    expect(uiStore.confirmDialog).not.toBeNull()
    uiStore.resolveConfirm(true)
    await flushPromises()
    expect(closeTabSpy).toHaveBeenCalledWith('q-sqlite-dirty')
    expect(disconnectSpy).toHaveBeenCalledWith('sqlite-dirty')
    expect(connStore.activeMap['sqlite-dirty']).toBeUndefined()
    expect(wsStore.tabs.some((t) => t.id === 'q-sqlite-dirty')).toBe(false)
  })

  it('auto-expands and selects the matching table in ExplorerSidebar when switching active browse tabs', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    markConnected(
      connStore,
      makePgConn('pg-sync'),
      makeConnectResult({
        connectionId: 'pg-sync',
        currentDb: 'db_a',
      }),
    )
    schemaStore.databasesMap['pg-sync'] = [
      { name: 'db_a', isSystem: false },
      { name: 'db_b', isSystem: false },
    ]
    schemaStore.schemasMap[schemaStore.dbKey('pg-sync', 'db_a')] = ['public']
    schemaStore.objectsMap[schemaStore.objKey('pg-sync', 'db_a', 'public')] = {
      tables: [{ database: 'db_a', schema: 'public', name: 'users', estimatedRows: 10 }],
      views: [],
      sequences: [],
      loading: false,
      loaded: true,
    }

    vi.spyOn(api, 'listSchemas').mockResolvedValue(['analytics'])
    vi.spyOn(api, 'listTables').mockResolvedValue([
      { database: 'db_b', schema: 'analytics', name: 'events', estimatedRows: 20 },
    ])
    vi.spyOn(api, 'listViews').mockResolvedValue([])
    vi.spyOn(api, 'listSequences').mockResolvedValue([])

    wsStore.tabs.push(
      {
        id: 'browse-users',
        kind: 'browse',
        title: 'public.users',
        connectionId: 'pg-sync',
        database: 'db_a',
        schema: 'public',
        table: 'users',
        page: 1,
        pageSize: 200,
        sortColumn: '',
        sortDir: '',
        filterWhere: '',
        loading: false,
        showExecutedSql: false,
        response: null,
        error: null,
      },
      {
        id: 'browse-events',
        kind: 'browse',
        title: 'analytics.events',
        connectionId: 'pg-sync',
        database: 'db_b',
        schema: 'analytics',
        table: 'events',
        page: 1,
        pageSize: 200,
        sortColumn: '',
        sortDir: '',
        filterWhere: '',
        loading: false,
        showExecutedSql: false,
        response: null,
        error: null,
      },
      makeQueryTab({ id: 'q-1', connectionId: 'pg-sync', database: 'db_a', schema: 'public' }),
    )
    wsStore.setActiveTab('browse-users')

    const sidebar = mount(ExplorerSidebar)
    await flushPromises()
    await nextTick()

    // Initial active browse tab auto-expands conn/db_a/public and selects "users"
    expect(sidebar.find('[data-selected="true"]').text()).toContain('users')

    // Switching to browse-events auto-expands db_b/analytics, loads objects, and highlights "events"
    wsStore.setActiveTab('browse-events')
    await flushPromises()
    await nextTick()

    const selectedAfterSwitch = sidebar.findAll('[data-selected="true"]')
    expect(selectedAfterSwitch).toHaveLength(1)
    expect(selectedAfterSwitch[0].text()).toContain('events')

    // Switching to a query tab clears table highlight cleanly
    wsStore.setActiveTab('q-1')
    await nextTick()
    expect(sidebar.findAll('[data-selected="true"]')).toHaveLength(0)
  })

  it('auto-refreshes Explorer after DROP DATABASE / CREATE TABLE and warns on preserved browse tabs', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    markConnected(
      connStore,
      makePgConn('pg-ddl'),
      makeConnectResult({ connectionId: 'pg-ddl', currentDb: 'postgres' }),
    )
    schemaStore.databasesMap['pg-ddl'] = [
      { name: 'postgres', isSystem: true },
      { name: 'blue_m2', isSystem: false },
    ]
    schemaStore.schemasMap[schemaStore.dbKey('pg-ddl', 'blue_m2')] = ['public']
    schemaStore.objectsMap[schemaStore.objKey('pg-ddl', 'blue_m2', 'public')] = {
      tables: [{ database: 'blue_m2', schema: 'public', name: 'orders', estimatedRows: 10 }],
      views: [],
      sequences: [],
      loading: false,
      loaded: true,
    }
    schemaStore.objectsMap[schemaStore.objKey('pg-ddl', 'postgres', 'public')] = {
      tables: [],
      views: [],
      sequences: [],
      loading: false,
      loaded: true,
    }

    const browseTab = {
      id: 'browse-blue',
      kind: 'browse' as const,
      title: 'public.orders',
      connectionId: 'pg-ddl',
      database: 'blue_m2',
      schema: 'public',
      table: 'orders',
      page: 1,
      pageSize: 200,
      sortColumn: '',
      sortDir: '' as const,
      filterWhere: '',
      loading: false,
      showExecutedSql: false,
      response: null,
      error: null,
    }
    wsStore.tabs.push(
      browseTab,
      makeQueryTab({ id: 'q-ddl', connectionId: 'pg-ddl', database: 'postgres', schema: 'public', sql: 'DROP DATABASE blue_m2;' }),
    )

    const listDbsSpy = vi.spyOn(api, 'listDatabases').mockResolvedValue([{ name: 'postgres', isSystem: true }])
    const listTablesSpy = vi.spyOn(api, 'listTables').mockResolvedValue([
      { database: 'postgres', schema: 'public', name: 'new_tbl', estimatedRows: 0 },
    ])
    vi.spyOn(api, 'listViews').mockResolvedValue([])
    vi.spyOn(api, 'listSequences').mockResolvedValue([])
    vi.spyOn(api, 'executeSQL').mockResolvedValueOnce({
      tabId: 'q-ddl',
      execId: 'e1',
      currentDb: 'postgres',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 2,
      statements: [{ index: 0, sql: 'DROP DATABASE blue_m2;', startLine: 1, isResultSet: false, affectedRows: 0, durationMs: 8 }],
      executedAll: true,
      totalMs: 8,
      schemaChanges: {
        databasesChanged: true,
        droppedDatabases: ['blue_m2'],
      },
    })

    await wsStore.executeQuery('q-ddl', { runMode: 'all', cursorOffset: 0 })
    await flushPromises()

    expect(listDbsSpy).toHaveBeenCalledWith('pg-ddl')
    expect(schemaStore.databasesMap['pg-ddl'].map((d) => d.name)).toEqual(['postgres'])
    expect(schemaStore.schemasMap[schemaStore.dbKey('pg-ddl', 'blue_m2')]).toBeUndefined()
    expect(schemaStore.objectsMap[schemaStore.objKey('pg-ddl', 'blue_m2', 'public')]).toBeUndefined()

    // Browse tab is preserved and displays a dropped-database warning banner
    const preserved = wsStore.tabs.find((t) => t.id === 'browse-blue')
    expect(preserved?.kind).toBe('browse')
    if (preserved?.kind === 'browse') {
      expect(preserved.droppedNotice).toContain('blue_m2')
      const browseView = mount(TableBrowseView, { props: { tab: preserved } })
      expect(browseView.find('[data-testid="browse-dropped-db-banner"]').exists()).toBe(true)
      expect(browseView.text()).toContain('已被删除')
    }

    // CREATE TABLE refreshes ONLY already-loaded (postgres, public) and skips unloaded (unloaded_db, public)
    vi.spyOn(api, 'executeSQL').mockResolvedValueOnce({
      tabId: 'q-ddl',
      execId: 'e2',
      currentDb: 'postgres',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 2,
      statements: [{ index: 0, sql: 'CREATE TABLE new_tbl (id int);', startLine: 1, isResultSet: false, affectedRows: 0, durationMs: 3 }],
      executedAll: true,
      totalMs: 3,
      schemaChanges: {
        changedObjects: [
          { database: 'postgres', schema: 'public' },
          { database: 'unloaded_db', schema: 'public' },
        ],
      },
    })

    await wsStore.executeQuery('q-ddl', { sqlOverride: 'CREATE TABLE new_tbl (id int);', runMode: 'all', cursorOffset: 0 })
    await flushPromises()
    expect(listTablesSpy).toHaveBeenCalledTimes(1)
    expect(listTablesSpy).toHaveBeenCalledWith('pg-ddl', 'postgres', 'public')
  })

  it('returns raw PG 55006 DROP DATABASE error verbatim without modifying tabs or database list', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()
    const wsStore = useWorkspaceStore()

    markConnected(
      connStore,
      makePgConn('pg-55006'),
      makeConnectResult({ connectionId: 'pg-55006', currentDb: 'postgres' }),
    )
    schemaStore.databasesMap['pg-55006'] = [
      { name: 'postgres', isSystem: true },
      { name: 'blue_m2', isSystem: false },
    ]
    wsStore.tabs.push(
      makeQueryTab({ id: 'q-drop', connectionId: 'pg-55006', database: 'postgres', schema: 'public', sql: 'DROP DATABASE blue_m2;' }),
    )

    vi.spyOn(api, 'executeSQL').mockResolvedValue({
      tabId: 'q-drop',
      execId: 'e1',
      currentDb: 'postgres',
      currentSchema: 'public',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 2,
      statements: [],
      totalMs: 5,
      error: {
        code: ErrorCode.ErrInternal,
        message: 'PostgreSQL 错误 (55006): database "blue_m2" is being accessed by other users',
        detail: 'There are 2 other sessions using the database.',
        sqlState: '55006',
        retrySafe: false,
      },
    })

    await wsStore.executeQuery('q-drop', { runMode: 'all', cursorOffset: 0 })
    const qTab = wsStore.tabs[0]
    expect(qTab.kind).toBe('query')
    if (qTab.kind !== 'query') return

    expect(qTab.bannerError?.sqlState).toBe('55006')
    expect(qTab.bannerError?.message).toContain('is being accessed by other users')
    expect(schemaStore.databasesMap['pg-55006'].map((d) => d.name)).toEqual(['postgres', 'blue_m2'])
  })

  it('provides a manual refresh-databases button on multi-DB connection nodes in ExplorerSidebar', async () => {
    const connStore = useConnectionsStore()
    const schemaStore = useSchemaStore()

    markConnected(
      connStore,
      makePgConn('pg-refresh'),
      makeConnectResult({ connectionId: 'pg-refresh', currentDb: 'postgres' }),
    )
    schemaStore.databasesMap['pg-refresh'] = [
      { name: 'postgres', isSystem: true },
      { name: 'old_db', isSystem: false },
    ]

    const listDbsSpy = vi.spyOn(api, 'listDatabases').mockResolvedValue([
      { name: 'postgres', isSystem: true },
      { name: 'fresh_db', isSystem: false },
    ])

    const sidebar = mount(ExplorerSidebar)
    await nextTick()
    const refreshBtn = sidebar.find('[data-testid="refresh-conn-btn"]')
    expect(refreshBtn.exists()).toBe(true)
    expect(refreshBtn.attributes('title')).toBe('刷新数据库列表')

    await refreshBtn.trigger('click')
    await flushPromises()

    expect(listDbsSpy).toHaveBeenCalledWith('pg-refresh')
    expect(schemaStore.databasesMap['pg-refresh'].map((d) => d.name)).toEqual(['postgres', 'fresh_db'])
  })
})
