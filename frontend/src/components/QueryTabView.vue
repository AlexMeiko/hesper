<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import {
  AlertTriangle,
  CheckCircle2,
  Clock,
  Database,
  History,
  Layers,
  Play,
  PlayCircle,
  RefreshCw,
  Shield,
  Square,
  X,
  ZapOff,
} from 'lucide-vue-next'
import { EngineType, SessionState } from '@/api/types'
import { useDragResize } from '@/composables/useDragResize'
import { useConnectionsStore } from '@/stores/connections'
import { useSchemaStore } from '@/stores/schema'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore, type QueryTab } from '@/stores/workspace'
import { formatDuration } from '@/utils/format'
import DataGrid, { type GridCopyNotice } from './DataGrid.vue'
import PaginationBar from './PaginationBar.vue'
import SessionBanner from './SessionBanner.vue'
import SqlEditor from './SqlEditor.vue'

const props = withDefaults(
  defineProps<{
    tab: QueryTab
    active?: boolean
  }>(),
  {
    active: true,
  },
)

const connStore = useConnectionsStore()
const schemaStore = useSchemaStore()
const wsStore = useWorkspaceStore()
const uiStore = useUiStore()
const settings = useSettingsStore()

const editorRef = ref<InstanceType<typeof SqlEditor> | null>(null)
const containerRef = ref<HTMLElement | null>(null)
const editorRatio = ref(0.42)
const { dragging: draggingSplit, startDrag } = useDragResize()

// Live running timer
const elapsedMs = ref(0)
let timerHandle: number | null = null

