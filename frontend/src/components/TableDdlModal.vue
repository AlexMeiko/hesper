<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Copy, KeyRound, Loader2, X } from 'lucide-vue-next'
import { api, parseAppError, RowIdentityKind, type TableDetail } from '@/api/client'
import { useConnectionsStore } from '@/stores/connections'
import { useWorkspaceStore } from '@/stores/workspace'

const wsStore = useWorkspaceStore()
const connStore = useConnectionsStore()

const activeSection = ref<'columns' | 'indexes' | 'ddl'>('columns')
const loading = ref(false)
const detail = ref<TableDetail | null>(null)
const ddl = ref('')
const errorMsg = ref('')

const displayTargetPath = computed(() => {
  const t = wsStore.ddlTarget
  if (!t) return ''
  const active = connStore.getActive(t.connectionId)
  const parts: string[] = []
  if (active?.capabilities.hasDatabases !== false && t.database) {
    parts.push(t.database)
  }
  if (t.schema) parts.push(t.schema)
  parts.push(t.table)
  return parts.join('.')
})

watch(
  () => wsStore.ddlTarget,
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
  try {
    await navigator.clipboard.writeText(ddl.value)
    connStore.notify('success', '已复制建表 DDL')
  } catch {
    // ignore
  }
}
</script>

