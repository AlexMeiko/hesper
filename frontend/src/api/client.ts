import {
  ErrorCode,
  streamUrl,
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
  type SessionStateEvent,
  type TableDetail,
  type TableSummary,
  type TabSessionInfo,
  type TestConnectionResult,
} from './types'

export * from './types'

// Lazy-load Wails binding modules on first actual RPC invocation so importing stores/api in
// unit tests (where individual api methods are mocked) never initializes @wailsio/runtime.
const connBinding = () => import('@bindings/sql-manager/internal/binding/connectionbinding')
const schemaBinding = () => import('@bindings/sql-manager/internal/binding/schemabinding')
const queryBinding = () => import('@bindings/sql-manager/internal/binding/querybinding')

export function isAppError(err: unknown): err is AppError {
  if (!err || typeof err !== 'object') return false
  const e = err as Partial<AppError>
  return typeof e.code === 'string' && typeof e.message === 'string' && typeof e.retrySafe === 'boolean'
}

function normalizeAppError(obj: Partial<AppError> | null | undefined): AppError | null {
  if (!obj || typeof obj.code !== 'string' || typeof obj.message !== 'string') {
    return null
  }
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
    sessionEpoch: typeof obj.sessionEpoch === 'number' ? obj.sessionEpoch : undefined,
    hostKeyChallenge: obj.hostKeyChallenge ?? undefined,
    sshAuthPrompt: obj.sshAuthPrompt ?? undefined,
    credentialSaved: obj.credentialSaved ? true : undefined,
    saveWarning: obj.saveWarning || undefined,
  }
}

function tryParseAppErrorJSON(raw: string): AppError | null {
  const trimmed = raw.trim()
  const idx = trimmed.indexOf('{')
  if (idx < 0) return null
  try {
    return normalizeAppError(JSON.parse(trimmed.slice(idx)) as Partial<AppError>)
  } catch {
    return null
  }
}

/**
 * Extracts a structured AppError from any Wails v3 rejection payload.
 * Returns already-normalized AppError instances as-is without redundant copying.
 */
