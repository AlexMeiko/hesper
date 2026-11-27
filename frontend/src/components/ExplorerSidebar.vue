<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useVirtualizer } from '@tanstack/vue-virtual'
import {
  ChevronDown,
  ChevronRight,
  Database,
  Edit3,
  Eye,
  FileCode2,
  Hash,
  Layers,
  Loader2,
  Play,
  Plus,
  Plug,
  PowerOff,
  RefreshCw,
  Search,
  Shield,
  Table2,
  Trash2,
} from 'lucide-vue-next'
import { parseAppError } from '@/api/client'
import type { ConnectionDTO, ConnectResult, DatabaseInfo, ObjectSummary, TableSummary } from '@/api/types'
import { useConnectionsStore } from '@/stores/connections'
import { useSchemaStore } from '@/stores/schema'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { copyToClipboard } from '@/utils/clipboard'
import { connectionEndpointSummary, engineLabel, envColorHex } from '@/utils/format'
import { buildQuickSelectSQL } from '@/utils/tableMeta'

const connStore = useConnectionsStore()
const schemaStore = useSchemaStore()
const wsStore = useWorkspaceStore()
const uiStore = useUiStore()
const settings = useSettingsStore()

const treeScrollRef = ref<HTMLElement | null>(null)
const filterText = ref('')
const expandedConns = ref<Record<string, boolean>>({})
const expandedDbs = ref<Record<string, boolean>>({})
const expandedSchemas = ref<Record<string, boolean>>({})
const showSystemDbs = ref(false)

// Context menu for tables
const ctxMenu = ref<{
  x: number
  y: number
  connId: string
  db: string
  schema: string
  table: string
} | null>(null)

const filterQuery = computed(() => filterText.value.trim().toLowerCase())

type TreeNode =
  | { kind: 'connection'; key: string; depth: 0; conn: ConnectionDTO; active?: ConnectResult }
  | { kind: 'database'; key: string; depth: 1; connId: string; db: DatabaseInfo }
  | { kind: 'schema'; key: string; depth: 2; connId: string; db: string; schema: string }
  | { kind: 'loading'; key: string; depth: number; label: string }
  | { kind: 'empty'; key: string; depth: number; label: string }
  | { kind: 'table'; key: string; depth: number; connId: string; db: string; schema: string; table: TableSummary }
  | { kind: 'view'; key: string; depth: number; connId: string; db: string; schema: string; view: ObjectSummary }
  | { kind: 'sequence'; key: string; depth: number; connId: string; db: string; schema: string; sequence: ObjectSummary }

function visibleDatabases(connId: string): DatabaseInfo[] {
  const list = schemaStore.databasesMap[connId] ?? []
  if (showSystemDbs.value) return list
  const userDbs = list.filter((d) => !d.isSystem)
  return userDbs.length > 0 ? userDbs : list
}

function matchesFilter(name: string): boolean {
  return !filterQuery.value || name.toLowerCase().includes(filterQuery.value)
}

function pushObjectNodes(
  nodes: TreeNode[],
  connId: string,
  db: string,
  schema: string,
  depth: number,
  showEmptyHint: boolean,
) {
  const key = schemaStore.objKey(connId, db, schema)
  const objs = schemaStore.objectsMap[key]

  if (objs?.loading) {
    nodes.push({ kind: 'loading', key: `loading::${key}`, depth, label: '加载中...' })
    return
  }

  const tables = (objs?.tables ?? []).filter((t) => matchesFilter(t.name))
  const views = (objs?.views ?? []).filter((v) => matchesFilter(v.name))
  const sequences = (objs?.sequences ?? []).filter((s) => matchesFilter(s.name))

  for (const table of tables) {
    nodes.push({ kind: 'table', key: `tbl::${key}::${table.name}`, depth, connId, db, schema, table })
  }
  for (const view of views) {
    nodes.push({ kind: 'view', key: `vw::${key}::${view.name}`, depth, connId, db, schema, view })
  }
  for (const sequence of sequences) {
    nodes.push({ kind: 'sequence', key: `seq::${key}::${sequence.name}`, depth, connId, db, schema, sequence })
  }

  if (
    showEmptyHint &&
    !filterQuery.value &&
    objs?.loaded &&
    tables.length === 0 &&
    views.length === 0 &&
    sequences.length === 0
  ) {
    nodes.push({ kind: 'empty', key: `empty::${key}`, depth, label: '暂无用户表或视图' })
  }
}

