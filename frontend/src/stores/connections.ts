import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, parseAppError } from '@/api/client'
import type { ConnectionDTO, ConnectResult, SaveConnectionRequest } from '@/api/types'
import { useSchemaStore } from './schema'
import { useUiStore } from './ui'

export const useConnectionsStore = defineStore('connections', () => {
  const connections = ref<ConnectionDTO[]>([])
  const activeMap = ref<Record<string, ConnectResult>>({})
  const connectingMap = ref<Record<string, boolean>>({})

  // Coalesce concurrent connect(connId) calls so clicking Connect then New Query
  // awaits the same in-flight connection instead of returning null.
  const inflightConnects = new Map<string, Promise<ConnectResult | null>>()
  // Monotonic per-connection epoch bumped on every lifecycle transition so stale async callbacks
  // cannot resurrect closed state, overwrite a newer connection, or repopulate cleared caches.
  const connEpochs = new Map<string, number>()
  // Per-connection serialization chain so backend connect / disconnect / delete RPCs for the
  // same connId never overlap or reorder (a late disconnect can never tear down a newer connect).
  const lifecycleChains = new Map<string, Promise<void>>()

  function getConnectionEpoch(connId: string): number {
    return connEpochs.get(connId) ?? 0
  }

  function bumpConnectionEpoch(connId: string): number {
    const next = getConnectionEpoch(connId) + 1
    connEpochs.set(connId, next)
    return next
  }

  function isConnectionCurrent(connId: string, epoch: number): boolean {
    return getConnectionEpoch(connId) === epoch && Boolean(activeMap.value[connId])
  }

  function invalidateConnection(connId: string): number {
    const nextEpoch = bumpConnectionEpoch(connId)
    delete activeMap.value[connId]
    connectingMap.value[connId] = false
    inflightConnects.delete(connId)
    useSchemaStore().clearConnectionCache(connId)
    return nextEpoch
  }

  function enqueueLifecycle<T>(connId: string, task: () => Promise<T>): Promise<T> {
    const prev = lifecycleChains.get(connId)
    const run = () => {
      try {
        return Promise.resolve(task())
      } catch (err) {
        return Promise.reject(err)
      }
    }
    const resultPromise = prev ? prev.then(run, run) : run()
    const tail = resultPromise.then(
      () => undefined,
      () => undefined,
    )
    lifecycleChains.set(connId, tail)
    void tail.finally(() => {
      if (lifecycleChains.get(connId) === tail) {
        lifecycleChains.delete(connId)
      }
    })
    return resultPromise
  }

  async function loadConnections() {
    const uiStore = useUiStore()
    try {
      connections.value = await api.listConnections()
    } catch (err) {
      const e = parseAppError(err)
      uiStore.notify('error', '加载连接列表失败', e.message)
    }
  }

  async function saveConnection(req: SaveConnectionRequest): Promise<ConnectionDTO> {
    const uiStore = useUiStore()
    const saved = await api.saveConnection(req)
    await loadConnections()
    uiStore.notify('success', req.id ? '连接配置已更新' : '连接配置已保存', saved.name)
    return saved
  }

  async function deleteConnection(id: string, opts?: { beforeDelete?: () => Promise<void> }) {
    const uiStore = useUiStore()
    // Immediately invalidate epoch, active state, connecting spinner, and cache BEFORE closing tabs
    // or calling backend RPC so in-flight connect/openQueryTab cannot resurrect tabs or stay spinning.
    invalidateConnection(id)
    try {
      await enqueueLifecycle(id, async () => {
        if (opts?.beforeDelete) {
          await opts.beforeDelete()
        }
        await api.deleteConnection(id)
      })
      await loadConnections()
      uiStore.notify('info', '已删除连接配置')
    } catch (err) {
      const e = parseAppError(err)
      uiStore.notify('error', '删除连接失败', e.message)
    }
  }

  async function connect(connId: string): Promise<ConnectResult | null> {
    if (inflightConnects.has(connId)) {
      return inflightConnects.get(connId)!
    }

    const uiStore = useUiStore()
    const schemaStore = useSchemaStore()
    const epoch = bumpConnectionEpoch(connId)
    connectingMap.value[connId] = true

    let promise!: Promise<ConnectResult | null>
    promise = (async (): Promise<ConnectResult | null> => {
      try {
        const res = await enqueueLifecycle(connId, async () => {
          if (getConnectionEpoch(connId) !== epoch) {
            return null
          }
          return api.connect(connId)
        })
        // If disconnected/deleted while connect() was queued or in flight, abort quietly:
        // the queued disconnect/delete in lifecycleChains handles backend teardown cleanly
        // without issuing a second rogue disconnect against any newer reconnect.
        if (!res || getConnectionEpoch(connId) !== epoch) {
          return null
        }
        activeMap.value[connId] = res
        if (res.capabilities.hasDatabases) {
          await schemaStore.loadDatabases(connId, true)
        } else {
          schemaStore.setDatabases(connId, [])
          await schemaStore.loadObjects(connId, res.currentDb, '')
        }
        if (!isConnectionCurrent(connId, epoch)) {
          return null
        }
        return res
      } catch (err) {
        if (getConnectionEpoch(connId) !== epoch) {
          return null
        }
        const e = parseAppError(err)
        uiStore.notify('error', '连接数据库失败', e.detail ? `${e.message} (${e.detail})` : e.message, 5500)
        return null
      } finally {
        if (inflightConnects.get(connId) === promise) {
          connectingMap.value[connId] = false
          inflightConnects.delete(connId)
        }
      }
    })()

    inflightConnects.set(connId, promise)
    return promise
  }

  async function disconnect(connId: string, opts?: { beforeClose?: () => Promise<void> }) {
    const uiStore = useUiStore()
    const schemaStore = useSchemaStore()
    // Invalidate immediately BEFORE closing tabs or awaiting backend RPC so any concurrent
    // openQueryTab() or connect() aborts cleanly instead of leaving orphan tabs or stale caches.
    const discEpoch = invalidateConnection(connId)
    try {
      await enqueueLifecycle(connId, async () => {
        if (opts?.beforeClose) {
          await opts.beforeClose()
        }
        await api.disconnect(connId)
      })
    } finally {
      // Never wipe state/caches belonging to a newer reconnect that started after this disconnect!
      if (getConnectionEpoch(connId) === discEpoch) {
        delete activeMap.value[connId]
        schemaStore.clearConnectionCache(connId)
        uiStore.notify('info', '已断开连接并释放会话与缓存')
      }
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
    getConnectionEpoch,
    isConnectionCurrent,
    loadConnections,
    saveConnection,
    deleteConnection,
    connect,
    disconnect,
    getConnection,
    getActive,
  }
})
