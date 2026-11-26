<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { X } from 'lucide-vue-next'

const props = withDefaults(
  defineProps<{
    open: boolean
    title?: string
    subtitle?: string
    maxWidthClass?: string
    maxHeightClass?: string
    zIndexClass?: string
    alignTop?: boolean
    hideHeader?: boolean
    testId?: string
  }>(),
  {
    title: '',
    subtitle: '',
    maxWidthClass: 'max-w-xl',
    maxHeightClass: 'max-h-[88vh]',
    zIndexClass: 'z-50',
    alignTop: false,
    hideHeader: false,
    testId: undefined,
  },
)

const emit = defineEmits<{
  (e: 'close'): void
}>()

const dialogRef = ref<HTMLElement | null>(null)
let previouslyFocused: HTMLElement | null = null

const FOCUSABLE_SELECTOR = [
  'button:not([disabled])',
  '[href]',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(', ')

function getFocusableElements(): HTMLElement[] {
  if (!dialogRef.value) return []
  return Array.from(dialogRef.value.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (el) =>
      !el.hasAttribute('disabled') &&
      !el.hidden &&
      el.getAttribute('aria-hidden') !== 'true' &&
      !el.closest('[hidden], [aria-hidden="true"]'),
  )
}

function restoreFocus() {
  const prev = previouslyFocused
  previouslyFocused = null
  if (prev && document.contains(prev) && typeof prev.focus === 'function') {
    prev.focus()
  }
}

watch(
  () => props.open,
  async (isOpen, wasOpen) => {
    if (isOpen && !wasOpen) {
      previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null
      await nextTick()
      if (!props.open || !dialogRef.value) return
      const explicit = dialogRef.value.querySelector<HTMLElement>('[data-modal-initial-focus]')
      if (explicit && !explicit.hasAttribute('disabled')) {
        explicit.focus()
      } else {
        const focusables = getFocusableElements()
        if (focusables.length > 0) {
          focusables[0].focus()
        } else {
          dialogRef.value.focus()
        }
      }
    } else if (!isOpen && wasOpen) {
      await nextTick()
      if (!props.open) {
        restoreFocus()
      }
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  if (props.open) {
    restoreFocus()
  }
})

function onDialogKeydown(ev: KeyboardEvent) {
  if (ev.key !== 'Tab') return
  const focusables = getFocusableElements()
  if (focusables.length === 0) {
    ev.preventDefault()
    dialogRef.value?.focus()
    return
  }
  const first = focusables[0]
  const last = focusables[focusables.length - 1]
  const active = document.activeElement as HTMLElement | null

  if (ev.shiftKey) {
    if (!active || active === first || !dialogRef.value?.contains(active)) {
      ev.preventDefault()
      last.focus()
    }
  } else {
    if (!active || active === last || !dialogRef.value?.contains(active)) {
      ev.preventDefault()
      first.focus()
    }
  }
}
</script>

<template>
  <div
    v-if="open"
    :data-testid="testId"
    class="fixed inset-0 flex justify-center bg-slate-900/45 backdrop-blur-[2px]"
    :class="[zIndexClass, alignTop ? 'items-start pt-20 px-4' : 'items-center p-4']"
    @click.self="emit('close')"
  >
    <div
      ref="dialogRef"
      role="dialog"
      aria-modal="true"
      tabindex="-1"
      class="w-full rounded-xl border border-border-strong bg-elevated shadow-2xl overflow-hidden flex flex-col outline-none"
      :class="[maxWidthClass, maxHeightClass]"
      @keydown="onDialogKeydown"
    >
      <!-- Standard Header (optional) -->
      <div
        v-if="!hideHeader"
        class="px-5 py-3.5 border-b border-border flex items-center justify-between gap-3 shrink-0"
      >
        <div class="min-w-0 flex-1">
          <slot name="header">
            <div class="font-semibold text-sm text-ink truncate">{{ title }}</div>
            <div v-if="subtitle" class="text-[11px] font-mono-tabular text-ink-muted truncate mt-0.5">
              {{ subtitle }}
            </div>
          </slot>
        </div>
        <button
          type="button"
          class="btn btn-ghost btn-icon-sm shrink-0"
          title="关闭 (Esc)"
          @click="emit('close')"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Optional Secondary Toolbar -->
      <slot name="toolbar" />

      <!-- Main Body -->
      <slot />

      <!-- Optional Footer -->
      <div
        v-if="$slots.footer"
        class="px-5 py-3 border-t border-border bg-surface flex items-center justify-between gap-2 shrink-0"
      >
        <slot name="footer" />
      </div>
    </div>
  </div>
</template>
