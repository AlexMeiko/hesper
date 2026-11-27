import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { CellValue, ColumnMeta, ConnectionDTO, SSHAuthPrompt } from '@/api/types'

export interface ToastItem {
  id: number
  kind: 'info' | 'success' | 'warning' | 'error'
  message: string
  detail?: string
}

export interface InspectedCell {
  column: ColumnMeta
  cell: CellValue
  rowIndex: number
}

export interface DdlTarget {
  connectionId: string
  database: string
  schema: string
  table: string
}

export interface ConfirmOptions {
  title: string
  message: string
  tone?: 'danger' | 'warning' | 'default'
  bullets?: string[]
  codePreview?: string
  confirmLabel?: string
  cancelLabel?: string
}

export interface ConfirmDialogState extends ConfirmOptions {
  tone: 'danger' | 'warning' | 'default'
  confirmLabel: string
  cancelLabel: string
}

export interface SSHSecretSubmission {
  secret: string
  saveSecret: boolean
}

/**
 * Centralized UI store owning global toasts and all modal overlays
 * so Esc priority handling and modal lifecycle stay in one predictable place.
 */
export const useUiStore = defineStore('ui', () => {
  // --- Toast notifications ---
  const toasts = ref<ToastItem[]>([])
  let toastSeq = 1

  function notify(kind: ToastItem['kind'], message: string, detail?: string, ttlMs = 3800) {
    const id = toastSeq++
    toasts.value.push({ id, kind, message, detail })
    window.setTimeout(() => {
      toasts.value = toasts.value.filter((t) => t.id !== id)
    }, ttlMs)
  }

  function dismissToast(id: number) {
    toasts.value = toasts.value.filter((t) => t.id !== id)
  }

  // --- Modal states ---
  const connectionModalOpen = ref(false)
  const editingConnection = ref<ConnectionDTO | null>(null)
  const settingsModalOpen = ref(false)
  const commandPaletteOpen = ref(false)
  const ddlTarget = ref<DdlTarget | null>(null)
  const inspectedCell = ref<InspectedCell | null>(null)
  const confirmDialog = ref<ConfirmDialogState | null>(null)
  let confirmResolver: ((ok: boolean) => void) | null = null
  const sshSecretPrompt = ref<SSHAuthPrompt | null>(null)
  const sshSecretSaveMode = ref<'immediate' | 'form'>('immediate')
  let sshSecretResolver: ((res: SSHSecretSubmission | null) => void) | null = null

  function openNewConnectionModal() {
    editingConnection.value = null
    connectionModalOpen.value = true
  }

  function openEditConnectionModal(conn: ConnectionDTO) {
    editingConnection.value = conn
    connectionModalOpen.value = true
  }

  function closeConnectionModal() {
    connectionModalOpen.value = false
    editingConnection.value = null
  }

  function openSettingsModal() {
    settingsModalOpen.value = true
  }

  function closeSettingsModal() {
    settingsModalOpen.value = false
  }

  function openCommandPalette() {
    commandPaletteOpen.value = true
  }

  function closeCommandPalette() {
    commandPaletteOpen.value = false
  }

  function toggleCommandPalette() {
    commandPaletteOpen.value = !commandPaletteOpen.value
  }

  function openTableDdl(target: DdlTarget) {
    ddlTarget.value = target
  }

  function closeTableDdl() {
    ddlTarget.value = null
  }

  function inspectCell(column: ColumnMeta, cell: CellValue, rowIndex: number) {
    inspectedCell.value = { column, cell, rowIndex }
  }

  function closeInspector() {
    inspectedCell.value = null
  }

  function requestConfirm(opts: ConfirmOptions): Promise<boolean> {
    if (confirmResolver) {
      confirmResolver(false)
      confirmResolver = null
    }
    confirmDialog.value = {
      ...opts,
      tone: opts.tone ?? 'default',
      confirmLabel: opts.confirmLabel ?? '确定',
      cancelLabel: opts.cancelLabel ?? '取消',
    }
    return new Promise<boolean>((resolve) => {
      confirmResolver = resolve
    })
  }

  function resolveConfirm(ok: boolean) {
    const resolver = confirmResolver
    confirmResolver = null
    confirmDialog.value = null
    resolver?.(ok)
  }

  function requestSSHSecret(
    prompt: SSHAuthPrompt,
    opts?: { saveMode?: 'immediate' | 'form' },
  ): Promise<SSHSecretSubmission | null> {
    if (sshSecretResolver) {
      sshSecretResolver(null)
      sshSecretResolver = null
    }
    sshSecretSaveMode.value = opts?.saveMode ?? 'immediate'
    sshSecretPrompt.value = prompt
    return new Promise<SSHSecretSubmission | null>((resolve) => {
      sshSecretResolver = resolve
    })
  }

  function resolveSSHSecret(res: SSHSecretSubmission | null) {
    const resolver = sshSecretResolver
    sshSecretResolver = null
    sshSecretPrompt.value = null
    sshSecretSaveMode.value = 'immediate'
    resolver?.(res)
  }

  const hasOpenModal = computed(
    () =>
      Boolean(confirmDialog.value) ||
      Boolean(sshSecretPrompt.value) ||
      Boolean(inspectedCell.value) ||
      commandPaletteOpen.value ||
      settingsModalOpen.value ||
      connectionModalOpen.value ||
      Boolean(ddlTarget.value),
  )

  /**
   * Closes only the topmost open modal (matching visual z-order).
   * Returns true if a modal was closed so global shortcuts (e.g. Esc cancel query) never fire underneath.
   */
  function closeTopModal(): boolean {
    if (confirmDialog.value) {
      resolveConfirm(false)
      return true
    }
    if (sshSecretPrompt.value) {
      resolveSSHSecret(null)
      return true
    }
    if (inspectedCell.value) {
      closeInspector()
      return true
    }
    if (commandPaletteOpen.value) {
      closeCommandPalette()
      return true
    }
    if (settingsModalOpen.value) {
      closeSettingsModal()
      return true
    }
    if (connectionModalOpen.value) {
      closeConnectionModal()
      return true
    }
    if (ddlTarget.value) {
      closeTableDdl()
      return true
    }
    return false
  }

  return {
    toasts,
    notify,
    dismissToast,
    connectionModalOpen,
    editingConnection,
    settingsModalOpen,
    commandPaletteOpen,
    ddlTarget,
    inspectedCell,
    confirmDialog,
    sshSecretPrompt,
    sshSecretSaveMode,
    hasOpenModal,
    openNewConnectionModal,
    openEditConnectionModal,
    closeConnectionModal,
    openSettingsModal,
    closeSettingsModal,
    openCommandPalette,
    closeCommandPalette,
    toggleCommandPalette,
    openTableDdl,
    closeTableDdl,
    inspectCell,
    closeInspector,
    requestConfirm,
    resolveConfirm,
    requestSSHSecret,
    resolveSSHSecret,
    closeTopModal,
  }
})
