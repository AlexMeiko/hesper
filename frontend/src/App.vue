<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Database, FilePlus2, Loader2, Plus, Table2, TerminalSquare, X } from 'lucide-vue-next'
import { useConnectionsStore } from '@/stores/connections'
import { useSettingsStore } from '@/stores/settings'
import { useWorkspaceStore } from '@/stores/workspace'
import { envColorHex } from '@/utils/format'
import CommandPalette from './components/CommandPalette.vue'
import ConnectionModal from './components/ConnectionModal.vue'
import ExplorerSidebar from './components/ExplorerSidebar.vue'
import QueryTabView from './components/QueryTabView.vue'
import SettingsModal from './components/SettingsModal.vue'
import TableBrowseView from './components/TableBrowseView.vue'
import TableDdlModal from './components/TableDdlModal.vue'
import TopBar from './components/TopBar.vue'
import ValueInspector from './components/ValueInspector.vue'

const connStore = useConnectionsStore()
const wsStore = useWorkspaceStore()
const settings = useSettingsStore()

const draggingSidebar = ref(false)

function startSidebarResize(ev: MouseEvent) {
  ev.preventDefault()
  draggingSidebar.value = true
  const onMove = (m: MouseEvent) => {
    settings.setSidebarWidth(m.clientX)
  }
  const onUp = () => {
    draggingSidebar.value = false
    window.removeEventListener('mousemove', onMove)
    window.removeEventListener('mouseup', onUp)
  }
  window.addEventListener('mousemove', onMove)
  window.addEventListener('mouseup', onUp)
}

function tabEnvColor(connId: string): string {
  const c = connStore.getConnection(connId)
  return envColorHex(c?.envColor)
}

function onGlobalKeydown(ev: KeyboardEvent) {
  if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'k') {
    ev.preventDefault()
    wsStore.commandPaletteOpen = !wsStore.commandPaletteOpen
  }
}

