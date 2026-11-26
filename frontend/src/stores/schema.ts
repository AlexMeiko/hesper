import { defineStore } from 'pinia'
import { markRaw, ref } from 'vue'
import { api, parseAppError } from '@/api/client'
import type { CompletionCatalog, DatabaseInfo, ObjectSummary, TableSummary } from '@/api/types'
import { useConnectionsStore } from './connections'
import { useUiStore } from './ui'

export interface SchemaObjects {
  tables: TableSummary[]
  views: ObjectSummary[]
  sequences: ObjectSummary[]
  loaded: boolean
  loading: boolean
}

/**
 * Dedicated metadata & explorer cache store (per docs/03).
 * Deduplicates concurrent in-flight requests and guards writes with per-connection epochs
 * and per-key request tokens so stale refreshes or post-disconnect responses never corrupt state.
 */
export const useSchemaStore = defineStore('schema', () => {
  const databasesMap = ref<Record<string, DatabaseInfo[]>>({})
  const schemasMap = ref<Record<string, string[]>>({}) // key: `${connId}::${db}`
  const objectsMap = ref<Record<string, SchemaObjects>>({}) // key: `${connId}::${db}::${schema}`
  const catalogMap = ref<Record<string, CompletionCatalog>>({}) // key: `${connId}::${db}::${schema}`

  // Non-reactive in-flight Promise maps and monotonicity guards
  const inflightDatabases = new Map<string, Promise<DatabaseInfo[] | null>>()
  const inflightSchemas = new Map<string, Promise<string[] | null>>()
  const inflightObjects = new Map<string, Promise<SchemaObjects | null>>()
  const inflightCatalog = new Map<string, Promise<CompletionCatalog | null>>()

  const connEpochs = new Map<string, number>()
  const reqSeqs = new Map<string, number>()

  function getEpoch(connId: string): number {
    return connEpochs.get(connId) ?? 0
  }

  function nextSeq(scopedKey: string): number {
    const next = (reqSeqs.get(scopedKey) ?? 0) + 1
    reqSeqs.set(scopedKey, next)
    return next
  }

  function isCurrent(connId: string, epoch: number, scopedKey: string, seq: number): boolean {
    return getEpoch(connId) === epoch && reqSeqs.get(scopedKey) === seq
  }

  /**
   * Shared in-flight coalescing, epoch/sequence validity check, and own-promise cleanup across
   * all 4 metadata caches. Loaders check `checkCurrent()` and return `null` when invalidated
   * (e.g. after disconnect) or superseded by a newer forced refresh so callers stop cleanly.
   */
  function runDeduped<T>(opts: {
    connId: string
    key: string
    seqPrefix: string
    force: boolean
    inflight: Map<string, Promise<T>>
    run: (checkCurrent: () => boolean) => Promise<T>
  }): Promise<T> {
    if (!opts.force && opts.inflight.has(opts.key)) {
      return opts.inflight.get(opts.key)!
    }

    const epoch = getEpoch(opts.connId)
    const seqKey = `${opts.seqPrefix}::${opts.key}`
    const seq = nextSeq(seqKey)
    const checkCurrent = () => isCurrent(opts.connId, epoch, seqKey, seq)

    let promise!: Promise<T>
    promise = opts.run(checkCurrent).finally(() => {
      if (opts.inflight.get(opts.key) === promise) {
        opts.inflight.delete(opts.key)
      }
    })

    opts.inflight.set(opts.key, promise)
    return promise
  }

  function dbKey(connId: string, db: string): string {
    return `${connId}::${db}`
  }

  function objKey(connId: string, db: string, schema: string): string {
    return `${connId}::${db}::${schema}`
  }

  /**
   * Resolves the effective database identifier from backend-authoritative ConnectResult
   * without scattering engine-specific literals across UI components.
   */
  function resolveEffectiveDatabase(connId: string, db?: string): string {
    if (db) return db
    const connStore = useConnectionsStore()
    const active = connStore.getActive(connId)
    const conn = connStore.getConnection(connId)
    return active?.currentDb || conn?.params.defaultDatabase || ''
  }

  /**
   * Picks a fallback schema from backend-returned schema list when no schema was reported.
   */
  function resolveDefaultSchema(schemas: string[], preferred?: string): string {
    if (preferred) return preferred
    if (schemas.length === 0) return ''
    return schemas.includes('public') ? 'public' : schemas[0]
  }

  function setDatabases(connId: string, list: DatabaseInfo[]) {
    databasesMap.value[connId] = list
  }

  async function loadDatabases(connId: string, force = false): Promise<DatabaseInfo[] | null> {
    if (!force && databasesMap.value[connId]) {
      return databasesMap.value[connId]
    }
    return runDeduped({
      connId,
      key: connId,
      seqPrefix: 'db',
      force,
      inflight: inflightDatabases,
      run: async (checkCurrent) => {
        const list = await api.listDatabases(connId)
        if (!checkCurrent()) return null
        databasesMap.value[connId] = list
        return list
      },
    })
  }

  async function loadSchemas(connId: string, db: string, force = false): Promise<string[] | null> {
    const key = dbKey(connId, db)
    if (!force && schemasMap.value[key]) {
      return schemasMap.value[key]
    }
    return runDeduped({
      connId,
      key,
      seqPrefix: 'sch',
      force,
      inflight: inflightSchemas,
      run: async (checkCurrent) => {
        const list = await api.listSchemas(connId, db)
        if (!checkCurrent()) return null
        schemasMap.value[key] = list
        return list
      },
    })
  }

  async function loadObjects(connId: string, db: string, schema: string, force = false): Promise<SchemaObjects | null> {
    const effectiveDb = resolveEffectiveDatabase(connId, db)
    const key = objKey(connId, effectiveDb, schema)
    const existing = objectsMap.value[key]
    if (existing && existing.loaded && !force) {
      return existing
    }

    const connStore = useConnectionsStore()
    const uiStore = useUiStore()
    const caps = connStore.getActive(connId)?.capabilities

    return runDeduped({
      connId,
      key,
      seqPrefix: 'obj',
      force,
      inflight: inflightObjects,
      run: async (checkCurrent) => {
        objectsMap.value[key] = {
          tables: existing?.tables ?? [],
          views: existing?.views ?? [],
          sequences: existing?.sequences ?? [],
          loaded: existing?.loaded ?? false,
          loading: true,
        }
        try {
          const [tables, views, sequences] = await Promise.all([
            api.listTables(connId, effectiveDb, schema),
            caps?.supportsViews ? api.listViews(connId, effectiveDb, schema) : Promise.resolve([]),
            caps?.supportsSequences ? api.listSequences(connId, effectiveDb, schema) : Promise.resolve([]),
          ])
          if (!checkCurrent()) return null
          const result: SchemaObjects = {
            tables: markRaw(tables),
            views: markRaw(views),
            sequences: markRaw(sequences),
            loaded: true,
            loading: false,
          }
          objectsMap.value[key] = result
          return result
        } catch (err) {
          if (!checkCurrent()) return null
          objectsMap.value[key] = {
            tables: [],
            views: [],
            sequences: [],
            loaded: false,
            loading: false,
          }
          const e = parseAppError(err)
          uiStore.notify('error', '加载对象列表失败', e.message)
          throw e
        }
      },
    })
  }

  async function loadCompletionCatalog(
    connId: string,
    db: string,
    schema: string,
    force = false,
  ): Promise<CompletionCatalog | null> {
    if (!connId) return null
    const connStore = useConnectionsStore()
    const active = connStore.getActive(connId)
    const effectiveDb = resolveEffectiveDatabase(connId, db)
    if (active?.capabilities.hasDatabases !== false && !effectiveDb) {
      return null
    }

    const key = objKey(connId, effectiveDb, schema)
    if (!force && catalogMap.value[key]) {
      return catalogMap.value[key]
    }
    return runDeduped({
      connId,
      key,
      seqPrefix: 'cat',
      force,
      inflight: inflightCatalog,
      run: async (checkCurrent) => {
        try {
          const cat = await api.fetchCompletionCatalog(connId, effectiveDb, schema)
          if (!cat || !checkCurrent()) return null
          catalogMap.value[key] = markRaw(cat)
          return catalogMap.value[key]
        } catch {
          return null
        }
      },
    })
  }

  function clearConnectionCache(connId: string) {
    // Bump connection epoch so any still-in-flight RPC responses are silently discarded
    connEpochs.set(connId, getEpoch(connId) + 1)

    delete databasesMap.value[connId]
    inflightDatabases.delete(connId)

    const prefix = `${connId}::`
    for (const k of Object.keys(schemasMap.value)) {
      if (k.startsWith(prefix)) delete schemasMap.value[k]
    }
    for (const k of Object.keys(objectsMap.value)) {
      if (k.startsWith(prefix)) delete objectsMap.value[k]
    }
    for (const k of Object.keys(catalogMap.value)) {
      if (k.startsWith(prefix)) delete catalogMap.value[k]
    }
    for (const k of inflightSchemas.keys()) {
      if (k.startsWith(prefix)) inflightSchemas.delete(k)
    }
    for (const k of inflightObjects.keys()) {
      if (k.startsWith(prefix)) inflightObjects.delete(k)
    }
    for (const k of inflightCatalog.keys()) {
      if (k.startsWith(prefix)) inflightCatalog.delete(k)
    }
  }

  return {
    databasesMap,
    schemasMap,
    objectsMap,
    catalogMap,
    dbKey,
    objKey,
    resolveEffectiveDatabase,
    resolveDefaultSchema,
    setDatabases,
    loadDatabases,
    loadSchemas,
    loadObjects,
    loadCompletionCatalog,
    clearConnectionCache,
  }
})
