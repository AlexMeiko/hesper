<script setup lang="ts">
import { Monitor, Moon, SlidersHorizontal, Sun, Type } from 'lucide-vue-next'
import { useSettingsStore, type FontScale, type ThemePreference } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import BaseModal from './BaseModal.vue'

const settings = useSettingsStore()
const uiStore = useUiStore()

const themeOptions: {
  value: ThemePreference
  label: string
  desc: string
}[] = [
  { value: 'system', label: '跟随系统', desc: '自动同步操作系统外观' },
  { value: 'light', label: '亮色', desc: '清爽高对比日间主题' },
  { value: 'dark', label: '暗色', desc: '低眩光深色护眼主题' },
]

const fontScaleOptions: {
  value: FontScale
  label: string
  spec: string
  samplePx: string
}[] = [
  { value: 'compact', label: '紧凑', spec: '12px · 行高 28px', samplePx: '12px' },
  { value: 'standard', label: '标准', spec: '13px · 行高 31px', samplePx: '13px' },
  { value: 'comfortable', label: '舒适', spec: '14px · 行高 34px', samplePx: '14px' },
]
</script>

<template>
  <BaseModal
    :open="uiStore.settingsModalOpen"
    max-width-class="max-w-lg"
    test-id="settings-modal"
    @close="uiStore.closeSettingsModal()"
  >
    <template #header>
      <div class="flex items-center gap-2">
        <SlidersHorizontal class="w-4 h-4 text-accent" />
        <span class="font-semibold text-sm text-ink">偏好设置</span>
      </div>
    </template>

    <!-- Body -->
    <div class="p-5 space-y-6 overflow-y-auto">
      <!-- Section 1: Theme Mode -->
      <section class="space-y-2.5">
        <div class="flex items-center justify-between gap-2">
          <div class="flex items-center gap-1.5 text-[13px] font-medium text-ink">
            <Sun class="w-4 h-4 text-ink-muted" />
            <span>外观主题</span>
          </div>
          <span v-if="settings.themePreference === 'system'" class="text-xs text-ink-muted">
            当前生效：{{ settings.theme === 'dark' ? '暗色' : '亮色' }}
          </span>
        </div>

        <div class="grid grid-cols-3 gap-2.5">
          <button
            v-for="opt in themeOptions"
            :key="opt.value"
            type="button"
            :data-testid="`theme-option-${opt.value}`"
            class="rounded-lg border p-3 text-left flex flex-col gap-1.5 cursor-pointer outline-none transition-colors"
            :class="
              settings.themePreference === opt.value
                ? 'border-accent bg-accent-soft'
                : 'border-border bg-canvas hover:bg-subtle'
            "
            @click="settings.setThemePreference(opt.value)"
          >
            <div
              class="flex items-center gap-1.5 text-[13px] font-medium"
              :class="settings.themePreference === opt.value ? 'text-accent-text' : 'text-ink'"
            >
              <Monitor v-if="opt.value === 'system'" class="w-4 h-4 shrink-0" />
              <Sun v-else-if="opt.value === 'light'" class="w-4 h-4 shrink-0" />
              <Moon v-else class="w-4 h-4 shrink-0" />
              <span>{{ opt.label }}</span>
            </div>
            <div class="text-[11px] leading-snug text-ink-muted">
              {{ opt.desc }}
            </div>
          </button>
        </div>
      </section>

      <div class="border-t border-border" />

      <!-- Section 2: Font Scale -->
      <section class="space-y-2.5">
        <div class="flex items-center justify-between gap-2">
          <div class="flex items-center gap-1.5 text-[13px] font-medium text-ink">
            <Type class="w-4 h-4 text-ink-muted" />
            <span>界面字号与密度</span>
          </div>
          <span class="text-xs text-ink-muted">
            同步调整对象树、结果网格与编辑器
          </span>
        </div>

        <div class="grid grid-cols-3 gap-2.5">
          <button
            v-for="opt in fontScaleOptions"
            :key="opt.value"
            type="button"
            :data-testid="`font-scale-option-${opt.value}`"
            class="rounded-lg border p-3 text-left flex flex-col gap-1 cursor-pointer outline-none transition-colors"
            :class="
              settings.fontScale === opt.value
                ? 'border-accent bg-accent-soft'
                : 'border-border bg-canvas hover:bg-subtle'
            "
            @click="settings.setFontScale(opt.value)"
          >
            <div class="flex items-baseline justify-between gap-1">
              <span
                class="text-[13px] font-medium"
                :class="settings.fontScale === opt.value ? 'text-accent-text' : 'text-ink'"
              >
                {{ opt.label }}
              </span>
              <span
                class="font-mono-tabular font-medium"
                :class="settings.fontScale === opt.value ? 'text-accent-text' : 'text-ink-secondary'"
                :style="{ fontSize: opt.samplePx }"
              >
                Aa
              </span>
            </div>
            <div class="text-[11px] font-mono-tabular text-ink-muted">
              {{ opt.spec }}
            </div>
          </button>
        </div>
      </section>
    </div>

    <template #footer>
      <span class="text-xs text-ink-muted">设置自动保存在本地并即时生效</span>
      <button type="button" class="btn btn-primary" @click="uiStore.closeSettingsModal()">完成</button>
    </template>
  </BaseModal>
</template>
