import { onBeforeUnmount, onMounted } from 'vue'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore } from '@/stores/workspace'

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable
}

/**
 * Single centralized global keyboard shortcut manager (docs/03 §6.5).
 * Enforces strict modal-first Escape priority so pressing Esc inside any floating modal
 * ONLY closes that modal and NEVER cancels a running SQL query underneath.
 */
export function useShortcuts() {
  const uiStore = useUiStore()
  const wsStore = useWorkspaceStore()

  function handleKeydown(ev: KeyboardEvent) {
    if (ev.defaultPrevented) return

    const mod = ev.ctrlKey || ev.metaKey
    const key = ev.key.toLowerCase()

    // 1. Escape: strict modal-stack priority before query cancellation
    if (ev.key === 'Escape') {
      if (uiStore.closeTopModal()) {
        ev.preventDefault()
        return
      }
      const tab = wsStore.activeTab
      if (tab && tab.kind === 'query' && tab.running) {
        ev.preventDefault()
        void wsStore.cancelQuery(tab.id)
      }
      return
    }

    // 2. Mod+K: Command Palette (works even inside editor, unless confirm dialog blocks)
    if (mod && !ev.shiftKey && !ev.altKey && key === 'k') {
      if (uiStore.confirmDialog) return
      ev.preventDefault()
      uiStore.toggleCommandPalette()
      return
    }

    // Ignore other global shortcuts while any modal is open
    if (uiStore.hasOpenModal) return

    // 3. Mod+, : Settings Modal
    if (mod && !ev.shiftKey && !ev.altKey && ev.key === ',') {
      ev.preventDefault()
      uiStore.openSettingsModal()
      return
    }

    // 4. Mod+N : New Query Tab
    if (mod && !ev.shiftKey && !ev.altKey && key === 'n') {
      ev.preventDefault()
      wsStore.openQuickQuery()
      return
    }

    // 5. Mod+W : Close Active Tab (with dirty-SQL confirmation)
    if (mod && !ev.shiftKey && !ev.altKey && key === 'w') {
      const tab = wsStore.activeTab
      if (tab) {
        ev.preventDefault()
        void wsStore.closeTab(tab.id, { confirmDirty: true })
      }
      return
    }

    // 6. Mod+. : Cancel Running Query
    if (mod && !ev.shiftKey && !ev.altKey && ev.key === '.') {
      const tab = wsStore.activeTab
      if (tab && tab.kind === 'query' && tab.running) {
        ev.preventDefault()
        void wsStore.cancelQuery(tab.id)
      }
      return
    }

    // 7. F5 : Refresh active live TableBrowse tab (when not typing in form controls)
    if (ev.key === 'F5' && !mod && !ev.altKey && !isEditableTarget(ev.target)) {
      const tab = wsStore.activeTab
      if (tab && tab.kind === 'browse' && !tab.loading) {
        ev.preventDefault()
        void wsStore.refreshBrowseTab(tab.id)
      }
    }
  }

  onMounted(() => window.addEventListener('keydown', handleKeydown))
  onBeforeUnmount(() => window.removeEventListener('keydown', handleKeydown))

  return { handleKeydown }
}
