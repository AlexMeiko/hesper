<script setup lang="ts">
import { X } from 'lucide-vue-next'
import { useUiStore, type ToastItem } from '@/stores/ui'

const uiStore = useUiStore()

function borderClass(kind: ToastItem['kind']): string {
  switch (kind) {
    case 'error':
      return 'border-danger'
    case 'warning':
      return 'border-warning'
    case 'success':
      return 'border-success'
    default:
      return 'border-border-strong'
  }
}
</script>

<template>
  <div class="fixed right-4 bottom-4 z-70 flex flex-col gap-2 w-80 pointer-events-none">
    <div
      v-for="t in uiStore.toasts"
      :key="t.id"
      class="pointer-events-auto rounded-lg border bg-elevated text-ink px-3 py-2.5 shadow-lg flex items-start justify-between gap-2 text-xs"
      :class="borderClass(t.kind)"
    >
      <div class="min-w-0 space-y-0.5">
        <div class="font-medium">{{ t.message }}</div>
        <div v-if="t.detail" class="text-[11px] text-ink-muted break-all">{{ t.detail }}</div>
      </div>
      <button
        type="button"
        class="btn btn-ghost btn-icon-xs shrink-0"
        title="关闭通知"
        @click="uiStore.dismissToast(t.id)"
      >
        <X class="w-3.5 h-3.5" />
      </button>
    </div>
  </div>
</template>