/**
 * Flattens all expanded connections, databases, schemas, and objects into one reactive array
 * for O(viewport) virtualized rendering via @tanstack/vue-virtual (docs/03).
 */
const flatNodes = computed<TreeNode[]>(() => {
  const nodes: TreeNode[] = []

  for (const conn of connStore.connections) {
    const active = connStore.activeMap[conn.id]
    nodes.push({ kind: 'connection', key: `conn::${conn.id}`, depth: 0, conn, active })

    if (!active || !expandedConns.value[conn.id]) continue

    // Single-file engine without multiple databases (e.g. SQLite)
    if (!active.capabilities.hasDatabases) {
      const db = schemaStore.resolveEffectiveDatabase(conn.id)
      pushObjectNodes(nodes, conn.id, db, '', 1, true)
      continue
    }

    // Multi-database engines (MySQL / MariaDB / PostgreSQL)
    for (const db of visibleDatabases(conn.id)) {
      const dKey = schemaStore.dbKey(conn.id, db.name)
      nodes.push({ kind: 'database', key: `db::${dKey}`, depth: 1, connId: conn.id, db })

      if (!expandedDbs.value[dKey]) continue

      if (active.capabilities.hasSchemas) {
        const schemas = schemaStore.schemasMap[dKey] ?? []
        for (const sch of schemas) {
          const sKey = schemaStore.objKey(conn.id, db.name, sch)
          nodes.push({ kind: 'schema', key: `sch::${sKey}`, depth: 2, connId: conn.id, db: db.name, schema: sch })
          if (expandedSchemas.value[sKey]) {
            pushObjectNodes(nodes, conn.id, db.name, sch, 3, false)
          }
        }
      } else {
        pushObjectNodes(nodes, conn.id, db.name, '', 2, false)
      }
    }
  }

  return nodes
})

const treeVirtualizer = useVirtualizer(
  computed(() => ({
    count: flatNodes.value.length,
    getScrollElement: () => treeScrollRef.value,
    getItemKey: (index: number) => flatNodes.value[index]?.key ?? index,
    estimateSize: (index: number) =>
      flatNodes.value[index]?.kind === 'connection'
        ? settings.densityMetrics.treeConnHeight
        : settings.densityMetrics.treeItemHeight,
    overscan: 16,
    initialRect: { width: 280, height: 600 },
  })),
)

watch([flatNodes, () => settings.fontScale], () => {
  treeVirtualizer.value.measure()
})

const activeBrowseTarget = computed(() => {
  const tab = wsStore.activeTab
  if (!tab || tab.kind !== 'browse') return null
  return {
    tabId: tab.id,
    connId: tab.connectionId,
    db: tab.database,
    schema: tab.schema,
    table: tab.table,
  }
})

function isActiveBrowseNode(node: TreeNode): boolean {
  const target = activeBrowseTarget.value
  if (!target) return false
  if (node.kind !== 'table' && node.kind !== 'view') return false
  const name = node.kind === 'table' ? node.table.name : node.view.name
  if (node.connId !== target.connId || name !== target.table || node.schema !== target.schema) {
    return false
  }
  const active = connStore.getActive(node.connId)
  if (active && !active.capabilities.hasDatabases) {
    return true
  }
  return node.db === target.db
}

function scrollNodeIntoView(idx: number) {
  if (idx < 0) return
  const el = treeScrollRef.value
  const existing = treeVirtualizer.value.getVirtualItems().find((item) => item.index === idx)
  if (el && existing && el.clientHeight > 0) {
    const top = el.scrollTop
    const bottom = top + el.clientHeight
    if (existing.start >= top && existing.end <= bottom) {
      return
    }
  }
  treeVirtualizer.value.scrollToIndex(idx, { align: 'center' })
}

let browseSyncSeq = 0

