<script setup lang="ts">
import { computed } from 'vue'
import { Database, FilePlus2, Moon, Plus, Search, Shield, Sun, X } from 'lucide-vue-next'
import { useConnectionsStore } from '@/stores/connections'
import { useSettingsStore } from '@/stores/settings'
import { useWorkspaceStore } from '@/stores/workspace'
import { envColorHex } from '@/utils/format'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const settings = useSettingsStore()

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

function quickNewQuery() {
  const tab = wsStore.activeTab
  if (tab) {
    void wsStore.openQueryTab(tab.connectionId, tab.database, tab.schema)
    return
  }
  const firstActive = Object.keys(connStore.activeMap)[0]
  if (firstActive) {
    void wsStore.openQueryTab(firstActive)
    return
  }
  const firstSaved = connStore.connections[0]
  if (firstSaved) {
    void wsStore.openQueryTab(firstSaved.id)
    return
  }
  connStore.openNewConnectionModal()
}
</script>

<template>
  <header
    class="h-11 px-3 border-b flex items-center justify-between gap-3 shrink-0 select-none"
    style="background: var(--bg-surface); border-color: var(--border-subtle)"
  >
    <!-- Left: Brand + Quick Actions -->
    <div class="flex items-center gap-2">
      <div class="flex items-center gap-2 pr-2">
        <div
          class="w-6 h-6 rounded-md flex items-center justify-center"
          style="background: var(--accent); color: #ffffff"
        >
          <Database class="w-3.5 h-3.5" />
        </div>
        <span class="font-semibold text-sm tracking-tight" style="color: var(--text-primary)">SQL Manager</span>
      </div>

      <button type="button" class="btn" @click="connStore.openNewConnectionModal()">
        <Plus class="w-3.5 h-3.5" />
        新建连接
      </button>

      <button type="button" class="btn" @click="quickNewQuery">
        <FilePlus2 class="w-3.5 h-3.5" />
        新建查询
      </button>
    </div>

    <!-- Center: Always-Visible Active Connection & DB Context -->
    <div
      v-if="activeContext"
      class="hidden md:flex items-center gap-2 px-3 py-1 rounded-full border text-xs max-w-md truncate"
      style="background: var(--bg-canvas); border-color: var(--border-subtle)"
    >
      <span class="w-2 h-2 rounded-full shrink-0" :style="{ background: envColorHex(activeContext.conn.envColor) }" />
      <span class="font-medium truncate" style="color: var(--text-primary)">{{ activeContext.conn.name }}</span>
      <span v-if="activeContext.db" class="font-mono-tabular truncate" style="color: var(--text-secondary)">
        / {{ activeContext.db }}<template v-if="activeContext.schema">.{{ activeContext.schema }}</template>
      </span>
      <span
        v-if="activeContext.version"
        class="font-mono-tabular text-[11px] shrink-0"
        style="color: var(--text-faint)"
      >
        {{ activeContext.version.split(' ')[0] }}
      </span>
      <Shield
        v-if="activeContext.conn.readOnly"
        class="w-3.5 h-3.5 shrink-0"
        style="color: var(--warning)"
        title="客户端只读保护已开启"
      />
    </div>

    <!-- Right: Quick Jump Palette + Theme Switch -->
    <div class="flex items-center gap-1.5">
      <button
        type="button"
        class="btn !text-[11px] !px-2.5"
        style="color: var(--text-muted)"
        title="快速跳转表、连接或命令 (Ctrl+K)"
        @click="wsStore.commandPaletteOpen = true"
      >
        <Search class="w-3.5 h-3.5" />
        <span>快速跳转</span>
        <kbd
          class="ml-1 px-1 py-0 rounded border font-mono-tabular text-[10px]"
          style="border-color: var(--border-strong)"
        >
          Ctrl+K
        </kbd>
      </button>

      <button
        type="button"
        class="btn btn-ghost !w-7 !h-7 !p-0"
        :title="settings.theme === 'dark' ? '切换到浅色主题' : '切换到深色主题'"
        @click="settings.toggleTheme()"
      >
        <Sun v-if="settings.theme === 'dark'" class="w-4 h-4" />
        <Moon v-else class="w-4 h-4" />
      </button>
    </div>

    <!-- Toast Notifications -->
    <div class="fixed right-4 bottom-4 z-50 flex flex-col gap-2 w-80 pointer-events-none">
      <div
        v-for="t in connStore.toasts"
        :key="t.id"
        class="pointer-events-auto rounded-lg border px-3 py-2.5 shadow-lg flex items-start justify-between gap-2 text-xs"
        :style="{
          background: 'var(--bg-elevated)',
          borderColor:
            t.kind === 'error'
              ? 'var(--danger)'
              : t.kind === 'warning'
                ? 'var(--warning)'
                : t.kind === 'success'
                  ? 'var(--success)'
                  : 'var(--border-strong)',
          color: 'var(--text-primary)',
        }"
      >
        <div class="min-w-0 space-y-0.5">
          <div class="font-medium">{{ t.message }}</div>
          <div v-if="t.detail" class="text-[11px] break-all" style="color: var(--text-muted)">{{ t.detail }}</div>
        </div>
        <button
          type="button"
          class="btn btn-ghost !w-5 !h-5 !p-0 shrink-0"
          @click="connStore.dismissToast(t.id)"
        >
          <X class="w-3.5 h-3.5" />
        </button>
      </div>
    </div>
  </header>
</template>
