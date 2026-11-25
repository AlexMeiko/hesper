<script setup lang="ts">
import { computed } from 'vue'
import {
  ChevronLeft,
  ChevronRight,
  Code2,
  Database,
  FileCode2,
  Filter,
  Info,
  RefreshCw,
  ShieldCheck,
  Table2,
} from 'lucide-vue-next'
import { RowIdentityKind } from '@/api/client'
import { useConnectionsStore } from '@/stores/connections'
import { useWorkspaceStore, type BrowseTab } from '@/stores/workspace'
import { formatDuration } from '@/utils/format'
import DataGrid from './DataGrid.vue'
import SessionBanner from './SessionBanner.vue'

const props = defineProps<{
  tab: BrowseTab
}>()

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()

const conn = computed(() => connStore.getConnection(props.tab.connectionId))
const columns = computed(() => props.tab.response?.columns ?? [])
const rows = computed(() => props.tab.response?.rows ?? [])
const rowOffset = computed(() => (props.tab.page - 1) * props.tab.pageSize)

const editabilitySummary = computed(() => {
  const ed = props.tab.response?.editability
  if (!ed) return ''
  const parts: string[] = []
  if (ed.storageEngine) parts.push(ed.storageEngine)
  if (ed.rowIdentity === RowIdentityKind.RowIdentityPrimaryKey) {
    parts.push(`主键(${(ed.keyColumns ?? []).join(', ')})`)
  } else if (ed.rowIdentity === RowIdentityKind.RowIdentityUniqueNotNull) {
    parts.push(`非空唯一键(${(ed.keyColumns ?? []).join(', ')})`)
  } else {
    parts.push('无稳定唯一键')
  }
  if (!ed.batchTransactional) {
    parts.push('非事务引擎')
  }
  if (conn.value?.readOnly) {
    parts.push('只读连接')
  }
  return parts.join(' · ')
})

function applyFilter() {
  props.tab.page = 1
  void wsStore.refreshBrowseTab(props.tab.id)
}

function clearFilter() {
  props.tab.filterWhere = ''
  props.tab.page = 1
  void wsStore.refreshBrowseTab(props.tab.id)
}

function onSort(colName: string) {
  if (props.tab.sortColumn !== colName) {
    props.tab.sortColumn = colName
    props.tab.sortDir = 'asc'
  } else if (props.tab.sortDir === 'asc') {
    props.tab.sortDir = 'desc'
  } else {
    props.tab.sortColumn = ''
    props.tab.sortDir = ''
  }
  props.tab.page = 1
  void wsStore.refreshBrowseTab(props.tab.id)
}

function goPage(next: number) {
  if (next < 1) return
  props.tab.page = next
  void wsStore.refreshBrowseTab(props.tab.id)
}

function onPageSizeChange(ev: Event) {
  const val = Number((ev.target as HTMLSelectElement).value)
  if (val > 0) {
    props.tab.pageSize = val
    props.tab.page = 1
    void wsStore.refreshBrowseTab(props.tab.id)
  }
}

function openDdl() {
  wsStore.openTableDdl({
    connectionId: props.tab.connectionId,
    database: props.tab.database,
    schema: props.tab.schema,
    table: props.tab.table,
  })
}

function openNewQueryFromTable() {
  const fq = props.tab.schema ? `${props.tab.schema}.${props.tab.table}` : props.tab.table
  void wsStore.openQueryTab(
    props.tab.connectionId,
    props.tab.database,
    props.tab.schema,
    `SELECT *\nFROM ${fq}\nLIMIT 100;\n`,
    `查询 ${props.tab.table}`,
  )
}
</script>