export function parseAppError(err: unknown): AppError {
  if (isAppError(err)) {
    return err
  }
  if (err && typeof err === 'object') {
    const normalized = normalizeAppError(err as Partial<AppError>)
    if (normalized) return normalized

    if ('message' in err && typeof (err as { message: unknown }).message === 'string') {
      const msg = (err as { message: string }).message
      return (
        tryParseAppErrorJSON(msg) ?? {
          code: ErrorCode.ErrInternal,
          message: msg,
          retrySafe: false,
        }
      )
    }
  }
  if (typeof err === 'string') {
    return (
      tryParseAppErrorJSON(err) ?? {
        code: ErrorCode.ErrInternal,
        message: err,
        retrySafe: false,
      }
    )
  }
  return {
    code: ErrorCode.ErrInternal,
    message: '发生未知错误',
    detail: String(err),
    retrySafe: false,
  }
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
    return (await call(async () => (await connBinding()).ListConnections())) ?? []
  },
  saveConnection(req: SaveConnectionRequest): Promise<ConnectionDTO> {
    return call(async () => (await connBinding()).SaveConnection(req))
  },
  deleteConnection(id: string): Promise<void> {
    return call(async () => (await connBinding()).DeleteConnection(id))
  },
  testConnection(req: SaveConnectionRequest): Promise<TestConnectionResult> {
    return call(async () => (await connBinding()).TestConnection(req))
  },
  trustSSHHostKey(hostPort: string, algorithm: string, fingerprint: string): Promise<void> {
    return call(async () => (await connBinding()).TrustSSHHostKey(hostPort, algorithm, fingerprint))
  },
  connect(connID: string): Promise<ConnectResult> {
    return call(async () => (await connBinding()).Connect(connID))
  },
  connectWithSSHSecret(connID: string, sshSecret: string, saveSecret: boolean): Promise<ConnectResult> {
    return call(async () => (await connBinding()).ConnectWithSSHSecret(connID, sshSecret, saveSecret))
  },
  disconnect(connID: string): Promise<void> {
    return call(async () => (await connBinding()).Disconnect(connID))
  },
  pickSQLiteFile(): Promise<string> {
    return call(async () => (await connBinding()).PickSQLiteFile())
  },
  pickLocalFile(kind: string): Promise<string> {
    return call(async () => (await connBinding()).PickLocalFile(kind))
  },

  // --- Schema / Explorer (Track A) ---
  async listDatabases(connID: string): Promise<DatabaseInfo[]> {
    return (await call(async () => (await schemaBinding()).ListDatabases(connID))) ?? []
  },
  async listSchemas(connID: string, db: string): Promise<string[]> {
    return (await call(async () => (await schemaBinding()).ListSchemas(connID, db))) ?? []
  },
  async listTables(connID: string, db: string, schema: string): Promise<TableSummary[]> {
    return (await call(async () => (await schemaBinding()).ListTables(connID, db, schema))) ?? []
  },
  async listViews(connID: string, db: string, schema: string): Promise<ObjectSummary[]> {
    return (await call(async () => (await schemaBinding()).ListViews(connID, db, schema))) ?? []
  },
  async listSequences(connID: string, db: string, schema: string): Promise<ObjectSummary[]> {
    return (await call(async () => (await schemaBinding()).ListSequences(connID, db, schema))) ?? []
  },
  getTableDetail(connID: string, db: string, schema: string, table: string): Promise<TableDetail | null> {
    return call(async () => (await schemaBinding()).GetTableDetail(connID, db, schema, table))
  },
  getTableDDL(connID: string, db: string, schema: string, table: string): Promise<string> {
    return call(async () => (await schemaBinding()).GetTableDDL(connID, db, schema, table))
  },
  fetchCompletionCatalog(connID: string, db: string, schema: string): Promise<CompletionCatalog | null> {
    return call(async () => (await schemaBinding()).FetchCompletionCatalog(connID, db, schema))
  },

  // --- Query & Live Browse (Track A Browse + Track B Dedicated Query Session) ---
  openTabSession(tabID: string, connID: string, db: string, schema: string): Promise<TabSessionInfo> {
    return call(async () => (await queryBinding()).OpenTabSession(tabID, connID, db, schema))
  },
  openTabSessionWithSSHSecret(
    tabID: string,
    connID: string,
    db: string,
    schema: string,
    sshSecret: string,
    saveSecret: boolean,
  ): Promise<TabSessionInfo> {
    return call(async () =>
      (await queryBinding()).OpenTabSessionWithSSHSecret(tabID, connID, db, schema, sshSecret, saveSecret),
    )
  },
  switchTabDatabase(tabID: string, targetDB: string): Promise<TabSessionInfo> {
    return call(async () => (await queryBinding()).SwitchTabDatabase(tabID, targetDB))
  },
  switchTabSchema(tabID: string, targetSchema: string): Promise<TabSessionInfo> {
    return call(async () => (await queryBinding()).SwitchTabSchema(tabID, targetSchema))
  },
  reconnectTabSession(tabID: string, db: string, schema: string): Promise<TabSessionInfo> {
    return call(async () => (await queryBinding()).ReconnectTabSession(tabID, db, schema))
  },
  reconnectTabSessionWithSSHSecret(
    tabID: string,
    db: string,
    schema: string,
    sshSecret: string,
    saveSecret: boolean,
  ): Promise<TabSessionInfo> {
    return call(async () =>
      (await queryBinding()).ReconnectTabSessionWithSSHSecret(tabID, db, schema, sshSecret, saveSecret),
    )
  },
  closeTab(tabID: string): Promise<void> {
    return call(async () => (await queryBinding()).CloseTab(tabID))
  },
  browseTable(req: BrowseTableRequest): Promise<BrowseTableResponse | null> {
    return call(async () => (await queryBinding()).BrowseTable(req))
  },
  executeSQL(req: ExecuteSQLRequest): Promise<ExecuteSQLResponse | null> {
    return call(async () => (await queryBinding()).ExecuteSQL(req))
  },
  fetchResultPage(req: FetchResultPageRequest): Promise<FetchResultPageResponse | null> {
    return call(async () => (await queryBinding()).FetchResultPage(req))
  },
  cancelExecution(tabID: string, execID: string): Promise<CancelExecutionResponse> {
    return call(async () => (await queryBinding()).CancelExecution(tabID, execID))
  },
  forceAbortSession(tabID: string): Promise<TabSessionInfo> {
    return call(async () => (await queryBinding()).ForceAbortSession(tabID))
  },
  async listHistory(connID: string, limit = 80): Promise<QueryHistoryItem[]> {
    return (await call(async () => (await queryBinding()).ListHistory(connID, limit))) ?? []
  },

  // --- Background Wails Events ---
  onSessionStateChanged(handler: (evt: SessionStateEvent) => void): () => void {
    // Avoid booting @wailsio/runtime background timers/fetches in headless unit tests.
    if ((import.meta as { env?: { MODE?: string } }).env?.MODE === 'test') {
      return () => {}
    }
    let canceled = false
    let unsub: (() => void) | null = null
    void Promise.all([import('@wailsio/runtime'), import('@bindings/events')])
      .then(([{ Events }, { SessionStateChanged }]) => {
        if (canceled) return
        unsub = Events.On(SessionStateChanged, (ev: { data?: SessionStateEvent }) => {
          if (ev?.data && typeof ev.data.tabId === 'string') {
            handler(ev.data)
          }
        })
      })
      .catch(() => {
        // Ignore in non-Wails preview environments
      })
    return () => {
      canceled = true
      if (unsub) {
        unsub()
        unsub = null
      }
    }
  },

  // --- Binary / Oversized Stream URL ---
  streamUrl,
}
