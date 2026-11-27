<script setup lang="ts">
import { computed } from 'vue'
import { AlertTriangle, Code2, Database, FileCode2, Filter, Info, RefreshCw, ShieldCheck, Table2, X } from 'lucide-vue-next'
import { EngineType, RowIdentityKind } from '@/api/types'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore, type BrowseTab } from '@/stores/workspace'
import { formatDuration } from '@/utils/format'
import { buildQuickSelectSQL, formatEditabilitySummary } from '@/utils/tableMeta'
import DataGrid, { type GridCopyNotice } from './DataGrid.vue'
import PaginationBar from './PaginationBar.vue'
import SessionBanner from './SessionBanner.vue'

const props = withDefaults(
  defineProps<{
    tab: BrowseTab
    active?: boolean
  }>(),
  {
    active: true,
  },
)

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const uiStore = useUiStore()

const conn = computed(() => connStore.getConnection(props.tab.connectionId))
const activeConn = computed(() => connStore.getActive(props.tab.connectionId))
const dialect = computed<EngineType>(
  () => activeConn.value?.detectedType ?? conn.value?.driver ?? EngineType.EngineMySQL,
)
const columns = computed(() => props.tab.response?.columns ?? [])
const rows = computed(() => props.tab.response?.rows ?? [])
const rowOffset = computed(() => (props.tab.page - 1) * props.tab.pageSize)

const editabilitySummary = computed(() =>
  formatEditabilitySummary(props.tab.response?.editability, Boolean(conn.value?.readOnly)),
)

function openDdl() {
  uiStore.openTableDdl({
    connectionId: props.tab.connectionId,
    database: props.tab.database,
    schema: props.tab.schema,
    table: props.tab.table,
  })
}

function openNewQueryFromTable() {
  void wsStore.openQueryTab(
    props.tab.connectionId,
    props.tab.database,
    props.tab.schema,
    buildQuickSelectSQL(props.tab.schema, props.tab.table, dialect.value),
    `查询 ${props.tab.table}`,
  )
}

function onGridCopied(notice: GridCopyNotice) {
  uiStore.notify(notice.kind, notice.message, notice.detail, notice.detail ? 5500 : 3800)
}
</script>