async function syncExplorerToActiveBrowseTab(target: NonNullable<typeof activeBrowseTarget.value>) {
  const seq = ++browseSyncSeq
  const active = connStore.getActive(target.connId)
  if (!active) return

  expandedConns.value[target.connId] = true

  if (active.capabilities.hasDatabases) {
    const allDbs = schemaStore.databasesMap[target.connId] ?? []
    const dbInfo = allDbs.find((d) => d.name === target.db)
    if (dbInfo?.isSystem && !showSystemDbs.value) {
      showSystemDbs.value = true
    }

    const dKey = schemaStore.dbKey(target.connId, target.db)
    expandedDbs.value[dKey] = true

    if (active.capabilities.hasSchemas) {
      if (!schemaStore.schemasMap[dKey]) {
        const schemas = await schemaStore.loadSchemas(target.connId, target.db)
        if (seq !== browseSyncSeq || !schemas) return
      }
      if (target.schema) {
        const sKey = schemaStore.objKey(target.connId, target.db, target.schema)
        expandedSchemas.value[sKey] = true
        if (!schemaStore.objectsMap[sKey]?.loaded) {
          await schemaStore.loadObjects(target.connId, target.db, target.schema)
          if (seq !== browseSyncSeq) return
        }
      }
    } else {
      const oKey = schemaStore.objKey(target.connId, target.db, '')
      if (!schemaStore.objectsMap[oKey]?.loaded) {
        await schemaStore.loadObjects(target.connId, target.db, '')
        if (seq !== browseSyncSeq) return
      }
    }
  } else {
    const db = schemaStore.resolveEffectiveDatabase(target.connId)
    const oKey = schemaStore.objKey(target.connId, db, '')
    if (!schemaStore.objectsMap[oKey]?.loaded) {
      await schemaStore.loadObjects(target.connId, db, '')
      if (seq !== browseSyncSeq) return
    }
  }

  await nextTick()
  if (seq !== browseSyncSeq) return
  treeVirtualizer.value.measure()
  const idx = flatNodes.value.findIndex((node) => isActiveBrowseNode(node))
  scrollNodeIntoView(idx)
}

watch(
  activeBrowseTarget,
  (target) => {
    if (!target) {
      browseSyncSeq++
      return
    }
    void syncExplorerToActiveBrowseTab(target)
  },
  { immediate: true },
)

const visibleTreeRows = computed(() => {
  const nodes = flatNodes.value
  return treeVirtualizer.value
    .getVirtualItems()
    .flatMap((vItem) => {
      const node = nodes[vItem.index]
      return node ? [{ vItem, node }] : []
    })
})
const totalTreeHeight = computed(() => treeVirtualizer.value.getTotalSize())

function indentPx(depth: number): number {
  return depth === 0 ? 8 : 8 + depth * 14
}

async function toggleConn(conn: ConnectionDTO) {
  const active = connStore.getActive(conn.id)
  if (!active) {
    const res = await connStore.connect(conn.id)
    if (!res || !connStore.getActive(conn.id)) return
    expandedConns.value[conn.id] = true
    if (res.capabilities.hasDatabases) {
      const dbs = schemaStore.databasesMap[conn.id] ?? []
      const defaultDb = res.currentDb || conn.params.defaultDatabase
      const match = dbs.find((d) => d.name === defaultDb) || dbs.find((d) => !d.isSystem)
      if (match) {
        await expandDatabase(conn.id, match.name)
      }
    }
    return
  }
  expandedConns.value[conn.id] = !expandedConns.value[conn.id]
}

async function expandDatabase(connId: string, dbName: string) {
  const key = schemaStore.dbKey(connId, dbName)
  expandedDbs.value[key] = true
  const active = connStore.getActive(connId)
  if (active?.capabilities.hasSchemas) {
    const schemas = await schemaStore.loadSchemas(connId, dbName)
    if (!schemas) return
    const defaultSchema = schemaStore.resolveDefaultSchema(schemas)
    if (defaultSchema) {
      const sKey = schemaStore.objKey(connId, dbName, defaultSchema)
      expandedSchemas.value[sKey] = true
      await schemaStore.loadObjects(connId, dbName, defaultSchema)
    }
  } else {
    await schemaStore.loadObjects(connId, dbName, '')
  }
}

async function toggleDatabase(connId: string, dbName: string) {
  const key = schemaStore.dbKey(connId, dbName)
  if (expandedDbs.value[key]) {
    expandedDbs.value[key] = false
    return
  }
  await expandDatabase(connId, dbName)
}

async function toggleSchema(connId: string, dbName: string, schemaName: string) {
  const key = schemaStore.objKey(connId, dbName, schemaName)
  if (expandedSchemas.value[key]) {
    expandedSchemas.value[key] = false
    return
  }
  expandedSchemas.value[key] = true
  await schemaStore.loadObjects(connId, dbName, schemaName)
}

