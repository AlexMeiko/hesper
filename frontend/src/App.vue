<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'
import { Database, FilePlus2, Loader2, Plus, Table2, TerminalSquare, X } from 'lucide-vue-next'
import { api } from '@/api/client'
import { useDragResize } from '@/composables/useDragResize'
import { useShortcuts } from '@/composables/useShortcuts'
import { useConnectionsStore } from '@/stores/connections'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'
import { envColorHex } from '@/utils/format'
import CommandPalette from './components/CommandPalette.vue'
import ConfirmDialog from './components/ConfirmDialog.vue'
import ConnectionModal from './components/ConnectionModal.vue'
import ExplorerSidebar from './components/ExplorerSidebar.vue'
import QueryTabView from './components/QueryTabView.vue'
import SettingsModal from './components/SettingsModal.vue'
import SSHSecretModal from './components/SSHSecretModal.vue'
import TableBrowseView from './components/TableBrowseView.vue'
import TableDdlModal from './components/TableDdlModal.vue'
import ToastHost from './components/ToastHost.vue'
import TopBar from './components/TopBar.vue'
import ValueInspector from './components/ValueInspector.vue'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const settings = useSettingsStore()
const uiStore = useUiStore()

const { dragging: draggingSidebar, startDrag } = useDragResize()
useShortcuts()

function startSidebarResize(ev: MouseEvent) {
  startDrag(ev, {
    axis: 'x',
    onMove: (_delta, moveEv) => {
      settings.setSidebarWidth(moveEv.clientX)
    },
  })
}

function tabEnvColor(connId: string): string {
  const c = connStore.getConnection(connId)
  return envColorHex(c?.envColor)
}

let unsubSessionState: (() => void) | null = null

onMounted(() => {
  void connStore.loadConnections()
  unsubSessionState = api.onSessionStateChanged((evt) => {
    wsStore.applySessionStateEvent(evt)
  })
})

onUnmounted(() => {
  unsubSessionState?.()
  unsubSessionState = null
})
</script>

<template>
  <div class="w-full h-full flex flex-col overflow-hidden">
    <!-- Top Application Bar -->
    <TopBar />

    <!-- Main Workbench Body -->
    <div class="flex-1 min-h-0 flex overflow-hidden">
      <!-- Left Object Explorer -->
      <div class="shrink-0 h-full min-h-0 overflow-hidden" :style="{ width: `${settings.sidebarWidth}px` }">
        <ExplorerSidebar />
      </div>

      <!-- Sidebar Resizer -->
      <div
        class="splitter-v"
        :class="draggingSidebar ? 'dragging' : ''"
        title="拖动调整侧边栏宽度"
        @mousedown="startSidebarResize"
      />

      <!-- Center Tabs & Active View -->
      <main class="flex-1 min-w-0 flex flex-col overflow-hidden bg-canvas">
        <!-- Open Tabs Bar -->
        <div
          v-if="wsStore.tabs.length > 0"
          class="h-10 px-2 border-b border-border bg-surface flex items-center gap-1.5 overflow-x-auto shrink-0 select-none"
        >
          <div
            v-for="tab in wsStore.tabs"
            :key="tab.id"
            class="group relative h-8 px-2.5 rounded-md flex items-center gap-1.5 text-[13px] cursor-pointer max-w-[240px] shrink-0 border transition-colors"
            :class="
              wsStore.activeTabId === tab.id
                ? 'bg-canvas border-border-strong text-ink'
                : 'bg-transparent border-transparent text-ink-secondary hover:bg-subtle'
            "
            @click="wsStore.setActiveTab(tab.id)"
            @mousedown.middle.prevent="wsStore.closeTab(tab.id, { confirmDirty: true })"
          >
            <!-- Environment Color Indicator -->
            <span class="w-1.5 h-3.5 rounded-full shrink-0" :style="{ background: tabEnvColor(tab.connectionId) }" />

            <Loader2 v-if="tab.kind === 'query' && tab.running" class="w-3.5 h-3.5 shrink-0 animate-spin text-accent" />
            <Table2 v-else-if="tab.kind === 'browse'" class="w-3.5 h-3.5 shrink-0 text-success" />
            <TerminalSquare v-else class="w-3.5 h-3.5 shrink-0 text-accent" />

            <span class="truncate font-medium">{{ tab.title }}</span>

            <button
              type="button"
              class="ml-0.5 w-4 h-4 rounded flex items-center justify-center border-0 bg-transparent text-ink-secondary cursor-pointer opacity-60 hover:opacity-100 hover:bg-hover"
              title="关闭标签页 (同步释放该标签物理连接与快照缓存)"
              @click.stop="wsStore.closeTab(tab.id, { confirmDirty: true })"
            >
              <X class="w-3 h-3" />
            </button>
          </div>
        </div>

        <!-- Active Workspace Content (v-for + v-show preserves CodeMirror undo/cursor/scroll & grid widths across tab switches) -->
        <div class="flex-1 min-h-0 flex overflow-hidden">
          <div class="flex-1 min-w-0 flex flex-col overflow-hidden">
            <template v-if="wsStore.tabs.length > 0">
              <div
                v-for="tab in wsStore.tabs"
                :key="tab.id"
                v-show="wsStore.activeTabId === tab.id"
                class="flex-1 min-h-0 flex flex-col overflow-hidden"
              >
                <TableBrowseView v-if="tab.kind === 'browse'" :tab="tab" :active="wsStore.activeTabId === tab.id" />
                <QueryTabView v-else :tab="tab" :active="wsStore.activeTabId === tab.id" />
              </div>
            </template>

            <!-- Friendly Welcome / Quick-Start Empty State -->
            <div v-else class="flex-1 flex items-center justify-center p-8 overflow-y-auto">
              <div class="max-w-lg w-full space-y-6">
                <div class="space-y-2 text-center">
                  <div class="w-11 h-11 rounded-xl mx-auto flex items-center justify-center bg-accent-soft text-accent-text">
                    <Database class="w-6 h-6" />
                  </div>
                  <h1 class="text-lg font-semibold m-0 text-ink">
                    欢迎使用 SQL Manager
                  </h1>
                  <p class="text-xs m-0 leading-relaxed text-ink-muted">
                    面向 MySQL、PostgreSQL、MariaDB 与 SQLite 的轻量桌面查询工作台。每个查询标签独占一条物理连接，表浏览与编辑器一次性快照清晰分离。
                  </p>
                </div>

                <div class="flex items-center justify-center gap-2.5">
                  <button type="button" class="btn btn-primary h-9 px-4" @click="uiStore.openNewConnectionModal()">
                    <Plus class="w-4 h-4" />
                    新建数据库连接
                  </button>
                  <button
                    v-if="connStore.connections.length > 0"
                    type="button"
                    class="btn h-9 px-4"
                    @click="wsStore.openQueryTab(connStore.connections[0].id)"
                  >
                    <FilePlus2 class="w-4 h-4" />
                    在「{{ connStore.connections[0].name }}」新建查询
                  </button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </main>
    </div>

    <!-- Global Overlays & Toast Host -->
    <ConnectionModal />
    <SettingsModal />
    <TableDdlModal />
    <CommandPalette />
    <ValueInspector />
    <ConfirmDialog />
    <SSHSecretModal />
    <ToastHost />
  </div>
</template>