<template>
  <div class="flex flex-col flex-1 min-h-0 overflow-hidden">
    <!-- Top Live-Browse Toolbar -->
    <div
      class="px-3 py-2 border-b flex flex-wrap items-center gap-2 shrink-0"
      style="background: var(--bg-surface); border-color: var(--border-subtle)"
    >
      <div class="flex items-center gap-1.5 mr-1">
        <span
          class="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-[11px] font-medium"
          style="background: var(--success-soft); color: var(--success)"
          title="双击表打开实时表浏览：按稳定键排序实时查库，与编辑器一次性快照严格分离"
        >
          <Table2 class="w-3 h-3" />
          实时浏览
        </span>
        <span class="text-xs font-medium" style="color: var(--text-primary)">
          {{ tab.database }}<template v-if="tab.schema">.{{ tab.schema }}</template>.{{ tab.table }}
        </span>
      </div>

      <!-- Quick WHERE Filter -->
      <div class="flex items-center gap-1 flex-1 min-w-[220px] max-w-md">
        <div class="relative flex-1">
          <Filter
            class="w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 pointer-events-none"
            style="color: var(--text-faint)"
          />
          <input
            v-model="tab.filterWhere"
            type="text"
            class="input-field input-field-sm w-full !pl-7 font-mono-tabular"
            placeholder="WHERE 过滤条件，例如 id > 10（按 Enter 应用）"
            @keydown.enter="applyFilter"
          />
        </div>
        <button type="button" class="btn" @click="applyFilter">筛选</button>
        <button v-if="tab.filterWhere" type="button" class="btn btn-ghost" @click="clearFilter">清除</button>
      </div>

      <div class="flex items-center gap-1.5 ml-auto">
        <button
          type="button"
          class="btn"
          :disabled="tab.loading"
          title="刷新当前页"
          @click="wsStore.refreshBrowseTab(tab.id)"
        >
          <RefreshCw class="w-3.5 h-3.5" :class="tab.loading ? 'animate-spin' : ''" />
          刷新
        </button>

        <button
          type="button"
          class="btn"
          :class="tab.showExecutedSql ? '!border-[var(--accent)] !text-[var(--accent-text)]' : ''"
          title="展开/收起本次实时浏览执行的 SQL"
          @click="tab.showExecutedSql = !tab.showExecutedSql"
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
      class="px-3 py-1.5 border-b flex items-center justify-between gap-2 text-xs font-mono-tabular shrink-0"
      style="background: var(--bg-subtle); border-color: var(--border-subtle); color: var(--text-secondary)"
    >
      <span class="truncate select-text" :title="tab.response.executedSql">{{ tab.response.executedSql }}</span>
      <span v-if="tab.response.stableOrderUsed" class="shrink-0 text-[11px]" style="color: var(--text-muted)">
        稳定排序: {{ (tab.response.orderColumns ?? []).join(', ') }}
      </span>
    </div>

    <!-- Error Banner -->
    <div v-if="tab.error" class="p-2.5 shrink-0">
      <SessionBanner :error="tab.error" />
    </div>

    <!-- Pagination & Editability Info Strip -->
    <div
      class="px-3 py-1.5 border-b flex flex-wrap items-center justify-between gap-2 text-xs shrink-0"
      style="background: var(--bg-canvas); border-color: var(--border-subtle); color: var(--text-secondary)"
    >
      <div class="flex items-center gap-2">
        <button
          type="button"
          class="btn"
          :disabled="tab.page <= 1 || tab.loading"
          @click="goPage(tab.page - 1)"
        >
          <ChevronLeft class="w-3.5 h-3.5" />
          上一页
        </button>
        <span class="font-mono-tabular text-xs px-1">第 {{ tab.page }} 页</span>
        <button
          type="button"
          class="btn"
          :disabled="!tab.response?.hasMore || tab.loading"
          @click="goPage(tab.page + 1)"
        >
          下一页
          <ChevronRight class="w-3.5 h-3.5" />
        </button>

        <select
          :value="tab.pageSize"
          class="input-field select-sm ml-1 min-w-[104px]"
          @change="onPageSizeChange"
        >
          <option :value="100">100 行/页</option>
          <option :value="200">200 行/页</option>
          <option :value="500">500 行/页</option>
          <option :value="1000">1000 行/页</option>
        </select>

        <span v-if="tab.response" class="font-mono-tabular text-[11px] ml-2" style="color: var(--text-muted)">
          本页 {{ rows.length }} 行
          <template v-if="tab.response.totalKnown"> · 共 {{ tab.response.totalRows }} 行</template>
          · {{ formatDuration(tab.response.durationMs) }}
        </span>
      </div>

      <div
        v-if="editabilitySummary"
        class="flex items-center gap-1.5 text-[11px]"
        style="color: var(--text-muted)"
        :title="(tab.response?.editability.reasons ?? []).join('；')"
      >
        <ShieldCheck
          v-if="tab.response?.editability.rowIdentity !== RowIdentityKind.RowIdentityNone"
          class="w-3.5 h-3.5"
          style="color: var(--success)"
        />
        <Info v-else class="w-3.5 h-3.5" style="color: var(--warning)" />
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
      :table-name="tab.table"
      :loading="tab.loading"
      @sort="onSort"
      @inspect="(p) => wsStore.inspectCell(p.column, p.cell, p.rowIndex)"
      @copied="(lbl) => connStore.notify('success', lbl)"
    />
  </div>
</template>
