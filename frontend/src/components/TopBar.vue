<script setup lang="ts">
import { computed } from 'vue'
import { Database, FilePlus2, Plus, Search, Settings, Shield } from 'lucide-vue-next'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { envColorHex } from '@/utils/format'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const uiStore = useUiStore()

const activeContext = computed(() => {
  const tab = wsStore.activeTab
  if (!tab) {
    const firstConnectedId = Object.keys(connStore.activeMap)[0]
    if (!firstConnectedId) return null
    const c = connStore.getConnection(firstConnectedId)
    const a = connStore.activeMap[firstConnectedId]
    if (!c || !a) return null
    return {
      conn: c,
      version: a.serverVersion,
      db: a.currentDb,
      schema: '',
    }
  }
  const c = connStore.getConnection(tab.connectionId)
  const a = connStore.activeMap[tab.connectionId]
  if (!c) return null
  return {
    conn: c,
    version: a?.serverVersion || '',
    db: tab.database,
    schema: tab.schema,
  }
})
</script>

<template>
  <header class="h-11 px-3 border-b border-border bg-surface flex items-center justify-between gap-3 shrink-0 select-none">
    <!-- Left: Brand + Quick Actions -->
    <div class="flex items-center gap-2">
      <div class="flex items-center gap-2 pr-2">
        <div class="w-6 h-6 rounded-md bg-accent text-white flex items-center justify-center">
          <Database class="w-3.5 h-3.5" />
        </div>
        <span class="font-semibold text-sm tracking-tight text-ink">Hesper</span>
      </div>

      <button type="button" class="btn" @click="uiStore.openNewConnectionModal()">
        <Plus class="w-3.5 h-3.5" />
        新建连接
      </button>

      <button type="button" class="btn" title="新建查询标签 (Ctrl+N)" @click="wsStore.openQuickQuery()">
        <FilePlus2 class="w-3.5 h-3.5" />
        新建查询
      </button>
    </div>

    <!-- Center: Always-Visible Active Connection & DB Context -->
    <div
      v-if="activeContext"
      class="hidden md:flex items-center gap-2 px-3 py-1 rounded-full border border-border bg-canvas text-[13px] max-w-md truncate"
    >
      <span class="w-2 h-2 rounded-full shrink-0" :style="{ background: envColorHex(activeContext.conn.envColor) }" />
      <span class="font-medium truncate text-ink">{{ activeContext.conn.name }}</span>
      <span v-if="activeContext.db" class="font-mono-tabular truncate text-ink-secondary">
        / {{ activeContext.db }}<template v-if="activeContext.schema">.{{ activeContext.schema }}</template>
      </span>
      <span v-if="activeContext.version" class="font-mono-tabular text-xs shrink-0 text-ink-faint">
        {{ activeContext.version.split(' ')[0] }}
      </span>
      <Shield
        v-if="activeContext.conn.readOnly"
        class="w-3.5 h-3.5 shrink-0 text-warning"
        title="客户端只读保护已开启"
      />
    </div>

    <!-- Right: Quick Jump Palette + Settings -->
    <div class="flex items-center gap-1.5">
      <button
        type="button"
        class="btn btn-sm text-ink-muted"
        title="快速跳转表、连接或命令 (Ctrl+K)"
        @click="uiStore.openCommandPalette()"
      >
        <Search class="w-3.5 h-3.5" />
        <span>快速跳转</span>
        <kbd class="ml-1 px-1 py-0 rounded border border-border-strong font-mono-tabular text-[11px]">
          Ctrl+K
        </kbd>
      </button>

      <button
        type="button"
        data-testid="open-settings-btn"
        class="btn btn-ghost btn-icon"
        title="偏好设置 (外观主题与界面字号，Ctrl+,)"
        @click="uiStore.openSettingsModal()"
      >
        <Settings class="w-4 h-4" />
      </button>
    </div>
  </header>
</template>