<template>
  <div
    v-if="wsStore.ddlTarget"
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    style="background: rgba(15, 23, 42, 0.45); backdrop-filter: blur(2px)"
    @click.self="wsStore.closeTableDdl()"
  >
    <div
      class="w-full max-w-3xl rounded-xl border shadow-2xl overflow-hidden flex flex-col max-h-[85vh]"
      style="background: var(--bg-elevated); border-color: var(--border-strong)"
    >
      <div
        class="px-5 py-3 border-b flex items-center justify-between"
        style="border-color: var(--border-subtle)"
      >
        <div class="space-y-0.5 min-w-0">
          <div class="font-semibold text-sm truncate" style="color: var(--text-primary)">
            表结构与 DDL · {{ displayTargetPath }}
          </div>
          <div v-if="detail" class="text-[11px] flex items-center gap-2" style="color: var(--text-muted)">
            <span v-if="detail.engine">存储引擎: {{ detail.engine }}</span>
            <span>
              行定位键:
              {{
                detail.editability.rowIdentity === RowIdentityKind.RowIdentityPrimaryKey
                  ? `主键 (${(detail.editability.keyColumns ?? []).join(', ')})`
                  : detail.editability.rowIdentity === RowIdentityKind.RowIdentityUniqueNotNull
                    ? `非空唯一键 (${(detail.editability.keyColumns ?? []).join(', ')})`
                    : '无稳定唯一键'
              }}
            </span>
            <span>事务能力: {{ detail.editability.batchTransactional ? '支持事务回滚' : '非事务引擎' }}</span>
          </div>
        </div>
        <button type="button" class="btn btn-ghost !w-7 !h-7 !p-0" @click="wsStore.closeTableDdl()">
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Tabs -->
      <div
        class="px-5 py-2 border-b flex items-center justify-between gap-2 shrink-0"
        style="background: var(--bg-surface); border-color: var(--border-subtle)"
      >
        <div class="flex items-center gap-1.5">
          <button
            type="button"
            class="btn !h-7"
            :class="activeSection === 'columns' ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]' : 'btn-ghost'"
            @click="activeSection = 'columns'"
          >
            列定义 ({{ detail?.columns?.length ?? 0 }})
          </button>
          <button
            type="button"
            class="btn !h-7"
            :class="activeSection === 'indexes' ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]' : 'btn-ghost'"
            @click="activeSection = 'indexes'"
          >
            索引 ({{ detail?.indexes?.length ?? 0 }})
          </button>
          <button
            type="button"
            class="btn !h-7"
            :class="activeSection === 'ddl' ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]' : 'btn-ghost'"
            @click="activeSection = 'ddl'"
          >
            DDL 源码
          </button>
        </div>

        <button v-if="activeSection === 'ddl' && ddl" type="button" class="btn !h-7" @click="copyDdl">
          <Copy class="w-3.5 h-3.5" />
          复制 DDL
        </button>
      </div>

      <!-- Content -->
      <div class="flex-1 min-h-0 overflow-auto p-4">
        <div v-if="loading" class="py-16 flex items-center justify-center gap-2 text-xs" style="color: var(--text-muted)">
          <Loader2 class="w-4 h-4 animate-spin" />
          正在读取表元数据...
        </div>

        <div
          v-else-if="errorMsg"
          class="rounded-lg border p-3 text-xs"
          style="background: var(--danger-soft); border-color: var(--danger); color: var(--danger)"
        >
          {{ errorMsg }}
        </div>

        <!-- Columns Table -->
        <table
          v-else-if="activeSection === 'columns' && detail"
          class="w-full text-xs border-collapse"
        >
          <thead>
            <tr class="border-b text-left" style="border-color: var(--border-subtle); color: var(--text-muted)">
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
              class="border-b hover:bg-[var(--bg-subtle)]"
              style="border-color: var(--border-subtle)"
            >
              <td class="py-1.5 px-2 font-mono-tabular" style="color: var(--text-faint)">{{ col.ordinalPos }}</td>
              <td class="py-1.5 px-2 font-mono-tabular font-medium">
                <span class="inline-flex items-center gap-1">
                  <KeyRound v-if="col.isPrimaryKey" class="w-3 h-3" style="color: var(--warning)" />
                  {{ col.name }}
                </span>
              </td>
              <td class="py-1.5 px-2 font-mono-tabular" style="color: var(--accent-text)">{{ col.columnType || col.dataType }}</td>
              <td class="py-1.5 px-2">{{ col.nullable ? 'YES' : 'NO' }}</td>
              <td class="py-1.5 px-2 font-mono-tabular" style="color: var(--text-secondary)">
                {{ col.hasDefault ? col.defaultValue : '-' }}
              </td>
              <td class="py-1.5 px-2" style="color: var(--text-muted)">{{ col.comment || '' }}</td>
            </tr>
          </tbody>
        </table>

        <!-- Indexes Table -->
        <table
          v-else-if="activeSection === 'indexes' && detail"
          class="w-full text-xs border-collapse"
        >
          <thead>
            <tr class="border-b text-left" style="border-color: var(--border-subtle); color: var(--text-muted)">
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
              class="border-b hover:bg-[var(--bg-subtle)]"
              style="border-color: var(--border-subtle)"
            >
              <td class="py-1.5 px-2 font-mono-tabular font-medium">{{ idx.name }}</td>
              <td class="py-1.5 px-2 font-mono-tabular">{{ (idx.columns ?? []).join(', ') }}</td>
              <td class="py-1.5 px-2">
                <span v-if="idx.primary" class="px-1.5 py-0.5 rounded text-[10px]" style="background: var(--warning-soft); color: var(--warning)">PRIMARY</span>
                <span v-else-if="idx.unique" class="px-1.5 py-0.5 rounded text-[10px]" style="background: var(--accent-soft); color: var(--accent-text)">UNIQUE</span>
                <span v-else style="color: var(--text-muted)">INDEX</span>
              </td>
              <td class="py-1.5 px-2 font-mono-tabular" style="color: var(--text-muted)">{{ idx.indexType || '-' }}</td>
            </tr>
          </tbody>
        </table>

        <!-- DDL Code -->
        <pre
          v-else-if="activeSection === 'ddl'"
          class="m-0 p-3 rounded-lg border font-mono-tabular text-xs whitespace-pre-wrap break-all select-text leading-relaxed"
          style="background: var(--bg-canvas); border-color: var(--border-subtle); color: var(--text-primary)"
        >{{ ddl || '-- 无 DDL 信息' }}</pre>
      </div>
    </div>
  </div>
</template>
