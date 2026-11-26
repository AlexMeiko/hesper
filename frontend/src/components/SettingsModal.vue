<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import { Monitor, Moon, SlidersHorizontal, Sun, Type, X } from 'lucide-vue-next'
import { useSettingsStore, type FontScale, type ThemePreference } from '@/stores/settings'

const settings = useSettingsStore()

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

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Escape' && settings.settingsModalOpen) {
    ev.preventDefault()
    ev.stopPropagation()
    settings.closeSettingsModal()
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div
    v-if="settings.settingsModalOpen"
    data-testid="settings-modal"
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    style="background: rgba(15, 23, 42, 0.45); backdrop-filter: blur(2px)"
    @click.self="settings.closeSettingsModal()"
  >
    <div
      class="w-full max-w-lg rounded-xl border shadow-2xl overflow-hidden flex flex-col max-h-[88vh]"
      style="background: var(--bg-elevated); border-color: var(--border-strong)"
    >
      <!-- Header -->
      <div
        class="px-5 py-3.5 border-b flex items-center justify-between gap-3 shrink-0"
        style="border-color: var(--border-subtle)"
      >
        <div class="flex items-center gap-2">
          <SlidersHorizontal class="w-4 h-4" style="color: var(--accent)" />
          <span class="font-semibold text-sm" style="color: var(--text-primary)">偏好设置</span>
        </div>
        <button
          type="button"
          class="btn btn-ghost !w-7 !h-7 !p-0"
          title="关闭设置 (Esc)"
          @click="settings.closeSettingsModal()"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Body -->
      <div class="p-5 space-y-6 overflow-y-auto">
        <!-- Section 1: Theme Mode -->
        <section class="space-y-2.5">
          <div class="flex items-center justify-between gap-2">
            <div class="flex items-center gap-1.5 text-[13px] font-medium" style="color: var(--text-primary)">
              <Sun class="w-4 h-4" style="color: var(--text-muted)" />
              <span>外观主题</span>
            </div>
            <span
              v-if="settings.themePreference === 'system'"
              class="text-xs"
              style="color: var(--text-muted)"
            >
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
              :style="{
                borderColor: settings.themePreference === opt.value ? 'var(--accent)' : 'var(--border-subtle)',
                background: settings.themePreference === opt.value ? 'var(--accent-soft)' : 'var(--bg-canvas)',
              }"
              @click="settings.setThemePreference(opt.value)"
            >
              <div
                class="flex items-center gap-1.5 text-[13px] font-medium"
                :style="{ color: settings.themePreference === opt.value ? 'var(--accent-text)' : 'var(--text-primary)' }"
              >
                <Monitor v-if="opt.value === 'system'" class="w-4 h-4 shrink-0" />
                <Sun v-else-if="opt.value === 'light'" class="w-4 h-4 shrink-0" />
                <Moon v-else class="w-4 h-4 shrink-0" />
                <span>{{ opt.label }}</span>
              </div>
              <div class="text-[11px] leading-snug" style="color: var(--text-muted)">
                {{ opt.desc }}
              </div>
            </button>
          </div>
        </section>

        <div class="border-t" style="border-color: var(--border-subtle)" />

        <!-- Section 2: Font Scale -->
        <section class="space-y-2.5">
          <div class="flex items-center justify-between gap-2">
            <div class="flex items-center gap-1.5 text-[13px] font-medium" style="color: var(--text-primary)">
              <Type class="w-4 h-4" style="color: var(--text-muted)" />
              <span>界面字号与密度</span>
            </div>
            <span class="text-xs" style="color: var(--text-muted)">
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
              :style="{
                borderColor: settings.fontScale === opt.value ? 'var(--accent)' : 'var(--border-subtle)',
                background: settings.fontScale === opt.value ? 'var(--accent-soft)' : 'var(--bg-canvas)',
              }"
              @click="settings.setFontScale(opt.value)"
            >
              <div class="flex items-baseline justify-between gap-1">
                <span
                  class="text-[13px] font-medium"
                  :style="{ color: settings.fontScale === opt.value ? 'var(--accent-text)' : 'var(--text-primary)' }"
                >
                  {{ opt.label }}
                </span>
                <span
                  class="font-mono-tabular font-medium"
                  :style="{
                    fontSize: opt.samplePx,
                    color: settings.fontScale === opt.value ? 'var(--accent-text)' : 'var(--text-secondary)',
                  }"
                >
                  Aa
                </span>
              </div>
              <div class="text-[11px] font-mono-tabular" style="color: var(--text-muted)">
                {{ opt.spec }}
              </div>
            </button>
          </div>
        </section>
      </div>

      <!-- Footer -->
      <div
        class="px-5 py-3 border-t flex items-center justify-between gap-2 shrink-0"
        style="background: var(--bg-surface); border-color: var(--border-subtle)"
      >
        <span class="text-xs" style="color: var(--text-muted)">设置自动保存在本地并即时生效</span>
        <button type="button" class="btn btn-primary" @click="settings.closeSettingsModal()">完成</button>
      </div>
    </div>
  </div>
</template>