watch(
  () => props.tab.running,
  (running) => {
    if (timerHandle !== null) {
      window.clearInterval(timerHandle)
      timerHandle = null
    }
    if (running) {
      elapsedMs.value = Date.now() - props.tab.startedAt
      timerHandle = window.setInterval(() => {
        elapsedMs.value = Date.now() - props.tab.startedAt
      }, 100)
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  if (timerHandle !== null) window.clearInterval(timerHandle)
})

const conn = computed(() => connStore.getConnection(props.tab.connectionId))
const activeConn = computed(() => connStore.getActive(props.tab.connectionId))
const capabilities = computed(() => activeConn.value?.capabilities)
const dialect = computed<EngineType>(
  () => activeConn.value?.detectedType ?? conn.value?.driver ?? EngineType.EngineMySQL,
)

const databases = computed(() => schemaStore.databasesMap[props.tab.connectionId] ?? [])
const schemas = computed(() => {
  if (!capabilities.value?.hasSchemas || !props.tab.database) return []
  return schemaStore.schemasMap[schemaStore.dbKey(props.tab.connectionId, props.tab.database)] ?? []
})

const completionCatalog = computed(() => {
  const effectiveDb = schemaStore.resolveEffectiveDatabase(props.tab.connectionId, props.tab.database)
  return schemaStore.catalogMap[schemaStore.objKey(props.tab.connectionId, effectiveDb, props.tab.schema)] ?? null
})

const activeStatement = computed(() => {
  if (props.tab.activeResultIdx < 0) return null
  return props.tab.statements[props.tab.activeResultIdx] ?? null
})

const sessionPill = computed(() => {
  switch (props.tab.sessionState) {
    case SessionState.SessionAliveIdle:
      return { label: '会话空闲', dotClass: 'bg-success', pillClass: 'bg-subtle text-ink-secondary' }
    case SessionState.SessionAliveInTx:
      return { label: '事务进行中', dotClass: 'bg-warning', pillClass: 'bg-warning-soft text-warning' }
    case SessionState.SessionAliveFailedTx:
      return { label: '事务已中止', dotClass: 'bg-danger', pillClass: 'bg-danger-soft text-danger' }
    case SessionState.SessionLost:
      return { label: '原会话已结束', dotClass: 'bg-warning', pillClass: 'bg-warning-soft text-warning' }
    case SessionState.SessionCommitUnknown:
      return { label: '提交结果未知', dotClass: 'bg-danger', pillClass: 'bg-danger-soft text-danger' }
    default:
      return { label: '未知', dotClass: 'bg-ink-faint', pillClass: 'bg-subtle text-ink-muted' }
  }
})

function runCurrentFromButton() {
  const ctx = editorRef.value?.getExecutionContext() ?? { runMode: 'current' as const, cursorOffset: 0 }
  void wsStore.executeQuery(props.tab.id, ctx)
}

function runCurrentFromEditor(payload: { sqlOverride?: string; runMode: 'current' | 'selection'; cursorOffset: number }) {
  void wsStore.executeQuery(props.tab.id, payload)
}

function runAll() {
  void wsStore.executeQuery(props.tab.id, { runMode: 'all', cursorOffset: 0 })
}

function runRollback() {
  void wsStore.executeQuery(props.tab.id, {
    sqlOverride: 'ROLLBACK;',
    runMode: 'all',
    cursorOffset: 0,
  })
}

function rerunExpiredStatement() {
  const stmt = activeStatement.value
  if (!stmt) return
  void wsStore.executeQuery(props.tab.id, {
    sqlOverride: stmt.sql,
    runMode: 'all',
    cursorOffset: 0,
  })
}

function goSnapshotPage(nextPage: number) {
  if (props.tab.activeResultIdx < 0 || nextPage < 1) return
  void wsStore.fetchSnapshotPage(props.tab.id, props.tab.activeResultIdx, nextPage)
}

function onSnapshotPageSizeChange(size: number) {
  if (size > 0 && props.tab.activeResultIdx >= 0) {
    void wsStore.fetchSnapshotPage(props.tab.id, props.tab.activeResultIdx, 1, size)
  }
}

function startSplitDrag(ev: MouseEvent) {
  startDrag(ev, {
    axis: 'y',
    onMove: (_delta, moveEv) => {
      const el = containerRef.value
      if (!el) return
      const rect = el.getBoundingClientRect()
      if (rect.height <= 0) return
      const ratio = (moveEv.clientY - rect.top) / rect.height
      editorRatio.value = Math.max(0.18, Math.min(0.78, ratio))
    },
  })
}

function onGridCopied(notice: GridCopyNotice) {
  uiStore.notify(notice.kind, notice.message, notice.detail, notice.detail ? 5500 : 3800)
}
</script>

<template>
  <div class="flex flex-col flex-1 min-h-0 overflow-hidden relative">
    <!-- Query Toolbar -->
    <div class="px-3 py-1.5 border-b border-border bg-surface flex flex-wrap items-center gap-2 shrink-0">
      <!-- Execute / Stop Controls -->
      <div class="flex items-center gap-1.5">
        <button
          v-if="!tab.running"
          type="button"
          class="btn btn-primary"
          :disabled="tab.sessionState === SessionState.SessionCommitUnknown || !tab.sql.trim()"
          title="执行当前光标所在语句或选中片段 (Ctrl+Enter)"
          @click="runCurrentFromButton"
        >
          <Play class="w-3.5 h-3.5 fill-current" />
          运行
        </button>

        <button
          v-if="!tab.running"
          type="button"
          class="btn"
          :disabled="tab.sessionState === SessionState.SessionCommitUnknown || !tab.sql.trim()"
          title="顺序执行编辑器中的全部语句 (Ctrl+Shift+Enter)"
          @click="runAll"
        >
          <PlayCircle class="w-3.5 h-3.5" />
          全部运行
        </button>

        <template v-else>
          <button
            type="button"
            class="btn btn-danger"
            :disabled="tab.cancelPending"
            title="通过旁路通道停止当前正在执行的语句 (Esc / Ctrl+.)"
            @click="wsStore.cancelQuery(tab.id)"
          >
            <Square class="w-3 h-3 fill-current" />
            {{ tab.cancelPending ? '正在取消...' : '停止 (Esc)' }}
          </button>

          <button
            v-if="tab.cancelPending"
            type="button"
            class="btn border-danger text-danger"
            title="强制关闭当前标签页物理连接（原事务与临时表将失效）"
            @click="wsStore.forceAbortQuery(tab.id)"
          >
            <ZapOff class="w-3.5 h-3.5" />
            强制中断会话
          </button>

          <span class="inline-flex items-center gap-1 text-xs font-mono-tabular px-2 text-accent-text">
            <Clock class="w-3.5 h-3.5 animate-pulse" />
            {{ formatDuration(elapsedMs) }}
          </span>
        </template>
      </div>

      <div class="h-4 w-px mx-0.5 bg-border" />

      <!-- Target Database & Schema Switchers -->
      <div class="flex items-center gap-1.5">
        <div v-if="capabilities?.hasDatabases !== false" class="flex items-center gap-1 text-xs text-ink-muted">
          <Database class="w-3.5 h-3.5" />
          <select
            :value="tab.database"
            class="input-field select-sm max-w-[180px]"
            :disabled="tab.running || tab.sessionState === SessionState.SessionCommitUnknown"
            :title="
              capabilities?.switchDbReconnects
                ? '切换 PostgreSQL 数据库将重建当前标签页物理连接'
                : '切换当前标签页会话的默认数据库 (USE)'
            "
            @change="wsStore.switchQueryDatabase(tab.id, ($event.target as HTMLSelectElement).value)"
          >
            <option v-if="!tab.database" value="">选择数据库...</option>
            <option v-for="db in databases" :key="db.name" :value="db.name">
              {{ db.name }}
            </option>
          </select>
        </div>

        <div
          v-else-if="conn"
          class="flex items-center gap-1.5 text-xs px-2 py-0.5 rounded-md border border-border bg-canvas text-ink-secondary"
          :title="conn.params.filePath || conn.name"
        >
          <Database class="w-3.5 h-3.5 shrink-0 text-accent" />
          <span class="font-mono-tabular truncate max-w-[220px]">{{ conn.name }}</span>
        </div>

        <div v-if="capabilities?.hasSchemas" class="flex items-center gap-1 text-xs text-ink-muted">
          <Layers class="w-3.5 h-3.5" />
          <select
            :value="tab.schema"
            class="input-field select-sm max-w-[150px]"
            :disabled="tab.running || tab.sessionState === SessionState.SessionCommitUnknown"
            title="切换当前会话的 Schema (search_path)"
            @change="wsStore.switchQuerySchema(tab.id, ($event.target as HTMLSelectElement).value)"
          >
            <option v-for="sch in schemas" :key="sch" :value="sch">
              {{ sch }}
            </option>
          </select>
        </div>
      </div>

      <!-- Authoritative Session State Pill & Utilities -->
      <div class="flex items-center gap-2 ml-auto">
        <span
          v-if="conn?.envColor === 'red'"
          class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[11px] font-medium bg-danger-soft text-danger"
          title="生产环境连接：执行 DROP / TRUNCATE 或无 WHERE 的 UPDATE / DELETE 前将弹出二次确认"
        >
          生产保护
        </span>

        <span
          v-if="tab.readOnly"
          class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[11px] font-medium bg-warning-soft text-warning"
          :title="
            capabilities?.hasDatabases === false
              ? 'SQLite 引擎级只读打开 (mode=ro + query_only=ON)'
              : '此连接启用了客户端只读保护（生产强只读仍以数据库账号权限为准）'
          "
        >
          <Shield class="w-3 h-3" />
          {{ capabilities?.hasDatabases === false ? '只读文件' : '只读保护' }}
        </span>

        <span
          class="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-[11px] font-medium"
          :class="sessionPill.pillClass"
          :title="tab.serverConnId ? `独立物理连接 ID: #${tab.serverConnId}` : '后端确认的会话状态'"
        >
          <span class="w-2 h-2 rounded-full shrink-0" :class="sessionPill.dotClass" />
          {{ sessionPill.label }}
          <span v-if="tab.serverConnId" class="font-mono-tabular opacity-75">#{{ tab.serverConnId }}</span>
        </span>

        <button
          v-if="tab.sessionState === SessionState.SessionLost"
          type="button"
          class="btn btn-sm"
          @click="wsStore.reconnectQueryTab(tab.id)"
        >
          <RefreshCw class="w-3.5 h-3.5" />
          重连
        </button>

        <button
          type="button"
          class="btn btn-sm"
          :class="tab.historyOpen ? 'btn-active' : ''"
          title="查看当前连接的查询历史"
          @click="wsStore.setQueryHistoryOpen(tab.id, !tab.historyOpen)"
        >
          <History class="w-3.5 h-3.5" />
          历史
        </button>
      </div>
    </div>

    <!-- Split Workspace: Top SQL Editor / Bottom Result Pane -->
    <div ref="containerRef" class="flex-1 min-h-0 flex flex-col overflow-hidden">
      <!-- Top: SQL Editor -->
      <div class="min-h-0 overflow-hidden" :style="{ flex: `0 0 ${(editorRatio * 100).toFixed(2)}%` }">
        <SqlEditor
          ref="editorRef"
          :model-value="tab.sql"
          :dialect="dialect"
          :catalog="completionCatalog"
          :theme="settings.theme"
          :active="active"
          @update:model-value="(val) => wsStore.setQuerySql(tab.id, val)"
          @run-current="runCurrentFromEditor"
          @run-all="runAll"
        />
      </div>

      <!-- Horizontal Drag Splitter -->
      <div
        class="splitter-h"
        :class="draggingSplit ? 'dragging' : ''"
        title="拖动调整编辑器与结果区高度"
        @mousedown="startSplitDrag"
      />

      <!-- Bottom: Results & Status Area -->
      <div class="flex-1 min-h-0 flex flex-col overflow-hidden bg-canvas">
        <!-- Authoritative Session / Error / Expired Snapshot Banners -->
        <div
          v-if="
            tab.bannerError ||
            tab.sessionState === SessionState.SessionLost ||
            tab.sessionState === SessionState.SessionCommitUnknown ||
            tab.sessionState === SessionState.SessionAliveFailedTx ||
            activeStatement?.snapshotExpired ||
            tab.noticeMessage
          "
          class="px-3 pt-2 shrink-0"
        >
          <SessionBanner
            :error="tab.bannerError || activeStatement?.error"
            :session-state="tab.sessionState"
            :snapshot-expired="activeStatement?.snapshotExpired"
            :expired-message="activeStatement?.expiredMessage"
            :notice-message="tab.noticeMessage"
            @reconnect="wsStore.reconnectQueryTab(tab.id)"
            @rollback="runRollback"
            @rerun-expired="rerunExpiredStatement"
          />
        </div>

        <!-- Result Sub-tabs + Execute-Once Snapshot Pagination Bar -->
        <div
          v-if="tab.statements.length > 0"
          class="px-3 py-1.5 border-b border-border bg-surface flex flex-wrap items-center justify-between gap-2 shrink-0"
        >
          <!-- Statement Result Tabs -->
          <div class="flex items-center gap-1 overflow-x-auto">
            <button
              v-for="(st, idx) in tab.statements"
              :key="idx"
              type="button"
              class="btn btn-xs"
              :class="tab.activeResultIdx === idx ? 'btn-active' : 'btn-ghost'"
              @click="wsStore.setQueryActiveResult(tab.id, idx)"
            >
              <AlertTriangle v-if="st.error" class="w-3 h-3 text-danger" />
              <CheckCircle2 v-else class="w-3 h-3 text-success" />
              {{ st.isResultSet ? `结果 ${idx + 1}` : `语句 ${idx + 1}` }}
            </button>

            <button
              type="button"
              class="btn btn-xs"
              :class="tab.activeResultIdx === -1 ? 'btn-active' : 'btn-ghost'"
              @click="wsStore.setQueryActiveResult(tab.id, -1)"
            >
              执行明细 ({{ tab.statements.length }})
            </button>
          </div>

          <!-- Execute-Once Snapshot Pagination Controls -->
          <PaginationBar
            v-if="activeStatement?.isResultSet"
            :page="activeStatement.page ?? 1"
            :page-size="activeStatement.pageSize ?? 200"
            :has-more="Boolean(activeStatement.hasMorePages)"
            :loading="Boolean(activeStatement.pagingLoading)"
            @update:page="goSnapshotPage"
            @update:page-size="onSnapshotPageSizeChange"
          >
            <template #prefix>
              <span
                class="inline-flex items-center px-2 py-0.5 rounded-full text-[11px] font-medium bg-accent-soft text-accent-text"
                title="编辑器 SQL 仅执行一次；翻页只读取本次结果快照，绝不重跑 SQL"
              >
                一次性快照
              </span>
            </template>
            <template #summary>
              <span class="font-mono-tabular text-[11px] text-ink-muted">
                已缓冲 {{ activeStatement.totalBuffered ?? 0 }} 行 · {{ formatDuration(activeStatement.durationMs) }}
              </span>
            </template>
          </PaginationBar>
        </div>

        <!-- Truncation Cap Warning -->
        <div
          v-if="activeStatement?.truncatedByLimit"
          class="px-3 py-1.5 border-b border-warning bg-warning-soft text-warning text-xs flex items-center gap-2 shrink-0"
        >
          <AlertTriangle class="w-3.5 h-3.5 shrink-0" />
          <span>{{ activeStatement.limitReason || '结果已达到单次快照缓存上限并截断，建议加上 LIMIT 或导出。' }}</span>
        </div>

        <!-- Result Grid OR Non-ResultSet Summary OR Execution Log -->
        <template v-if="tab.activeResultIdx === -1">
          <div class="flex-1 min-h-0 overflow-auto p-3 space-y-2">
            <div
              v-for="st in tab.statements"
              :key="st.index"
              class="rounded-lg border border-border bg-surface p-2.5 text-xs space-y-1"
            >
              <div class="flex items-center justify-between gap-2">
                <span class="font-semibold text-ink">语句 #{{ st.index + 1 }}（第 {{ st.startLine }} 行）</span>
                <span class="font-mono-tabular text-[11px] text-ink-muted">
                  {{
                    st.error
                      ? `失败 (${st.error.code})`
                      : st.isResultSet
                        ? `返回 ${st.totalBuffered ?? 0} 行`
                        : `影响 ${st.affectedRows} 行`
                  }}
                  · {{ formatDuration(st.durationMs) }}
                </span>
              </div>
              <pre class="m-0 font-mono-tabular text-[11px] whitespace-pre-wrap break-all opacity-85">{{ st.sql }}</pre>
              <div v-if="st.error" class="font-mono-tabular text-[11px] text-danger">
                {{ st.error.message }} <template v-if="st.error.detail">({{ st.error.detail }})</template>
              </div>
            </div>
          </div>
        </template>

        <template v-else-if="activeStatement?.isResultSet">
          <DataGrid
            :columns="activeStatement.columns ?? []"
            :rows="activeStatement.rows ?? []"
            :row-number-offset="((activeStatement.page ?? 1) - 1) * (activeStatement.pageSize ?? 200)"
            :sortable="false"
            :dialect="dialect"
            :loading="Boolean(activeStatement.pagingLoading)"
            :active="active"
            @inspect="(p) => uiStore.inspectCell(p.column, p.cell, p.rowIndex)"
            @copied="onGridCopied"
          />
        </template>

        <template v-else-if="activeStatement && !activeStatement.error">
          <div class="flex-1 flex flex-col items-center justify-center gap-2 p-6 text-center">
            <CheckCircle2 class="w-7 h-7 text-success" />
            <div class="text-sm font-medium text-ink">
              执行成功 · 影响 {{ activeStatement.affectedRows }} 行
            </div>
            <div class="text-xs font-mono-tabular text-ink-muted">
              耗时 {{ formatDuration(activeStatement.durationMs) }} · 会话状态：{{ sessionPill.label }}
            </div>
          </div>
        </template>

        <template v-else-if="tab.statements.length === 0 && !tab.bannerError">
          <div class="flex-1 flex flex-col items-center justify-center gap-1.5 p-6 text-xs text-ink-muted">
            <div v-if="tab.running" class="flex items-center gap-2">
              <RefreshCw class="w-4 h-4 animate-spin text-accent" />
              <span>正在独立物理会话中执行 SQL，按 Esc 可随时取消...</span>
            </div>
            <template v-else>
              <div>准备就绪 · 每个查询标签页独占一条物理连接</div>
              <div class="opacity-80">
                按 <kbd class="px-1.5 py-0.5 rounded border border-border-strong text-[11px]">Ctrl+Enter</kbd> 运行当前语句，翻页只读快照绝不重跑 SQL
              </div>
            </template>
          </div>
        </template>
      </div>
    </div>

    <!-- Slide-over Query History Drawer -->
    <div
      v-if="tab.historyOpen"
      class="absolute top-0 right-0 bottom-0 w-96 max-w-full border-l border-border-strong bg-elevated shadow-xl z-20 flex flex-col"
    >
      <div class="h-10 px-3 border-b border-border flex items-center justify-between shrink-0">
        <span class="font-semibold text-xs text-ink">最近查询历史（点击填入编辑器）</span>
        <button
          type="button"
          class="btn btn-ghost btn-icon-sm"
          @click="wsStore.setQueryHistoryOpen(tab.id, false)"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="flex-1 overflow-auto p-2 space-y-1.5">
        <div v-if="tab.history.length === 0" class="py-10 text-center text-xs text-ink-muted">
          暂无执行历史记录
        </div>
        <button
          v-for="item in tab.history"
          :key="item.id"
          type="button"
          class="w-full text-left rounded-lg border border-border bg-surface text-ink p-2.5 cursor-pointer transition-colors hover:bg-subtle"
          @click="wsStore.applyHistorySql(tab.id, item.sqlText)"
        >
          <div class="flex items-center justify-between text-[11px] mb-1 text-ink-muted">
            <span class="font-mono-tabular">{{ item.databaseName || '-' }}</span>
            <span class="font-mono-tabular">
              <span
                :class="
                  item.status === 'ok'
                    ? 'text-success'
                    : item.status === 'canceled'
                      ? 'text-accent-text'
                      : 'text-danger'
                "
              >
                {{ item.status }}
              </span>
              · {{ formatDuration(item.durationMs) }}
            </span>
          </div>
          <div class="font-mono-tabular text-xs line-clamp-3 break-all">{{ item.sqlText }}</div>
        </button>
      </div>
    </div>
  </div>
</template>
