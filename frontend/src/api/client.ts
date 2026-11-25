import * as ConnBinding from '@bindings/sql-manager/internal/binding/connectionbinding'
import * as SchemaBinding from '@bindings/sql-manager/internal/binding/schemabinding'
import * as QueryBinding from '@bindings/sql-manager/internal/binding/querybinding'
import {
  ErrorCode,
  type AppError,
  type BrowseTableRequest,
  type BrowseTableResponse,
  type CancelExecutionResponse,
  type CompletionCatalog,
  type ConnectionDTO,
  type ConnectResult,
  type DatabaseInfo,
  type ExecuteSQLRequest,
  type ExecuteSQLResponse,
  type FetchResultPageRequest,
  type FetchResultPageResponse,
  type ObjectSummary,
  type QueryHistoryItem,
  type SaveConnectionRequest,
  type TableDetail,
  type TableSummary,
  type TabSessionInfo,
  type TestConnectionResult,
} from '@bindings/sql-manager/internal/dto/models'

export * from '@bindings/sql-manager/internal/dto/models'

/**
 * Extracts a structured AppError from any Wails v3 rejection payload.
 * Wails MarshalError serializes *dto.AppError to JSON; fallback wraps raw text cleanly.
 */
export function parseAppError(err: unknown): AppError {
  if (err && typeof err === 'object') {
    const maybe = err as Partial<AppError>
    if (typeof maybe.code === 'string' && typeof maybe.message === 'string') {
      return {
        code: maybe.code as ErrorCode,
        message: maybe.message,
        detail: maybe.detail,
        sqlState: maybe.sqlState,
        line: maybe.line,
        column: maybe.column,
        position: maybe.position,
        retrySafe: Boolean(maybe.retrySafe),
        sessionState: maybe.sessionState,
      }
    }
    if ('message' in err && typeof (err as { message: unknown }).message === 'string') {
      const msg = (err as { message: string }).message
      const parsed = tryParseAppErrorJSON(msg)
      if (parsed) return parsed
      return {
        code: ErrorCode.ErrInternal,
        message: msg,
        retrySafe: false,
      }
    }
  }
  if (typeof err === 'string') {
    const parsed = tryParseAppErrorJSON(err)
    if (parsed) return parsed
    return {
      code: ErrorCode.ErrInternal,
      message: err,
      retrySafe: false,
    }
  }
  return {
    code: ErrorCode.ErrInternal,
    message: '发生未知错误',
    detail: String(err),
    retrySafe: false,
  }
}

function tryParseAppErrorJSON(raw: string): AppError | null {
  const trimmed = raw.trim()
  const idx = trimmed.indexOf('{')
  if (idx < 0) return null
  try {
    const obj = JSON.parse(trimmed.slice(idx)) as Partial<AppError>
    if (obj && typeof obj.code === 'string' && typeof obj.message === 'string') {
      return {
        code: obj.code as ErrorCode,
        message: obj.message,
        detail: obj.detail,
        sqlState: obj.sqlState,
        line: obj.line,
        column: obj.column,
        position: obj.position,
        retrySafe: Boolean(obj.retrySafe),
        sessionState: obj.sessionState,
      }
    }
  } catch {
    // ignore non-JSON errors
  }
  return null
}

async function call<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn()
  } catch (err) {
    throw parseAppError(err)
  }
}