onMounted(() => {
  void connStore.loadConnections()
  window.addEventListener('keydown', onGlobalKeydown)
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onGlobalKeydown)
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
      <main class="flex-1 min-w-0 flex flex-col overflow-hidden" style="background: var(--bg-canvas)">
        <!-- Open Tabs Bar -->
        <div
          v-if="wsStore.tabs.length > 0"
          class="h-10 px-2 border-b flex items-center gap-1.5 overflow-x-auto shrink-0 select-none"
          style="background: var(--bg-surface); border-color: var(--border-subtle)"
        >
          <div
            v-for="tab in wsStore.tabs"
            :key="tab.id"
            class="group relative h-8 px-2.5 rounded-md flex items-center gap-1.5 text-[13px] cursor-pointer max-w-[240px] shrink-0 border transition-colors"
            :style="{
              background: wsStore.activeTabId === tab.id ? 'var(--bg-canvas)' : 'transparent',
              borderColor: wsStore.activeTabId === tab.id ? 'var(--border-strong)' : 'transparent',
              color: wsStore.activeTabId === tab.id ? 'var(--text-primary)' : 'var(--text-secondary)',
            }"
            @click="wsStore.setActiveTab(tab.id)"
            @mousedown.middle.prevent="wsStore.closeTab(tab.id)"
          >
            <!-- Environment Color Indicator -->
            <span class="w-1.5 h-3.5 rounded-full shrink-0" :style="{ background: tabEnvColor(tab.connectionId) }" />

            <Loader2
              v-if="tab.kind === 'query' && tab.running"
              class="w-3.5 h-3.5 shrink-0 animate-spin"
              style="color: var(--accent)"
            />
            <Table2 v-else-if="tab.kind === 'browse'" class="w-3.5 h-3.5 shrink-0" style="color: var(--success)" />
            <TerminalSquare v-else class="w-3.5 h-3.5 shrink-0" style="color: var(--accent)" />

            <span class="truncate font-medium">{{ tab.title }}</span>

            <button
              type="button"
              class="ml-0.5 w-4 h-4 rounded flex items-center justify-center border-0 bg-transparent cursor-pointer opacity-60 hover:opacity-100 hover:bg-[var(--bg-hover)]"
              style="color: var(--text-secondary)"
              title="关闭标签页（同步释放该标签物理连接与快照缓存）"
              @click.stop="wsStore.closeTab(tab.id)"
            >
              <X class="w-3 h-3" />
            </button>
          </div>
        </div>

        <!-- Active Workspace Content -->
        <div class="flex-1 min-h-0 flex overflow-hidden">
          <div class="flex-1 min-w-0 flex flex-col overflow-hidden">
            <template v-if="wsStore.activeTab">
              <TableBrowseView
                v-if="wsStore.activeTab.kind === 'browse'"
                :key="wsStore.activeTab.id"
                :tab="wsStore.activeTab"
              />
              <QueryTabView
                v-else
                :key="wsStore.activeTab.id"
                :tab="wsStore.activeTab"
              />
            </template>

            <!-- Friendly Welcome / Quick-Start Empty State -->
            <div v-else class="flex-1 flex items-center justify-center p-8 overflow-y-auto">
              <div class="max-w-lg w-full space-y-6">
                <div class="space-y-2 text-center">
                  <div
                    class="w-11 h-11 rounded-xl mx-auto flex items-center justify-center"
                    style="background: var(--accent-soft); color: var(--accent-text)"
                  >
                    <Database class="w-6 h-6" />
                  </div>
                  <h1 class="text-lg font-semibold m-0" style="color: var(--text-primary)">
                    欢迎使用 SQL Manager
                  </h1>
                  <p class="text-xs m-0 leading-relaxed" style="color: var(--text-muted)">
                    面向 MySQL、PostgreSQL、MariaDB 与 SQLite 的轻量桌面查询工作台。每个查询标签独占一条物理连接，表浏览与编辑器一次性快照清晰分离。
                  </p>
                </div>

                <div class="flex items-center justify-center gap-2.5">
                  <button type="button" class="btn btn-primary !h-9 !px-4" @click="connStore.openNewConnectionModal()">
                    <Plus class="w-4 h-4" />
                    新建数据库连接
                  </button>
                  <button
                    v-if="connStore.connections.length > 0"
                    type="button"
                    class="btn !h-9 !px-4"
                    @click="wsStore.openQueryTab(connStore.connections[0].id)"
                  >
                    <FilePlus2 class="w-4 h-4" />
                    在「{{ connStore.connections[0].name }}」新建查询
                  </button>
                </div>

                <div
                  class="rounded-xl border p-4 grid grid-cols-1 sm:grid-cols-2 gap-3 text-xs"
                  style="background: var(--bg-surface); border-color: var(--border-subtle)"
                >
                  <div class="space-y-1">
                    <div class="font-medium" style="color: var(--text-primary)">双击表直接浏览</div>
                    <div class="text-[11px] leading-relaxed" style="color: var(--text-muted)">
                      左侧展开库表后双击任意表即开启「实时浏览」；支持 WHERE 过滤、表头排序和结构/DDL 查看。
                    </div>
                  </div>
                  <div class="space-y-1">
                    <div class="font-medium" style="color: var(--text-primary)">编辑器执行一次 · 翻页读快照</div>
                    <div class="text-[11px] leading-relaxed" style="color: var(--text-muted)">
                      按 Ctrl+Enter 运行当前语句，按 Esc 旁路停止；翻页只读第一次结果快照，绝不重复触发副作用。
                    </div>
                  </div>
                  <div class="space-y-1">
                    <div class="font-medium" style="color: var(--text-primary)">数值保真与 NULL 区分</div>
                    <div class="text-[11px] leading-relaxed" style="color: var(--text-muted)">
                      BIGINT / DECIMAL 原样文本呈现不丢精度，NULL 与空串 "" 一眼可辨，BLOB 按需流式查看。
                    </div>
                  </div>
                  <div class="space-y-1">
                    <div class="font-medium" style="color: var(--text-primary)">清晰会话状态与快捷键</div>
                    <div class="text-[11px] leading-relaxed" style="color: var(--text-muted)">
                      随时显示后端确认的会话状态；按 Ctrl+K 快速跳转表与命令，关闭标签自动释放连接与缓存。
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </main>
    </div>

    <!-- Global Modals -->
    <ConnectionModal />
    <SettingsModal />
    <TableDdlModal />
    <CommandPalette />
    <ValueInspector
      :item="wsStore.inspectedCell"
      @close="wsStore.closeInspector()"
      @copied="(lbl) => connStore.notify('success', lbl)"
    />
  </div>
</template>
