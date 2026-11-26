<script setup lang="ts">
import { computed } from 'vue'
import { AlertOctagon, AlertTriangle, Info } from 'lucide-vue-next'
import { useUiStore } from '@/stores/ui'
import BaseModal from './BaseModal.vue'

const uiStore = useUiStore()
const dlg = computed(() => uiStore.confirmDialog)
</script>

<template>
  <BaseModal
    :open="Boolean(dlg)"
    max-width-class="max-w-lg"
    max-height-class="max-h-[82vh]"
    z-index-class="z-60"
    test-id="confirm-dialog"
    @close="uiStore.resolveConfirm(false)"
  >
    <template #header>
      <div class="flex items-center gap-2">
        <AlertOctagon v-if="dlg?.tone === 'danger'" class="w-4 h-4 shrink-0 text-danger" />
        <AlertTriangle v-else-if="dlg?.tone === 'warning'" class="w-4 h-4 shrink-0 text-warning" />
        <Info v-else class="w-4 h-4 shrink-0 text-accent" />
        <span class="font-semibold text-sm text-ink truncate">{{ dlg?.title }}</span>
      </div>
    </template>

    <div v-if="dlg" class="p-5 space-y-3.5 overflow-y-auto text-xs">
      <p class="m-0 leading-relaxed text-ink">{{ dlg.message }}</p>

      <ul
        v-if="dlg.bullets && dlg.bullets.length > 0"
        class="m-0 pl-4 space-y-1 rounded-lg border px-3 py-2"
        :class="
          dlg.tone === 'danger'
            ? 'bg-danger-soft border-danger text-danger'
            : 'bg-warning-soft border-warning text-warning'
        "
      >
        <li v-for="(item, idx) in dlg.bullets" :key="idx" class="font-medium">{{ item }}</li>
      </ul>

      <pre
        v-if="dlg.codePreview"
        class="m-0 p-3 rounded-lg border border-border bg-canvas text-ink font-mono-tabular text-[12px] whitespace-pre-wrap break-all max-h-48 overflow-auto select-text"
      >{{ dlg.codePreview }}</pre>
    </div>

    <template #footer>
      <div class="ml-auto flex items-center gap-2">
        <button
          type="button"
          data-testid="confirm-dialog-cancel-btn"
          data-modal-initial-focus
          class="btn"
          @click="uiStore.resolveConfirm(false)"
        >
          {{ dlg?.cancelLabel }}
        </button>
        <button
          type="button"
          data-testid="confirm-dialog-confirm-btn"
          class="btn"
          :class="dlg?.tone === 'danger' ? 'btn-danger' : 'btn-primary'"
          @click="uiStore.resolveConfirm(true)"
        >
          {{ dlg?.confirmLabel }}
        </button>
      </div>
    </template>
  </BaseModal>
</template>