export const api = {
  // --- Connections ---
  async listConnections(): Promise<ConnectionDTO[]> {
    const res = await call(() => ConnBinding.ListConnections())
    return res ?? []
  },
  saveConnection(req: SaveConnectionRequest): Promise<ConnectionDTO> {
    return call(() => ConnBinding.SaveConnection(req))
  },
  deleteConnection(id: string): Promise<void> {
    return call(() => ConnBinding.DeleteConnection(id))
  },
  testConnection(req: SaveConnectionRequest): Promise<TestConnectionResult> {
    return call(() => ConnBinding.TestConnection(req))
  },
  connect(connID: string): Promise<ConnectResult> {
    return call(() => ConnBinding.Connect(connID))
  },
  disconnect(connID: string): Promise<void> {
    return call(() => ConnBinding.Disconnect(connID))
  },

  // --- Schema / Explorer (Track A) ---
  async listDatabases(connID: string): Promise<DatabaseInfo[]> {
    const res = await call(() => SchemaBinding.ListDatabases(connID))
    return res ?? []
  },
  async listSchemas(connID: string, db: string): Promise<string[]> {
    const res = await call(() => SchemaBinding.ListSchemas(connID, db))
    return res ?? []
  },
  async listTables(connID: string, db: string, schema: string): Promise<TableSummary[]> {
    const res = await call(() => SchemaBinding.ListTables(connID, db, schema))
    return res ?? []
  },
  async listViews(connID: string, db: string, schema: string): Promise<ObjectSummary[]> {
    const res = await call(() => SchemaBinding.ListViews(connID, db, schema))
    return res ?? []
  },
  async listSequences(connID: string, db: string, schema: string): Promise<ObjectSummary[]> {
    const res = await call(() => SchemaBinding.ListSequences(connID, db, schema))
    return res ?? []
  },
  getTableDetail(connID: string, db: string, schema: string, table: string): Promise<TableDetail | null> {
    return call(() => SchemaBinding.GetTableDetail(connID, db, schema, table))
  },
  getTableDDL(connID: string, db: string, schema: string, table: string): Promise<string> {
    return call(() => SchemaBinding.GetTableDDL(connID, db, schema, table))
  },
  fetchCompletionCatalog(connID: string, db: string, schema: string): Promise<CompletionCatalog | null> {
    return call(() => SchemaBinding.FetchCompletionCatalog(connID, db, schema))
  },

  // --- Query & Session (Track A Browse + Track B Dedicated SQL) ---
  openTabSession(tabID: string, connID: string, db: string, schema: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.OpenTabSession(tabID, connID, db, schema))
  },
  switchTabDatabase(tabID: string, targetDB: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.SwitchTabDatabase(tabID, targetDB))
  },
  switchTabSchema(tabID: string, targetSchema: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.SwitchTabSchema(tabID, targetSchema))
  },
  reconnectTabSession(tabID: string, db: string, schema: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.ReconnectTabSession(tabID, db, schema))
  },
  closeTab(tabID: string): Promise<void> {
    return call(() => QueryBinding.CloseTab(tabID))
  },
  getTabSessionInfo(tabID: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.GetTabSessionInfo(tabID))
  },
  browseTable(req: BrowseTableRequest): Promise<BrowseTableResponse | null> {
    return call(() => QueryBinding.BrowseTable(req))
  },
  executeSQL(req: ExecuteSQLRequest): Promise<ExecuteSQLResponse | null> {
    return call(() => QueryBinding.ExecuteSQL(req))
  },
  fetchResultPage(req: FetchResultPageRequest): Promise<FetchResultPageResponse | null> {
    return call(() => QueryBinding.FetchResultPage(req))
  },
  cancelExecution(tabID: string, execID: string): Promise<CancelExecutionResponse> {
    return call(() => QueryBinding.CancelExecution(tabID, execID))
  },
  forceAbortSession(tabID: string): Promise<TabSessionInfo> {
    return call(() => QueryBinding.ForceAbortSession(tabID))
  },
  async listHistory(connID: string, limit = 100): Promise<QueryHistoryItem[]> {
    const res = await call(() => QueryBinding.ListHistory(connID, limit))
    return res ?? []
  },

  // --- Binary / Large-Text Lazy Stream ---
  streamUrl(ref: string, download = false): string {
    const encoded = encodeURIComponent(ref)
    return download ? `/stream/${encoded}?download=1` : `/stream/${encoded}`
  },
}