<template>
  <div class="flex flex-col flex-1 min-h-0 overflow-hidden">
    <!-- Top Live-Browse Toolbar -->
    <div class="px-3 py-2 border-b border-border bg-surface flex flex-wrap items-center gap-2 shrink-0">
      <span
        class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium bg-success-soft text-success shrink-0"
        title="双击表打开实时表浏览：按稳定键排序实时查库，与编辑器一次性快照严格分离"
      >
        <Table2 class="w-3.5 h-3.5" />
        实时浏览
      </span>

      <!-- Quick WHERE Filter -->
      <div class="flex items-center gap-1.5 flex-1 min-w-[220px] max-w-md">
        <div class="relative flex-1">
          <Filter class="w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none text-ink-faint" />
          <input
            :value="tab.filterWhere"
            type="text"
            class="input-field input-field-sm w-full pl-8 font-mono-tabular"
            placeholder="WHERE 过滤条件，例如 id > 10 (按 Enter 应用)"
            @input="wsStore.setBrowseFilter(tab.id, ($event.target as HTMLInputElement).value)"
            @keydown.enter="wsStore.applyBrowseFilter(tab.id)"
          />
        </div>
        <button type="button" class="btn" @click="wsStore.applyBrowseFilter(tab.id)">筛选</button>
        <button v-if="tab.filterWhere" type="button" class="btn btn-ghost" @click="wsStore.clearBrowseFilter(tab.id)">
          清除
        </button>
      </div>

      <div class="flex items-center gap-1.5 ml-auto">
        <button
          type="button"
          class="btn"
          :disabled="tab.loading"
          title="刷新当前页 (F5)"
          @click="wsStore.refreshBrowseTab(tab.id)"
        >
          <RefreshCw class="w-3.5 h-3.5" :class="tab.loading ? 'animate-spin' : ''" />
          刷新
        </button>

        <button
          type="button"
          class="btn"
          :class="tab.showExecutedSql ? 'btn-active' : ''"
          title="展开/收起本次实时浏览执行的 SQL"
          @click="wsStore.toggleBrowseExecutedSql(tab.id)"
        >
          <Code2 class="w-3.5 h-3.5" />
          SQL
        </button>

        <button type="button" class="btn" title="查看列定义、索引与 DDL" @click="openDdl">
          <FileCode2 class="w-3.5 h-3.5" />
          结构 / DDL
        </button>

        <button type="button" class="btn" title="以此表为基础新建 SQL 查询标签" @click="openNewQueryFromTable">
          <Database class="w-3.5 h-3.5" />
          新建查询
        </button>
      </div>
    </div>

    <!-- Collapsible Executed SQL Preview -->
    <div
      v-if="tab.showExecutedSql && tab.response?.executedSql"
      class="px-3 py-1.5 border-b border-border bg-subtle text-ink-secondary flex items-center justify-between gap-2 text-xs font-mono-tabular shrink-0"
    >
      <span class="truncate select-text" :title="tab.response.executedSql">{{ tab.response.executedSql }}</span>
      <span v-if="tab.response.stableOrderUsed" class="shrink-0 text-xs text-ink-muted">
        稳定排序: {{ (tab.response.orderColumns ?? []).join(', ') }}
      </span>
    </div>

    <!-- Dropped Database Notice Banner (tab preserved per user preference) -->
    <div
      v-if="tab.droppedNotice"
      data-testid="browse-dropped-db-banner"
      class="px-3 pt-2 shrink-0"
    >
      <div class="flex items-center justify-between gap-3 rounded-lg border border-warning bg-warning-soft text-warning px-3 py-2 text-xs">
        <div class="flex items-center gap-2 min-w-0">
          <AlertTriangle class="w-4 h-4 shrink-0" />
          <span>{{ tab.droppedNotice }}</span>
        </div>
        <button
          type="button"
          class="btn btn-xs shrink-0"
          @click="wsStore.closeTab(tab.id)"
        >
          <X class="w-3 h-3" />
          关闭标签
        </button>
      </div>
    </div>

    <!-- Error Banner -->
    <div v-if="tab.error" class="p-2.5 shrink-0">
      <SessionBanner :error="tab.error" />
    </div>

    <!-- Pagination & Editability Info Strip -->
    <div class="px-3 py-1.5 border-b border-border bg-canvas text-ink-secondary flex flex-wrap items-center justify-between gap-2 text-xs shrink-0">
      <PaginationBar
        :page="tab.page"
        :page-size="tab.pageSize"
        :has-more="Boolean(tab.response?.hasMore)"
        :loading="tab.loading"
        @update:page="(p) => wsStore.setBrowsePage(tab.id, p)"
        @update:page-size="(s) => wsStore.setBrowsePageSize(tab.id, s)"
      >
        <template #summary>
          <span v-if="tab.response" class="font-mono-tabular text-xs ml-1 text-ink-muted">
            本页 {{ rows.length }} 行
            <template v-if="tab.response.totalKnown"> · 共 {{ tab.response.totalRows }} 行</template>
            · {{ formatDuration(tab.response.durationMs) }}
          </span>
        </template>
      </PaginationBar>

      <div
        v-if="editabilitySummary"
        class="flex items-center gap-1.5 text-xs text-ink-muted"
        :title="(tab.response?.editability.reasons ?? []).join('；')"
      >
        <ShieldCheck
          v-if="tab.response?.editability.rowIdentity !== RowIdentityKind.RowIdentityNone"
          class="w-3.5 h-3.5 text-success"
        />
        <Info v-else class="w-3.5 h-3.5 text-warning" />
        <span>{{ editabilitySummary }}</span>
      </div>
    </div>

    <!-- Main Virtualized Data Grid -->
    <DataGrid
      :columns="columns"
      :rows="rows"
      :row-number-offset="rowOffset"
      :sortable="true"
      :sort-column="tab.sortColumn"
      :sort-dir="tab.sortDir"
      :table-target="{ schema: tab.schema, table: tab.table }"
      :dialect="dialect"
      :loading="tab.loading"
      :active="active"
      @sort="(col) => wsStore.toggleBrowseSort(tab.id, col)"
      @inspect="(p) => uiStore.inspectCell(p.column, p.cell, p.rowIndex)"
      @copied="onGridCopied"
    />
  </div>
</template>
