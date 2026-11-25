import { defineStore } from 'pinia'
import { ref } from 'vue'
import {
  api,
  parseAppError,
  type AppError,
  type CompletionCatalog,
  type ConnectionDTO,
  type ConnectResult,
  type DatabaseInfo,
  type ObjectSummary,
  type SaveConnectionRequest,
  type TableSummary,
} from '@/api/client'

export interface ToastItem {
  id: number
  kind: 'info' | 'success' | 'warning' | 'error'
  message: string
  detail?: string
}

export interface SchemaObjects {
  tables: TableSummary[]
  views: ObjectSummary[]
  sequences: ObjectSummary[]
  loaded: boolean
  loading: boolean
}

export const useConnectionsStore = defineStore('connections', () => {
  const connections = ref<ConnectionDTO[]>([])
  const activeMap = ref<Record<string, ConnectResult>>({})
  const connectingMap = ref<Record<string, boolean>>({})

  // Explorer caches
  const databasesMap = ref<Record<string, DatabaseInfo[]>>({})
  const schemasMap = ref<Record<string, string[]>>({}) // key: `${connId}::${db}`
  const objectsMap = ref<Record<string, SchemaObjects>>({}) // key: `${connId}::${db}::${schema}`
  const catalogMap = ref<Record<string, CompletionCatalog>>({}) // key: `${connId}::${db}::${schema}`

  // Modal state
  const modalOpen = ref(false)
  const editingConnection = ref<ConnectionDTO | null>(null)

  // Toasts
  const toasts = ref<ToastItem[]>([])
  let toastSeq = 1

  function notify(kind: ToastItem['kind'], message: string, detail?: string, ttlMs = 3800) {
    const id = toastSeq++
    toasts.value.push({ id, kind, message, detail })
    window.setTimeout(() => {
      toasts.value = toasts.value.filter((t) => t.id !== id)
    }, ttlMs)
  }

  function dismissToast(id: number) {
    toasts.value = toasts.value.filter((t) => t.id !== id)
  }

  function dbKey(connId: string, db: string) {
    return `${connId}::${db}`
  }

  function objKey(connId: string, db: string, schema: string) {
    return `${connId}::${db}::${schema}`
  }

  async function loadConnections() {
    try {
      connections.value = await api.listConnections()
    } catch (err) {
      const e = parseAppError(err)
      notify('error', '加载连接列表失败', e.message)
    }
  }

  function openNewConnectionModal() {
    editingConnection.value = null
    modalOpen.value = true
  }

  function openEditConnectionModal(conn: ConnectionDTO) {
    editingConnection.value = conn
    modalOpen.value = true
  }

  function closeConnectionModal() {
    modalOpen.value = false
    editingConnection.value = null
  }

  async function saveConnection(req: SaveConnectionRequest): Promise<ConnectionDTO> {
    const saved = await api.saveConnection(req)
    await loadConnections()
    notify('success', req.id ? '连接配置已更新' : '连接配置已保存', saved.name)
    return saved
  }

  async function deleteConnection(id: string) {
    try {
      await api.deleteConnection(id)
      delete activeMap.value[id]
      delete databasesMap.value[id]
      await loadConnections()
      notify('info', '已删除连接配置')
    } catch (err) {
      const e = parseAppError(err)
      notify('error', '删除连接失败', e.message)
    }
  }

  async function connect(connId: string): Promise<ConnectResult | null> {
    if (connectingMap.value[connId]) return activeMap.value[connId] ?? null
    connectingMap.value[connId] = true
    try {
      const res = await api.connect(connId)
      activeMap.value[connId] = res
      await loadDatabases(connId)
      return res
    } catch (err) {
      const e = parseAppError(err)
      notify('error', '连接数据库失败', e.detail ? `${e.message} (${e.detail})` : e.message, 5500)
      return null
    } finally {
      connectingMap.value[connId] = false
    }
  }

  async function disconnect(connId: string) {
    try {
      await api.disconnect(connId)
    } finally {
      delete activeMap.value[connId]
      delete databasesMap.value[connId]
      for (const k of Object.keys(schemasMap.value)) {
        if (k.startsWith(`${connId}::`)) delete schemasMap.value[k]
      }
      for (const k of Object.keys(objectsMap.value)) {
        if (k.startsWith(`${connId}::`)) delete objectsMap.value[k]
      }
      for (const k of Object.keys(catalogMap.value)) {
        if (k.startsWith(`${connId}::`)) delete catalogMap.value[k]
      }
      notify('info', '已断开连接并释放会话与缓存')
    }
  }

  async function loadDatabases(connId: string): Promise<DatabaseInfo[]> {
    const list = await api.listDatabases(connId)
    databasesMap.value[connId] = list
    return list
  }

  async function loadSchemas(connId: string, db: string): Promise<string[]> {
    const key = dbKey(connId, db)
    const list = await api.listSchemas(connId, db)
    schemasMap.value[key] = list
    return list
  }

  async function loadObjects(connId: string, db: string, schema: string, force = false): Promise<SchemaObjects> {
    const key = objKey(connId, db, schema)
    const existing = objectsMap.value[key]
    if (existing && existing.loaded && !force) {
      return existing
    }
    const active = activeMap.value[connId]
    const caps = active?.capabilities
    objectsMap.value[key] = {
      tables: existing?.tables ?? [],
      views: existing?.views ?? [],
      sequences: existing?.sequences ?? [],
      loaded: existing?.loaded ?? false,
      loading: true,
    }
    try {
      const [tables, views, sequences] = await Promise.all([
        api.listTables(connId, db, schema),
        caps?.supportsViews ? api.listViews(connId, db, schema) : Promise.resolve([]),
        caps?.supportsSequences ? api.listSequences(connId, db, schema) : Promise.resolve([]),
      ])
      const result: SchemaObjects = {
        tables,
        views,
        sequences,
        loaded: true,
        loading: false,
      }
      objectsMap.value[key] = result
      return result
    } catch (err) {
      objectsMap.value[key] = {
        tables: [],
        views: [],
        sequences: [],
        loaded: false,
        loading: false,
      }
      const e = parseAppError(err) as AppError
      notify('error', '加载对象列表失败', e.message)
      throw e
    }
  }

  async function loadCompletionCatalog(connId: string, db: string, schema: string): Promise<CompletionCatalog | null> {
    if (!connId || !db) return null
    const key = objKey(connId, db, schema)
    if (catalogMap.value[key]) return catalogMap.value[key]
    try {
      const cat = await api.fetchCompletionCatalog(connId, db, schema)
      if (cat) {
        catalogMap.value[key] = cat
      }
      return cat
    } catch {
      return null
    }
  }

  function getConnection(connId: string): ConnectionDTO | undefined {
    return connections.value.find((c) => c.id === connId)
  }

  function getActive(connId: string): ConnectResult | undefined {
    return activeMap.value[connId]
  }

  return {
    connections,
    activeMap,
    connectingMap,
    databasesMap,
    schemasMap,
    objectsMap,
    catalogMap,
    modalOpen,
    editingConnection,
    toasts,
    notify,
    dismissToast,
    dbKey,
    objKey,
    loadConnections,
    openNewConnectionModal,
    openEditConnectionModal,
    closeConnectionModal,
    saveConnection,
    deleteConnection,
    connect,
    disconnect,
    loadDatabases,
    loadSchemas,
    loadObjects,
    loadCompletionCatalog,
    getConnection,
    getActive,
  }
})
