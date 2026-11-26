<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { FilePlus2, Moon, Plug, Plus, Search, Settings, Sun, Table2 } from 'lucide-vue-next'
import { useConnectionsStore } from '@/stores/connections'
import { useSchemaStore } from '@/stores/schema'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { connectionEndpointSummary, engineLabel } from '@/utils/format'
import BaseModal from './BaseModal.vue'

const connStore = useConnectionsStore()
const schemaStore = useSchemaStore()
const wsStore = useWorkspaceStore()
const settings = useSettingsStore()
const uiStore = useUiStore()

const query = ref('')
const selectedIdx = ref(0)

interface PaletteEntry {
  id: string
  kind: 'action' | 'connection' | 'table'
  label: string
  sub: string
  run: () => void
}

const entries = computed<PaletteEntry[]>(() => {
  const list: PaletteEntry[] = [
    {
      id: 'act-new-conn',
      kind: 'action',
      label: '新建数据库连接',
      sub: '配置 MySQL、PostgreSQL、MariaDB 或 SQLite 连接',
      run: () => uiStore.openNewConnectionModal(),
    },
    {
      id: 'act-settings',
      kind: 'action',
      label: '打开偏好设置',
      sub: '调整亮暗色主题（跟随系统/亮色/暗色）与界面字号',
      run: () => uiStore.openSettingsModal(),
    },
    {
      id: 'act-theme',
      kind: 'action',
      label: settings.theme === 'dark' ? '切换到亮色外观' : '切换到暗色外观',
      sub: '立即切换工作台配色主题',
      run: () => settings.toggleTheme(),
    },
  ]

  for (const c of connStore.connections) {
    list.push({
      id: `conn-query-${c.id}`,
      kind: 'connection',
      label: `新建查询 · ${c.name}`,
      sub: `${engineLabel(c.driver)} · ${connectionEndpointSummary(c)}`,
      run: () => void wsStore.openQueryTab(c.id),
    })
  }

  for (const [key, objs] of Object.entries(schemaStore.objectsMap)) {
    const [connId, db, schema] = key.split('::')
    const conn = connStore.getConnection(connId)
    const active = connStore.getActive(connId)
    const location = active?.capabilities.hasDatabases === false ? conn?.name ?? connId : `${conn?.name ?? connId} / ${db}`
    for (const t of objs.tables) {
      list.push({
        id: `tbl-${key}-${t.name}`,
        kind: 'table',
        label: schema ? `${schema}.${t.name}` : t.name,
        sub: `实时浏览表 · ${location}`,
        run: () => void wsStore.openTableBrowse(connId, db, schema, t.name),
      })
    }
  }

  const q = query.value.trim().toLowerCase()
  if (!q) return list.slice(0, 24)
  return list
    .filter((item) => item.label.toLowerCase().includes(q) || item.sub.toLowerCase().includes(q))
    .slice(0, 30)
})

watch(
  () => uiStore.commandPaletteOpen,
  (open) => {
    if (open) {
      query.value = ''
      selectedIdx.value = 0
    }
  },
)

watch(entries, () => {
  selectedIdx.value = 0
})

function executeItem(item: PaletteEntry) {
  uiStore.closeCommandPalette()
  item.run()
}

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'ArrowDown') {
    ev.preventDefault()
    if (entries.value.length > 0) {
      selectedIdx.value = (selectedIdx.value + 1) % entries.value.length
    }
  } else if (ev.key === 'ArrowUp') {
    ev.preventDefault()
    if (entries.value.length > 0) {
      selectedIdx.value = (selectedIdx.value - 1 + entries.value.length) % entries.value.length
    }
  } else if (ev.key === 'Enter') {
    ev.preventDefault()
    const target = entries.value[selectedIdx.value]
    if (target) executeItem(target)
  }
}
</script>

<template>
  <BaseModal
    :open="uiStore.commandPaletteOpen"
    :align-top="true"
    :hide-header="true"
    max-width-class="max-w-xl"
    test-id="command-palette-modal"
    @close="uiStore.closeCommandPalette()"
  >
    <div class="flex flex-col" @keydown="onKeydown">
      <div class="px-3.5 py-2.5 border-b border-border flex items-center gap-2.5">
        <Search class="w-4 h-4 shrink-0 text-ink-muted" />
        <input
          v-model="query"
          data-modal-initial-focus
          type="text"
          class="flex-1 bg-transparent border-0 outline-none text-sm text-ink"
          placeholder="搜索已加载的表、新建查询或命令..."
        />
        <kbd class="px-1.5 py-0.5 rounded border border-border-strong text-[10px] font-mono-tabular text-ink-faint">
          ESC
        </kbd>
      </div>

      <div class="max-h-80 overflow-y-auto p-1.5 space-y-0.5">
        <div v-if="entries.length === 0" class="py-8 text-center text-xs text-ink-muted">
          未找到匹配的表或命令
        </div>

        <button
          v-for="(item, idx) in entries"
          :key="item.id"
          type="button"
          class="w-full text-left px-3 py-2 rounded-lg border-0 cursor-pointer flex items-center gap-2.5 transition-colors"
          :class="idx === selectedIdx ? 'bg-accent-soft text-accent-text' : 'bg-transparent text-ink hover:bg-subtle'"
          @mouseenter="selectedIdx = idx"
          @click="executeItem(item)"
        >
          <Table2 v-if="item.kind === 'table'" class="w-4 h-4 shrink-0 opacity-80" />
          <Plug v-else-if="item.kind === 'connection'" class="w-4 h-4 shrink-0 opacity-80" />
          <Plus v-else-if="item.id === 'act-new-conn'" class="w-4 h-4 shrink-0 opacity-80" />
          <Settings v-else-if="item.id === 'act-settings'" class="w-4 h-4 shrink-0 opacity-80" />
          <Sun v-else-if="settings.theme === 'dark'" class="w-4 h-4 shrink-0 opacity-80" />
          <Moon v-else class="w-4 h-4 shrink-0 opacity-80" />

          <div class="min-w-0 flex-1">
            <div class="text-xs font-medium truncate">{{ item.label }}</div>
            <div class="text-[11px] truncate opacity-75">{{ item.sub }}</div>
          </div>

          <FilePlus2 v-if="item.kind === 'connection'" class="w-3.5 h-3.5 shrink-0 opacity-60" />
        </button>
      </div>
    </div>
  </BaseModal>
</template>
