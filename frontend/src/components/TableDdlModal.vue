<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Copy, KeyRound, Loader2 } from 'lucide-vue-next'
import { api, parseAppError } from '@/api/client'
import type { TableDetail } from '@/api/types'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { copyToClipboard } from '@/utils/clipboard'
import { formatEditabilitySummary, formatTablePath } from '@/utils/tableMeta'
import BaseModal from './BaseModal.vue'

const connStore = useConnectionsStore()
const uiStore = useUiStore()

const activeSection = ref<'columns' | 'indexes' | 'ddl'>('columns')
const loading = ref(false)
const detail = ref<TableDetail | null>(null)
const ddl = ref('')
const errorMsg = ref('')

const displayTargetPath = computed(() => {
  const t = uiStore.ddlTarget
  if (!t) return ''
  const active = connStore.getActive(t.connectionId)
  return formatTablePath(t.database, t.schema, t.table, active?.capabilities.hasDatabases !== false)
})

const editabilitySummary = computed(() => {
  const t = uiStore.ddlTarget
  if (!detail.value || !t) return ''
  const conn = connStore.getConnection(t.connectionId)
  return formatEditabilitySummary(detail.value.editability, Boolean(conn?.readOnly))
})

watch(
  () => uiStore.ddlTarget,
  async (target) => {
    detail.value = null
    ddl.value = ''
    errorMsg.value = ''
    activeSection.value = 'columns'
    if (!target) return

    loading.value = true
    try {
      const [det, ddlText] = await Promise.all([
        api.getTableDetail(target.connectionId, target.database, target.schema, target.table),
        api.getTableDDL(target.connectionId, target.database, target.schema, target.table),
      ])
      detail.value = det
      ddl.value = ddlText
    } catch (err) {
      const e = parseAppError(err)
      errorMsg.value = e.detail ? `${e.message} (${e.detail})` : e.message
    } finally {
      loading.value = false
    }
  },
  { immediate: true },
)

async function copyDdl() {
  if (!ddl.value) return
  if (await copyToClipboard(ddl.value)) {
    uiStore.notify('success', '已复制建表 DDL')
  }
}
</script>

<template>
  <BaseModal
    :open="Boolean(uiStore.ddlTarget)"
    :title="`表结构与 DDL · ${displayTargetPath}`"
    :subtitle="editabilitySummary"
    max-width-class="max-w-3xl"
    max-height-class="max-h-[85vh]"
    test-id="table-ddl-modal"
    @close="uiStore.closeTableDdl()"
  >
    <template #toolbar>
      <div class="px-5 py-2 border-b border-border bg-surface flex items-center justify-between gap-2 shrink-0">
        <div class="flex items-center gap-1.5">
          <button
            type="button"
            class="btn btn-sm"
            :class="activeSection === 'columns' ? 'btn-active' : 'btn-ghost'"
            @click="activeSection = 'columns'"
          >
            列定义 ({{ detail?.columns?.length ?? 0 }})
          </button>
          <button
            type="button"
            class="btn btn-sm"
            :class="activeSection === 'indexes' ? 'btn-active' : 'btn-ghost'"
            @click="activeSection = 'indexes'"
          >
            索引 ({{ detail?.indexes?.length ?? 0 }})
          </button>
          <button
            type="button"
            class="btn btn-sm"
            :class="activeSection === 'ddl' ? 'btn-active' : 'btn-ghost'"
            @click="activeSection = 'ddl'"
          >
            DDL 源码
          </button>
        </div>

        <button v-if="activeSection === 'ddl' && ddl" type="button" class="btn btn-sm" @click="copyDdl">
          <Copy class="w-3.5 h-3.5" />
          复制 DDL
        </button>
      </div>
    </template>

    <!-- Content -->
    <div class="flex-1 min-h-0 overflow-auto p-4">
      <div v-if="loading" class="py-16 flex items-center justify-center gap-2 text-xs text-ink-muted">
        <Loader2 class="w-4 h-4 animate-spin" />
        正在读取表元数据...
      </div>

      <div v-else-if="errorMsg" class="rounded-lg border border-danger bg-danger-soft text-danger p-3 text-xs">
        {{ errorMsg }}
      </div>

      <!-- Columns Table -->
      <table v-else-if="activeSection === 'columns' && detail" class="w-full text-xs border-collapse">
        <thead>
          <tr class="border-b border-border text-left text-ink-muted">
            <th class="py-1.5 px-2 font-medium">#</th>
            <th class="py-1.5 px-2 font-medium">列名</th>
            <th class="py-1.5 px-2 font-medium">类型</th>
            <th class="py-1.5 px-2 font-medium">允许 NULL</th>
            <th class="py-1.5 px-2 font-medium">默认值</th>
            <th class="py-1.5 px-2 font-medium">备注</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="col in detail.columns ?? []"
            :key="col.name"
            class="border-b border-border hover:bg-subtle"
          >
            <td class="py-1.5 px-2 font-mono-tabular text-ink-faint">{{ col.ordinalPos }}</td>
            <td class="py-1.5 px-2 font-mono-tabular font-medium text-ink">
              <span class="inline-flex items-center gap-1">
                <KeyRound v-if="col.isPrimaryKey" class="w-3 h-3 text-warning" />
                {{ col.name }}
              </span>
            </td>
            <td class="py-1.5 px-2 font-mono-tabular text-accent-text">{{ col.columnType || col.dataType }}</td>
            <td class="py-1.5 px-2">{{ col.nullable ? 'YES' : 'NO' }}</td>
            <td class="py-1.5 px-2 font-mono-tabular text-ink-secondary">
              {{ col.hasDefault ? col.defaultValue : '-' }}
            </td>
            <td class="py-1.5 px-2 text-ink-muted">{{ col.comment || '' }}</td>
          </tr>
        </tbody>
      </table>

      <!-- Indexes Table -->
      <table v-else-if="activeSection === 'indexes' && detail" class="w-full text-xs border-collapse">
        <thead>
          <tr class="border-b border-border text-left text-ink-muted">
            <th class="py-1.5 px-2 font-medium">索引名</th>
            <th class="py-1.5 px-2 font-medium">包含列</th>
            <th class="py-1.5 px-2 font-medium">属性</th>
            <th class="py-1.5 px-2 font-medium">类型</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="idx in detail.indexes ?? []"
            :key="idx.name"
            class="border-b border-border hover:bg-subtle"
          >
            <td class="py-1.5 px-2 font-mono-tabular font-medium text-ink">{{ idx.name }}</td>
            <td class="py-1.5 px-2 font-mono-tabular">{{ (idx.columns ?? []).join(', ') }}</td>
            <td class="py-1.5 px-2">
              <span v-if="idx.primary" class="px-1.5 py-0.5 rounded text-[10px] bg-warning-soft text-warning">PRIMARY</span>
              <span v-else-if="idx.unique" class="px-1.5 py-0.5 rounded text-[10px] bg-accent-soft text-accent-text">UNIQUE</span>
              <span v-else class="text-ink-muted">INDEX</span>
            </td>
            <td class="py-1.5 px-2 font-mono-tabular text-ink-muted">{{ idx.indexType || '-' }}</td>
          </tr>
        </tbody>
      </table>

      <!-- DDL Code -->
      <pre
        v-else-if="activeSection === 'ddl'"
        class="m-0 p-3 rounded-lg border border-border bg-canvas text-ink font-mono-tabular whitespace-pre-wrap break-all select-text leading-relaxed"
        style="font-size: var(--editor-font-size)"
      >{{ ddl || '-- 无 DDL 信息' }}</pre>
    </div>
  </BaseModal>
</template>
