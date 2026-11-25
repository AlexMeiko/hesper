<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
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
import type { ConnectionDTO, TableSummary } from '@/api/client'
import { useConnectionsStore } from '@/stores/connections'
import { useWorkspaceStore } from '@/stores/workspace'
import { connectionEndpointSummary, engineLabel, envColorHex } from '@/utils/format'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()

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

async function toggleConn(conn: ConnectionDTO) {
  const active = connStore.getActive(conn.id)
  if (!active) {
    const res = await connStore.connect(conn.id)
    if (!res) return
    expandedConns.value[conn.id] = true
    if (res.capabilities.hasDatabases) {
      // Auto-expand default database for multi-database servers
      const dbs = connStore.databasesMap[conn.id] ?? []
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
  const key = connStore.dbKey(connId, dbName)
  expandedDbs.value[key] = true
  const active = connStore.getActive(connId)
  if (active?.capabilities.hasSchemas) {
    const schemas = await connStore.loadSchemas(connId, dbName)
    const defaultSchema = schemas.includes('public') ? 'public' : schemas[0]
    if (defaultSchema) {
      const sKey = connStore.objKey(connId, dbName, defaultSchema)
      expandedSchemas.value[sKey] = true
      await connStore.loadObjects(connId, dbName, defaultSchema)
    }
  } else {
    await connStore.loadObjects(connId, dbName, '')
  }
}

async function toggleDatabase(connId: string, dbName: string) {
  const key = connStore.dbKey(connId, dbName)
  if (expandedDbs.value[key]) {
    expandedDbs.value[key] = false
    return
  }
  await expandDatabase(connId, dbName)
}

async function toggleSchema(connId: string, dbName: string, schemaName: string) {
  const key = connStore.objKey(connId, dbName, schemaName)
  if (expandedSchemas.value[key]) {
    expandedSchemas.value[key] = false
    return
  }
  expandedSchemas.value[key] = true
  await connStore.loadObjects(connId, dbName, schemaName)
}

function visibleDatabases(connId: string) {
  const list = connStore.databasesMap[connId] ?? []
  if (showSystemDbs.value) return list
  const userDbs = list.filter((d) => !d.isSystem)
  return userDbs.length > 0 ? userDbs : list
}

function filterTables(tables: TableSummary[]): TableSummary[] {
  if (!filterQuery.value) return tables
  return tables.filter((t) => t.name.toLowerCase().includes(filterQuery.value))
}

function filterObjectNames<T extends { name: string }>(items: T[]): T[] {
  if (!filterQuery.value) return items
  return items.filter((item) => item.name.toLowerCase().includes(filterQuery.value))
}

async function onDisconnect(connId: string) {
  await wsStore.closeTabsByConnection(connId)
  await connStore.disconnect(connId)
  expandedConns.value[connId] = false
}

async function onDelete(conn: ConnectionDTO) {
  if (!window.confirm(`确定删除连接「${conn.name}」吗？（不会影响目标数据库数据）`)) return
  await wsStore.closeTabsByConnection(conn.id)
  await connStore.deleteConnection(conn.id)
}

function openTableContextMenu(ev: MouseEvent, connId: string, db: string, schema: string, table: string) {
  ev.preventDefault()
  ctxMenu.value = { x: ev.clientX, y: ev.clientY, connId, db, schema, table }
}

function closeContextMenu() {
  ctxMenu.value = null
}

function copyTableName(schema: string, table: string) {
  const full = schema ? `${schema}.${table}` : table
  void navigator.clipboard?.writeText(full)
  connStore.notify('success', `已复制表名 ${full}`)
  closeContextMenu()
}

onMounted(() => window.addEventListener('click', closeContextMenu))
onBeforeUnmount(() => window.removeEventListener('click', closeContextMenu))
</script>

<template>
  <aside
    class="flex flex-col h-full min-h-0 overflow-hidden select-none"
    style="background: var(--bg-surface)"
  >
    <!-- Header & Search Filter -->
    <div class="p-2.5 border-b space-y-2 shrink-0" style="border-color: var(--border-subtle)">
      <div class="flex items-center justify-between gap-2">
        <span class="text-xs font-semibold tracking-wide" style="color: var(--text-secondary)">对象浏览器</span>
        <div class="flex items-center gap-1">
          <button
            type="button"
            class="btn btn-ghost !h-6 !px-1.5 !text-[11px]"
            :class="showSystemDbs ? '!text-[var(--accent-text)]' : ''"
            title="显示/隐藏系统库"
            @click="showSystemDbs = !showSystemDbs"
          >
            {{ showSystemDbs ? '含系统库' : '隐藏系统库' }}
          </button>
          <button
            type="button"
            class="btn btn-ghost !w-6 !h-6 !p-0"
            title="新建数据库连接"
            @click="connStore.openNewConnectionModal()"
          >
            <Plus class="w-3.5 h-3.5" />
          </button>
        </div>
      </div>

      <div class="relative">
        <Search
          class="w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none"
          style="color: var(--text-faint)"
        />
        <input
          v-model="filterText"
          type="text"
          class="input-field input-field-sm w-full !pl-7"
          placeholder="过滤表 / 视图名称..."
        />
      </div>
    </div>

    <!-- Connections Tree -->
    <div class="flex-1 min-h-0 overflow-y-auto p-1.5 space-y-1">
      <div
        v-if="connStore.connections.length === 0"
        class="px-3 py-8 text-center space-y-3"
      >
        <div class="text-xs" style="color: var(--text-muted)">尚未保存任何数据库连接</div>
        <button type="button" class="btn btn-primary w-full" @click="connStore.openNewConnectionModal()">
          <Plus class="w-3.5 h-3.5" />
          新建第一个连接
        </button>
      </div>

      <div v-for="conn in connStore.connections" :key="conn.id" class="space-y-0.5">
        <!-- Connection Node -->
        <div
          class="group flex items-center gap-1.5 px-2 py-1.5 rounded-md cursor-pointer transition-colors hover:bg-[var(--bg-subtle)]"
          @click="toggleConn(conn)"
        >
          <span
            class="w-1.5 h-4 rounded-full shrink-0"
            :style="{ background: envColorHex(conn.envColor) }"
          />

          <Loader2
            v-if="connStore.connectingMap[conn.id]"
            class="w-3.5 h-3.5 shrink-0 animate-spin"
            style="color: var(--accent)"
          />
          <template v-else-if="connStore.activeMap[conn.id]">
            <ChevronDown v-if="expandedConns[conn.id]" class="w-3.5 h-3.5 shrink-0" style="color: var(--text-muted)" />
            <ChevronRight v-else class="w-3.5 h-3.5 shrink-0" style="color: var(--text-muted)" />
          </template>
          <Plug v-else class="w-3.5 h-3.5 shrink-0" style="color: var(--text-faint)" />

          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5 min-w-0">
              <span class="text-xs font-medium truncate" style="color: var(--text-primary)" :title="conn.name">
                {{ conn.name }}
              </span>
              <Shield
                v-if="conn.readOnly"
                class="w-3 h-3 shrink-0"
                style="color: var(--warning)"
                :title="conn.driver === 'sqlite' ? 'SQLite 引擎级只读模式 (mode=ro)' : '已开启客户端只读保护'"
              />
            </div>
            <div
              class="text-[10px] font-mono-tabular truncate"
              style="color: var(--text-faint)"
              :title="conn.driver === 'sqlite' ? conn.params.filePath : `${conn.params.host}:${conn.params.port}`"
            >
              {{ engineLabel(conn.driver) }} · {{ connectionEndpointSummary(conn) }}
              <template v-if="connStore.activeMap[conn.id]">
                · <span style="color: var(--success)">已连接</span>
              </template>
            </div>
          </div>

          <!-- Hover Quick Actions -->
          <div class="hidden group-hover:flex items-center gap-0.5 shrink-0" @click.stop>
            <button
              type="button"
              class="btn btn-ghost !w-6 !h-6 !p-0"
              title="新建 SQL 查询"
              @click="wsStore.openQueryTab(conn.id)"
            >
              <Play class="w-3 h-3" />
            </button>
            <button
              v-if="connStore.activeMap[conn.id] && !connStore.activeMap[conn.id]?.capabilities.hasDatabases"
              type="button"
              class="btn btn-ghost !w-6 !h-6 !p-0"
              title="刷新表与视图"
              @click="connStore.loadObjects(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '', true)"
            >
              <RefreshCw class="w-3 h-3" />
            </button>
            <button
              type="button"
              class="btn btn-ghost !w-6 !h-6 !p-0"
              title="编辑连接"
              @click="connStore.openEditConnectionModal(conn)"
            >
              <Edit3 class="w-3 h-3" />
            </button>
            <button
              v-if="connStore.activeMap[conn.id]"
              type="button"
              class="btn btn-ghost !w-6 !h-6 !p-0"
              title="断开连接"
              @click="onDisconnect(conn.id)"
            >
              <PowerOff class="w-3 h-3" />
            </button>
            <button
              v-else
              type="button"
              class="btn btn-ghost !w-6 !h-6 !p-0"
              title="删除连接"
              @click="onDelete(conn)"
            >
              <Trash2 class="w-3 h-3" />
            </button>
          </div>
        </div>

        <!-- Case 0: Single-File Engine WITHOUT Databases (SQLite) — Tables & Views directly under Connection -->
        <div
          v-if="
            connStore.activeMap[conn.id] &&
            expandedConns[conn.id] &&
            !connStore.activeMap[conn.id]?.capabilities.hasDatabases
          "
          class="pl-5 space-y-0.5"
        >
          <div
            v-if="connStore.objectsMap[connStore.objKey(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '')]?.loading"
            class="px-2 py-1 text-[11px]"
            style="color: var(--text-muted)"
          >
            加载中...
          </div>

          <template v-else>
            <div
              v-for="tbl in filterTables(
                connStore.objectsMap[connStore.objKey(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '')]?.tables ?? [],
              )"
              :key="tbl.name"
              class="group flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
              title="双击实时浏览表数据，右键查看更多操作"
              @dblclick="wsStore.openTableBrowse(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '', tbl.name)"
              @contextmenu="
                openTableContextMenu($event, conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '', tbl.name)
              "
            >
              <Table2 class="w-3.5 h-3.5 shrink-0" style="color: var(--text-muted)" />
              <span class="text-xs truncate flex-1">{{ tbl.name }}</span>
              <button
                type="button"
                class="hidden group-hover:inline-flex btn btn-ghost !w-5 !h-5 !p-0"
                title="查看结构与 DDL"
                @click.stop="
                  wsStore.openTableDdl({
                    connectionId: conn.id,
                    database: connStore.activeMap[conn.id]?.currentDb || 'main',
                    schema: '',
                    table: tbl.name,
                  })
                "
              >
                <FileCode2 class="w-3 h-3" />
              </button>
            </div>

            <div
              v-for="vw in filterObjectNames(
                connStore.objectsMap[connStore.objKey(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '')]?.views ?? [],
              )"
              :key="`v-${vw.name}`"
              class="flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
              title="双击实时浏览视图"
              @dblclick="wsStore.openTableBrowse(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '', vw.name)"
            >
              <Eye class="w-3.5 h-3.5 shrink-0" style="color: var(--accent-text)" />
              <span class="text-xs truncate flex-1" style="color: var(--text-secondary)">{{ vw.name }}</span>
            </div>

            <div
              v-if="
                (connStore.objectsMap[connStore.objKey(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '')]?.tables
                  ?.length ?? 0) === 0 &&
                (connStore.objectsMap[connStore.objKey(conn.id, connStore.activeMap[conn.id]?.currentDb || 'main', '')]?.views
                  ?.length ?? 0) === 0
              "
              class="px-2 py-1 text-[11px]"
              style="color: var(--text-faint)"
            >
              暂无用户表或视图
            </div>
          </template>
        </div>

        <!-- Case 1: Multi-Database Engines (MySQL / MariaDB / PostgreSQL) -->
        <div
          v-else-if="connStore.activeMap[conn.id] && expandedConns[conn.id]"
          class="pl-3.5 space-y-0.5"
        >
          <div
            v-for="db in visibleDatabases(conn.id)"
            :key="db.name"
            class="space-y-0.5"
          >
            <div
              class="group flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
              @click="toggleDatabase(conn.id, db.name)"
            >
              <ChevronDown
                v-if="expandedDbs[connStore.dbKey(conn.id, db.name)]"
                class="w-3 h-3 shrink-0"
                style="color: var(--text-muted)"
              />
              <ChevronRight v-else class="w-3 h-3 shrink-0" style="color: var(--text-muted)" />
              <Database class="w-3.5 h-3.5 shrink-0" style="color: var(--accent)" />
              <span class="text-xs truncate flex-1" :style="{ color: db.isSystem ? 'var(--text-muted)' : 'var(--text-primary)' }">
                {{ db.name }}
              </span>
              <div class="hidden group-hover:flex items-center gap-0.5" @click.stop>
                <button
                  type="button"
                  class="btn btn-ghost !w-5 !h-5 !p-0"
                  title="在此库新建查询"
                  @click="wsStore.openQueryTab(conn.id, db.name)"
                >
                  <Play class="w-3 h-3" />
                </button>
                <button
                  type="button"
                  class="btn btn-ghost !w-5 !h-5 !p-0"
                  title="刷新对象列表"
                  @click="
                    connStore.activeMap[conn.id]?.capabilities.hasSchemas
                      ? connStore.loadSchemas(conn.id, db.name)
                      : connStore.loadObjects(conn.id, db.name, '', true)
                  "
                >
                  <RefreshCw class="w-3 h-3" />
                </button>
              </div>
            </div>

            <!-- Case A: Engine WITH Schemas (PostgreSQL) -->
            <template v-if="expandedDbs[connStore.dbKey(conn.id, db.name)] && connStore.activeMap[conn.id]?.capabilities.hasSchemas">
              <div class="pl-4 space-y-0.5">
                <div
                  v-for="sch in connStore.schemasMap[connStore.dbKey(conn.id, db.name)] ?? []"
                  :key="sch"
                  class="space-y-0.5"
                >
                  <div
                    class="group flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
                    @click="toggleSchema(conn.id, db.name, sch)"
                  >
                    <ChevronDown
                      v-if="expandedSchemas[connStore.objKey(conn.id, db.name, sch)]"
                      class="w-3 h-3 shrink-0"
                      style="color: var(--text-muted)"
                    />
                    <ChevronRight v-else class="w-3 h-3 shrink-0" style="color: var(--text-muted)" />
                    <Layers class="w-3.5 h-3.5 shrink-0" style="color: var(--text-secondary)" />
                    <span class="text-xs truncate flex-1">{{ sch }}</span>
                    <button
                      type="button"
                      class="hidden group-hover:inline-flex btn btn-ghost !w-5 !h-5 !p-0"
                      title="在此 Schema 新建查询"
                      @click.stop="wsStore.openQueryTab(conn.id, db.name, sch)"
                    >
                      <Play class="w-3 h-3" />
                    </button>
                  </div>

                  <!-- Objects under Schema -->
                  <div
                    v-if="expandedSchemas[connStore.objKey(conn.id, db.name, sch)]"
                    class="pl-4 space-y-0.5"
                  >
                    <div
                      v-if="connStore.objectsMap[connStore.objKey(conn.id, db.name, sch)]?.loading"
                      class="px-2 py-1 text-[11px]"
                      style="color: var(--text-muted)"
                    >
                      加载中...
                    </div>

                    <template v-else>
                      <div
                        v-for="tbl in filterTables(connStore.objectsMap[connStore.objKey(conn.id, db.name, sch)]?.tables ?? [])"
                        :key="tbl.name"
                        class="group flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
                        title="双击实时浏览表数据，右键查看更多操作"
                        @dblclick="wsStore.openTableBrowse(conn.id, db.name, sch, tbl.name)"
                        @contextmenu="openTableContextMenu($event, conn.id, db.name, sch, tbl.name)"
                      >
                        <Table2 class="w-3.5 h-3.5 shrink-0" style="color: var(--text-muted)" />
                        <span class="text-xs truncate flex-1">{{ tbl.name }}</span>
                        <button
                          type="button"
                          class="hidden group-hover:inline-flex btn btn-ghost !w-5 !h-5 !p-0"
                          title="查看结构与 DDL"
                          @click.stop="wsStore.openTableDdl({ connectionId: conn.id, database: db.name, schema: sch, table: tbl.name })"
                        >
                          <FileCode2 class="w-3 h-3" />
                        </button>
                      </div>

                      <div
                        v-for="vw in filterObjectNames(connStore.objectsMap[connStore.objKey(conn.id, db.name, sch)]?.views ?? [])"
                        :key="`v-${vw.name}`"
                        class="flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
                        title="双击实时浏览视图"
                        @dblclick="wsStore.openTableBrowse(conn.id, db.name, sch, vw.name)"
                      >
                        <Eye class="w-3.5 h-3.5 shrink-0" style="color: var(--accent-text)" />
                        <span class="text-xs truncate flex-1" style="color: var(--text-secondary)">{{ vw.name }}</span>
                      </div>

                      <div
                        v-for="seq in filterObjectNames(connStore.objectsMap[connStore.objKey(conn.id, db.name, sch)]?.sequences ?? [])"
                        :key="`s-${seq.name}`"
                        class="flex items-center gap-1.5 px-2 py-1 rounded-md"
                        style="color: var(--text-muted)"
                      >
                        <Hash class="w-3 h-3 shrink-0" />
                        <span class="text-[11px] truncate flex-1">{{ seq.name }}</span>
                      </div>
                    </template>
                  </div>
                </div>
              </div>
            </template>

            <!-- Case B: Engine WITHOUT Schemas (MySQL / MariaDB) -->
            <template v-else-if="expandedDbs[connStore.dbKey(conn.id, db.name)]">
              <div class="pl-4 space-y-0.5">
                <div
                  v-if="connStore.objectsMap[connStore.objKey(conn.id, db.name, '')]?.loading"
                  class="px-2 py-1 text-[11px]"
                  style="color: var(--text-muted)"
                >
                  加载中...
                </div>

                <template v-else>
                  <div
                    v-for="tbl in filterTables(connStore.objectsMap[connStore.objKey(conn.id, db.name, '')]?.tables ?? [])"
                    :key="tbl.name"
                    class="group flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
                    title="双击实时浏览表数据，右键查看更多操作"
                    @dblclick="wsStore.openTableBrowse(conn.id, db.name, '', tbl.name)"
                    @contextmenu="openTableContextMenu($event, conn.id, db.name, '', tbl.name)"
                  >
                    <Table2 class="w-3.5 h-3.5 shrink-0" style="color: var(--text-muted)" />
                    <span class="text-xs truncate flex-1">{{ tbl.name }}</span>
                    <span
                      v-if="tbl.engine && tbl.engine.toLowerCase() !== 'innodb'"
                      class="text-[10px] px-1 rounded"
                      style="background: var(--bg-subtle); color: var(--text-faint)"
                    >
                      {{ tbl.engine }}
                    </span>
                    <button
                      type="button"
                      class="hidden group-hover:inline-flex btn btn-ghost !w-5 !h-5 !p-0"
                      title="查看结构与 DDL"
                      @click.stop="wsStore.openTableDdl({ connectionId: conn.id, database: db.name, schema: '', table: tbl.name })"
                    >
                      <FileCode2 class="w-3 h-3" />
                    </button>
                  </div>

                  <div
                    v-for="vw in filterObjectNames(connStore.objectsMap[connStore.objKey(conn.id, db.name, '')]?.views ?? [])"
                    :key="`v-${vw.name}`"
                    class="flex items-center gap-1.5 px-2 py-1 rounded-md cursor-pointer hover:bg-[var(--bg-subtle)]"
                    title="双击实时浏览视图"
                    @dblclick="wsStore.openTableBrowse(conn.id, db.name, '', vw.name)"
                  >
                    <Eye class="w-3.5 h-3.5 shrink-0" style="color: var(--accent-text)" />
                    <span class="text-xs truncate flex-1" style="color: var(--text-secondary)">{{ vw.name }}</span>
                  </div>

                  <!-- MariaDB Sequences (capabilities.supportsSequences && !hasSchemas) -->
                  <div
                    v-for="seq in filterObjectNames(connStore.objectsMap[connStore.objKey(conn.id, db.name, '')]?.sequences ?? [])"
                    :key="`s-${seq.name}`"
                    class="flex items-center gap-1.5 px-2 py-1 rounded-md"
                    style="color: var(--text-muted)"
                    title="MariaDB 序列 (SEQUENCE)"
                  >
                    <Hash class="w-3 h-3 shrink-0" />
                    <span class="text-[11px] truncate flex-1">{{ seq.name }}</span>
                  </div>
                </template>
              </div>
            </template>
          </div>
        </div>
      </div>
    </div>

    <!-- Table Context Menu -->
    <div
      v-if="ctxMenu"
      class="fixed z-50 w-44 rounded-lg border py-1 shadow-xl"
      :style="{
        left: `${ctxMenu.x}px`,
        top: `${ctxMenu.y}px`,
        background: 'var(--bg-elevated)',
        borderColor: 'var(--border-strong)',
      }"
      @click.stop
    >
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] border-0 bg-transparent cursor-pointer"
        style="color: var(--text-primary)"
        @click="wsStore.openTableBrowse(ctxMenu.connId, ctxMenu.db, ctxMenu.schema, ctxMenu.table); closeContextMenu()"
      >
        实时浏览数据
      </button>
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] border-0 bg-transparent cursor-pointer"
        style="color: var(--text-primary)"
        @click="
          wsStore.openTableDdl({
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
        class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] border-0 bg-transparent cursor-pointer"
        style="color: var(--text-primary)"
        @click="
          wsStore.openQueryTab(
            ctxMenu.connId,
            ctxMenu.db,
            ctxMenu.schema,
            `SELECT *\nFROM ${ctxMenu.schema ? `${ctxMenu.schema}.${ctxMenu.table}` : ctxMenu.table}\nLIMIT 100;\n`,
            `查询 ${ctxMenu.table}`,
          );
          closeContextMenu()
        "
      >
        以此表新建查询
      </button>
      <div class="my-1 border-t" style="border-color: var(--border-subtle)" />
      <button
        type="button"
        class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] border-0 bg-transparent cursor-pointer"
        style="color: var(--text-secondary)"
        @click="copyTableName(ctxMenu.schema, ctxMenu.table)"
      >
        复制表名
      </button>
    </div>
  </aside>
</template>
