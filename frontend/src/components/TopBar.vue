<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Database, FilePlus2, Plus, Search, Settings, Shield } from 'lucide-vue-next'
import { api } from '@/api/client'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { envColorHex } from '@/utils/format'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const uiStore = useUiStore()

const showWindowControls = computed(() => api.hasDesktopWindowControls())
const isMaximised = ref(false)
let unsubMaximise: (() => void) | null = null

onMounted(() => {
  if (!showWindowControls.value) return
  unsubMaximise = api.onWindowMaximiseChanged((maximised) => {
    isMaximised.value = maximised
  })
})

onBeforeUnmount(() => {
  unsubMaximise?.()
  unsubMaximise = null
})

async function minimiseWindow() {
  await api.windowMinimise()
}

async function toggleMaximise() {
  isMaximised.value = await api.windowToggleMaximise()
}

async function closeWindow() {
  await api.windowClose()
}

function onHeaderDoubleClick(ev: MouseEvent) {
  if (!showWindowControls.value) return
  const target = ev.target as HTMLElement | null
  if (target?.closest('button, input, select, a, .titlebar-no-drag')) return
  void toggleMaximise()
}

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
  <header
    class="titlebar-drag h-11 pl-3 border-b border-border bg-surface flex items-center justify-between gap-3 shrink-0 select-none"
    :class="showWindowControls ? 'pr-0' : 'pr-3'"
    @dblclick="onHeaderDoubleClick"
  >
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

    <!-- Right: Quick Jump Palette + Settings + VS Code Style Window Controls -->
    <div class="flex items-center h-full">
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

      <div
        v-if="showWindowControls"
        data-testid="window-controls"
        class="titlebar-no-drag flex items-center h-full ml-2 border-l border-border"
      >
        <button
          type="button"
          data-testid="window-minimize-btn"
          class="window-ctrl-btn"
          title="最小化"
          aria-label="最小化"
          @click="minimiseWindow"
        >
          <svg class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.2">
            <path d="M2 6h8" />
          </svg>
        </button>

        <button
          type="button"
          data-testid="window-maximize-btn"
          class="window-ctrl-btn"
          :title="isMaximised ? '还原' : '最大化'"
          :aria-label="isMaximised ? '还原' : '最大化'"
          @click="toggleMaximise"
        >
          <svg v-if="isMaximised" class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.15">
            <rect x="2" y="4" width="6" height="6" rx="0.5" />
            <path d="M4 4V2.5a.5.5 0 0 1 .5-.5H9.5a.5.5 0 0 1 .5.5v5a.5.5 0 0 1-.5.5H8" />
          </svg>
          <svg v-else class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.15">
            <rect x="2.25" y="2.25" width="7.5" height="7.5" rx="0.5" />
          </svg>
        </button>

        <button
          type="button"
          data-testid="window-close-btn"
          class="window-ctrl-btn window-ctrl-close"
          title="关闭"
          aria-label="关闭"
          @click="closeWindow"
        >
          <svg class="w-3 h-3" viewBox="0 0 12 12" fill="none" stroke="currentColor" stroke-width="1.2">
            <path d="M2.5 2.5l7 7M9.5 2.5l-7 7" />
          </svg>
        </button>
      </div>
    </div>
  </header>
</template>