function refreshConnectionDatabases(connId: string) {
  void schemaStore.loadDatabases(connId, true).catch((err) => {
    const e = parseAppError(err)
    uiStore.notify('error', '刷新数据库列表失败', e.message)
  })
}

async function refreshDatabase(connId: string, dbName: string) {
  const active = connStore.getActive(connId)
  if (active?.capabilities.hasSchemas) {
    try {
      const schemas = await schemaStore.loadSchemas(connId, dbName, true)
      if (!schemas) return
      for (const sch of schemas) {
        const sKey = schemaStore.objKey(connId, dbName, sch)
        if (expandedSchemas.value[sKey]) {
          void schemaStore.loadObjects(connId, dbName, sch, true)
        }
      }
    } catch (err) {
      const e = parseAppError(err)
      uiStore.notify('error', '刷新 Schema 列表失败', e.message)
    }
  } else {
    void schemaStore.loadObjects(connId, dbName, '', true)
  }
}

function refreshSchema(connId: string, dbName: string, schemaName: string) {
  void schemaStore.loadObjects(connId, dbName, schemaName, true)
}

function refreshSingleFileObjects(connId: string) {
  const db = schemaStore.resolveEffectiveDatabase(connId)
  void schemaStore.loadObjects(connId, db, '', true)
}

// Prune stale expanded states whenever databases are dropped/removed from a connection's database list.
watch(
  () => schemaStore.databasesMap,
  (map) => {
    for (const [connId, dbs] of Object.entries(map)) {
      const valid = new Set((dbs ?? []).map((d) => d.name))
      const connPrefix = `${connId}::`
      for (const key of Object.keys(expandedDbs.value)) {
        if (!key.startsWith(connPrefix)) continue
        const dbName = key.slice(connPrefix.length)
        if (!valid.has(dbName)) {
          delete expandedDbs.value[key]
        }
      }
      for (const key of Object.keys(expandedSchemas.value)) {
        if (!key.startsWith(connPrefix)) continue
        const rest = key.slice(connPrefix.length)
        const sep = rest.indexOf('::')
        const dbName = sep >= 0 ? rest.slice(0, sep) : rest
        if (!valid.has(dbName)) {
          delete expandedSchemas.value[key]
        }
      }
    }
  },
  { deep: true },
)

async function onDisconnect(connId: string) {
  if (wsStore.hasDirtyOrRunningTabs(connId)) {
    const conn = connStore.getConnection(connId)
    const confirmed = await uiStore.requestConfirm({
      title: '断开数据库连接',
      message: `连接「${conn?.name ?? connId}」下仍有运行中或未执行的查询标签，断开将立即关闭这些标签并释放会话，确认断开吗？`,
      tone: 'warning',
      confirmLabel: '确认断开',
      cancelLabel: '保留连接',
    })
    if (!confirmed) return
  }
  expandedConns.value[connId] = false
  await connStore.disconnect(connId, {
    beforeClose: () => wsStore.closeTabsByConnection(connId),
  })
}

async function onDelete(conn: ConnectionDTO) {
  const hasDirty = wsStore.hasDirtyOrRunningTabs(conn.id)
  const confirmed = await uiStore.requestConfirm({
    title: '删除连接配置',
    message: hasDirty
      ? `确定删除连接「${conn.name}」吗？该连接下仍有运行中或未执行的查询标签将被一并关闭 (不会影响目标数据库数据)。`
      : `确定删除连接「${conn.name}」吗？ (仅删除本地配置，不会影响目标数据库数据)`,
    tone: 'danger',
    confirmLabel: '删除连接',
    cancelLabel: '取消',
  })
  if (!confirmed) return
  expandedConns.value[conn.id] = false
  await connStore.deleteConnection(conn.id, {
    beforeDelete: () => wsStore.closeTabsByConnection(conn.id),
  })
}

function openTableContextMenu(ev: MouseEvent, connId: string, db: string, schema: string, table: string) {
  ev.preventDefault()
  ctxMenu.value = { x: ev.clientX, y: ev.clientY, connId, db, schema, table }
}

function closeContextMenu() {
  ctxMenu.value = null
}

function openQuickQueryFromMenu() {
  if (!ctxMenu.value) return
  const { connId, db, schema, table } = ctxMenu.value
  const dialect = connStore.getActive(connId)?.detectedType ?? connStore.getConnection(connId)?.driver
  void wsStore.openQueryTab(connId, db, schema, buildQuickSelectSQL(schema, table, dialect), `查询 ${table}`)
  closeContextMenu()
}

