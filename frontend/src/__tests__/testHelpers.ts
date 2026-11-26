import { EngineType, SessionState, type ConnectionDTO, type ConnectResult } from '@/api/types'
import type { QueryTab } from '@/stores/workspace'

export function makeQueryTab(overrides: Partial<QueryTab> = {}): QueryTab {
  const sql = overrides.sql ?? ''
  return {
    id: 'q-1',
    kind: 'query',
    title: '查询 1',
    connectionId: 'conn-1',
    database: 'app_db',
    schema: 'public',
    sql,
    cleanSql: overrides.cleanSql ?? sql,
    sessionState: SessionState.SessionAliveIdle,
    sessionEpoch: 0,
    sessionEstablished: true,
    serverConnId: '100',
    readOnly: false,
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
    ...overrides,
  }
}

export function makeConn(overrides: Partial<ConnectionDTO> = {}): ConnectionDTO {
  return {
    id: 'conn-1',
    name: 'prod-pg',
    driver: EngineType.EnginePostgres,
    envColor: 'red',
    readOnly: false,
    params: {
      host: '127.0.0.1',
      port: 5432,
      username: 'postgres',
      defaultDatabase: 'app_db',
      tls: { mode: 'disable' },
    },
    hasPassword: true,
    sortOrder: 0,
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  }
}

export function makeConnectResult(overrides: Partial<ConnectResult> = {}): ConnectResult {
  return {
    connectionId: 'conn-1',
    detectedType: EngineType.EnginePostgres,
    serverVersion: '16.16',
    currentDb: 'app_db',
    capabilities: {
      hasDatabases: true,
      hasSchemas: true,
      switchDbReconnects: true,
      supportsCancel: true,
      supportsViews: true,
      supportsMatViews: true,
      supportsRoutines: true,
      supportsTriggers: true,
      supportsSequences: true,
      supportsSavepoint: true,
      supportsExplain: true,
      ddlTransactional: true,
      supportsSingleStmtAlterAll: true,
    },
    ...overrides,
  }
}

export function makeSqliteConn(id = 'sqlite-1', overrides: Partial<ConnectionDTO> = {}): ConnectionDTO {
  return makeConn({
    id,
    name: 'local.db',
    driver: EngineType.EngineSQLite,
    envColor: 'green',
    params: {
      host: '',
      port: 0,
      username: '',
      defaultDatabase: '',
      filePath: '/tmp/local.db',
      tls: { mode: 'disable' },
    },
    ...overrides,
  })
}

export function makeSqliteConnectResult(
  connectionId = 'sqlite-1',
  overrides: Partial<ConnectResult> = {},
): ConnectResult {
  return makeConnectResult({
    connectionId,
    detectedType: EngineType.EngineSQLite,
    currentDb: 'main',
    capabilities: {
      ...makeConnectResult().capabilities,
      hasDatabases: false,
      hasSchemas: false,
      supportsSequences: false,
    },
    ...overrides,
  })
}

export function mockVirtualViewport(width = 1000, height = 480): () => void {
  const origHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientHeight')
  const origWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth')
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', { configurable: true, value: height })
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, value: width })
  return () => {
    if (origHeight) {
      Object.defineProperty(HTMLElement.prototype, 'clientHeight', origHeight)
    } else {
      delete (HTMLElement.prototype as { clientHeight?: number }).clientHeight
    }
    if (origWidth) {
      Object.defineProperty(HTMLElement.prototype, 'clientWidth', origWidth)
    } else {
      delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth
    }
  }
}