async function copyTableName(schema: string, table: string) {
  const full = schema ? `${schema}.${table}` : table
  if (await copyToClipboard(full)) {
    uiStore.notify('success', `已复制表名 ${full}`)
  }
  closeContextMenu()
}

onMounted(() => window.addEventListener('click', closeContextMenu))
onBeforeUnmount(() => window.removeEventListener('click', closeContextMenu))
</script>

<template>
  <aside class="flex flex-col h-full min-h-0 overflow-hidden select-none bg-surface">
    <!-- Header & Search Filter -->
    <div class="p-2.5 border-b border-border space-y-2 shrink-0">
      <div class="flex items-center justify-between gap-2">
        <span class="text-[13px] font-semibold tracking-wide text-ink-secondary">对象浏览器</span>
        <div class="flex items-center gap-1">
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            :class="showSystemDbs ? 'text-accent-text' : ''"
            title="显示/隐藏系统库"
            @click="showSystemDbs = !showSystemDbs"
          >
            {{ showSystemDbs ? '含系统库' : '隐藏系统库' }}
          </button>
          <button
            type="button"
            class="btn btn-ghost btn-icon-sm"
            title="新建数据库连接"
            @click="uiStore.openNewConnectionModal()"
          >
            <Plus class="w-4 h-4" />
          </button>
        </div>
      </div>

      <div class="relative">
        <Search class="w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none text-ink-faint" />
        <input
          v-model="filterText"
          type="text"
          class="input-field input-field-sm w-full pl-8"
          placeholder="过滤表 / 视图名称..."
        />
      </div>
    </div>

    <!-- Virtualized Flattened Connections & Object Tree -->
    <div
      ref="treeScrollRef"
      class="flex-1 min-h-0 overflow-y-auto p-1.5"
      style="font-size: var(--tree-font-size)"
    >
      <div v-if="connStore.connections.length === 0" class="px-3 py-8 text-center space-y-3">
        <div class="text-xs text-ink-muted">尚未保存任何数据库连接</div>
        <button type="button" class="btn btn-primary w-full" @click="uiStore.openNewConnectionModal()">
          <Plus class="w-3.5 h-3.5" />
          新建第一个连接
        </button>
      </div>

      <div v-else class="relative w-full" :style="{ height: `${totalTreeHeight}px` }">
        <div
          v-for="{ vItem, node } in visibleTreeRows"
          :key="node.key"
          class="absolute left-0 right-0 flex items-center"
          :style="{
            height: `${vItem.size}px`,
            transform: `translateY(${vItem.start}px)`,
          }"
        >
          <!-- 1. Connection Node -->
          <div
            v-if="node.kind === 'connection'"
            class="group w-full h-full flex items-center gap-1.5 px-2 rounded-md cursor-pointer transition-colors hover:bg-subtle"
            @click="toggleConn(node.conn)"
          >
            <span class="w-1.5 h-4 rounded-full shrink-0" :style="{ background: envColorHex(node.conn.envColor) }" />

            <Loader2 v-if="connStore.connectingMap[node.conn.id]" class="w-3.5 h-3.5 shrink-0 animate-spin text-accent" />
            <template v-else-if="node.active">
              <ChevronDown v-if="expandedConns[node.conn.id]" class="w-3.5 h-3.5 shrink-0 text-ink-muted" />
              <ChevronRight v-else class="w-3.5 h-3.5 shrink-0 text-ink-muted" />
            </template>
            <Plug v-else class="w-3.5 h-3.5 shrink-0 text-ink-faint" />

            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-1.5 min-w-0">
                <span class="font-medium truncate text-ink" :title="node.conn.name">
                  {{ node.conn.name }}
                </span>
                <Shield
                  v-if="node.conn.readOnly"
                  class="w-3.5 h-3.5 shrink-0 text-warning"
                  :title="node.conn.driver === 'sqlite' ? 'SQLite 引擎级只读模式 (mode=ro)' : '已开启客户端只读保护'"
                />
              </div>
              <div
                class="text-[11px] font-mono-tabular truncate text-ink-faint leading-tight"
                :title="node.conn.driver === 'sqlite' ? node.conn.params.filePath : connectionEndpointSummary(node.conn)"
              >
                {{ engineLabel(node.conn.driver) }} · {{ connectionEndpointSummary(node.conn) }}
                <template v-if="node.active">
                  · <span class="text-success">已连接</span>
                </template>
              </div>
            </div>

            <!-- Hover Quick Actions -->
            <div class="hidden group-hover:flex items-center gap-0.5 shrink-0" @click.stop>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="新建 SQL 查询"
                @click="wsStore.openQueryTab(node.conn.id)"
              >
                <Play class="w-3 h-3" />
              </button>
              <button
                v-if="node.active"
                type="button"
                data-testid="refresh-conn-btn"
                class="btn btn-ghost btn-icon-xs"
                :title="node.active.capabilities.hasDatabases ? '刷新数据库列表' : '刷新表与视图'"
                @click="
                  node.active.capabilities.hasDatabases
                    ? refreshConnectionDatabases(node.conn.id)
                    : refreshSingleFileObjects(node.conn.id)
                "
              >
                <RefreshCw class="w-3 h-3" />
              </button>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="编辑连接"
                @click="uiStore.openEditConnectionModal(node.conn)"
              >
                <Edit3 class="w-3 h-3" />
              </button>
              <button
                v-if="node.active"
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="断开连接"
                @click="onDisconnect(node.conn.id)"
              >
                <PowerOff class="w-3 h-3" />
              </button>
              <button
                v-else
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="删除连接"
                @click="onDelete(node.conn)"
              >
                <Trash2 class="w-3 h-3" />
              </button>
            </div>
          </div>

          <!-- 2. Database Node -->
          <div
            v-else-if="node.kind === 'database'"
            class="group w-full h-full flex items-center gap-1.5 pr-2 rounded-md cursor-pointer hover:bg-subtle"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
            @click="toggleDatabase(node.connId, node.db.name)"
          >
            <ChevronDown
              v-if="expandedDbs[schemaStore.dbKey(node.connId, node.db.name)]"
              class="w-3.5 h-3.5 shrink-0 text-ink-muted"
            />
            <ChevronRight v-else class="w-3.5 h-3.5 shrink-0 text-ink-muted" />
            <Database class="w-3.5 h-3.5 shrink-0 text-accent" />
            <span class="truncate flex-1" :class="node.db.isSystem ? 'text-ink-muted' : 'text-ink'">
              {{ node.db.name }}
            </span>
            <div class="hidden group-hover:flex items-center gap-0.5 shrink-0" @click.stop>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="在此库新建查询"
                @click="wsStore.openQueryTab(node.connId, node.db.name)"
              >
                <Play class="w-3 h-3" />
              </button>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="刷新对象列表"
                @click="refreshDatabase(node.connId, node.db.name)"
              >
                <RefreshCw class="w-3 h-3" />
              </button>
            </div>
          </div>

          <!-- 3. Schema Node (PostgreSQL) -->
          <div
            v-else-if="node.kind === 'schema'"
            class="group w-full h-full flex items-center gap-1.5 pr-2 rounded-md cursor-pointer hover:bg-subtle"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
            @click="toggleSchema(node.connId, node.db, node.schema)"
          >
            <ChevronDown
              v-if="expandedSchemas[schemaStore.objKey(node.connId, node.db, node.schema)]"
              class="w-3.5 h-3.5 shrink-0 text-ink-muted"
            />
            <ChevronRight v-else class="w-3.5 h-3.5 shrink-0 text-ink-muted" />
            <Layers class="w-3.5 h-3.5 shrink-0 text-ink-secondary" />
            <span class="truncate flex-1 text-ink">{{ node.schema }}</span>
            <div class="hidden group-hover:flex items-center gap-0.5 shrink-0" @click.stop>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="在此 Schema 新建查询"
                @click="wsStore.openQueryTab(node.connId, node.db, node.schema)"
              >
                <Play class="w-3 h-3" />
              </button>
              <button
                type="button"
                class="btn btn-ghost btn-icon-xs"
                title="刷新对象列表"
                @click="refreshSchema(node.connId, node.db, node.schema)"
              >
                <RefreshCw class="w-3 h-3" />
              </button>
            </div>
          </div>

          <!-- 4. Table Node (unified across all 4 engines!) -->
          <div
            v-else-if="node.kind === 'table'"
            :data-selected="isActiveBrowseNode(node) ? 'true' : undefined"
            class="group w-full h-full flex items-center gap-1.5 pr-2 rounded-md cursor-pointer transition-colors"
            :class="isActiveBrowseNode(node) ? 'bg-accent-soft text-accent-text font-medium' : 'hover:bg-subtle'"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
            :title="`${node.table.name} - 双击实时浏览表数据，右键查看更多操作`"
            @dblclick="wsStore.openTableBrowse(node.connId, node.db, node.schema, node.table.name)"
            @contextmenu="openTableContextMenu($event, node.connId, node.db, node.schema, node.table.name)"
          >
            <Table2
              class="w-3.5 h-3.5 shrink-0"
              :class="isActiveBrowseNode(node) ? 'text-accent-text' : 'text-ink-muted'"
            />
            <span class="truncate flex-1" :class="isActiveBrowseNode(node) ? 'text-accent-text' : 'text-ink'">
              {{ node.table.name }}
            </span>
            <span
              v-if="node.table.engine && node.table.engine.toLowerCase() !== 'innodb'"
              class="text-[11px] px-1 rounded bg-subtle text-ink-faint shrink-0"
            >
              {{ node.table.engine }}
            </span>
            <button
              type="button"
              class="hidden group-hover:inline-flex btn btn-ghost btn-icon-xs shrink-0"
              title="查看结构与 DDL"
              @click.stop="
                uiStore.openTableDdl({
                  connectionId: node.connId,
                  database: node.db,
                  schema: node.schema,
                  table: node.table.name,
                })
              "
            >
              <FileCode2 class="w-3 h-3" />
            </button>
          </div>

          <!-- 5. View Node -->
          <div
            v-else-if="node.kind === 'view'"
            :data-selected="isActiveBrowseNode(node) ? 'true' : undefined"
            class="w-full h-full flex items-center gap-1.5 pr-2 rounded-md cursor-pointer transition-colors"
            :class="isActiveBrowseNode(node) ? 'bg-accent-soft text-accent-text font-medium' : 'hover:bg-subtle'"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
            :title="`${node.view.name} - 双击实时浏览视图`"
            @dblclick="wsStore.openTableBrowse(node.connId, node.db, node.schema, node.view.name)"
          >
            <Eye class="w-3.5 h-3.5 shrink-0 text-accent-text" />
            <span class="truncate flex-1" :class="isActiveBrowseNode(node) ? 'text-accent-text' : 'text-ink-secondary'">
              {{ node.view.name }}
            </span>
          </div>

          <!-- 6. Sequence Node -->
          <div
            v-else-if="node.kind === 'sequence'"
            class="w-full h-full flex items-center gap-1.5 pr-2 rounded-md text-ink-muted"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
            title="序列 (SEQUENCE)"
          >
            <Hash class="w-3.5 h-3.5 shrink-0" />
            <span class="text-xs truncate flex-1">{{ node.sequence.name }}</span>
          </div>

          <!-- 7. Loading / Empty Placeholder Node -->
          <div
            v-else
            class="w-full h-full flex items-center pr-2 text-xs"
            :class="node.kind === 'loading' ? 'text-ink-muted' : 'text-ink-faint'"
            :style="{ paddingLeft: `${indentPx(node.depth)}px` }"
          >
            {{ node.label }}
          </div>
        </div>
      </div>
    </div>

    <!-- Table Context Menu -->
    <div
      v-if="ctxMenu"
      class="fixed z-50 w-44 rounded-lg border border-border-strong bg-elevated py-1 shadow-xl"
      :style="{ left: `${ctxMenu.x}px`, top: `${ctxMenu.y}px` }"
      @click.stop
    >
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle border-0 bg-transparent cursor-pointer"
        @click="wsStore.openTableBrowse(ctxMenu.connId, ctxMenu.db, ctxMenu.schema, ctxMenu.table); closeContextMenu()"
      >
        实时浏览数据
      </button>
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle border-0 bg-transparent cursor-pointer"
        @click="
          uiStore.openTableDdl({
            connectionId: ctxMenu.connId,
            database: ctxMenu.db,
            schema: ctxMenu.schema,
            table: ctxMenu.table,
          });
          closeContextMenu()
        "
      >
        查看表结构与 DDL
      </button>
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle border-0 bg-transparent cursor-pointer"
        @click="openQuickQueryFromMenu"
      >
        以此表新建查询
      </button>
      <div class="my-1 border-t border-border" />
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs text-ink-secondary hover:bg-subtle border-0 bg-transparent cursor-pointer"
        @click="copyTableName(ctxMenu.schema, ctxMenu.table)"
      >
        复制表名
      </button>
    </div>
  </aside>
</template>
