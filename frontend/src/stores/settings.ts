import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'

export type ThemeMode = 'light' | 'dark'
export type ThemePreference = 'system' | 'light' | 'dark'
export type FontScale = 'compact' | 'standard' | 'comfortable'

const VALID_THEME_PREFS: ThemePreference[] = ['system', 'light', 'dark']
const VALID_SCALES: FontScale[] = ['compact', 'standard', 'comfortable']

function readSystemPrefersDark(): boolean {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return false
  }
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

export const useSettingsStore = defineStore('settings', () => {
  const storedTheme = localStorage.getItem('sqlm:theme') as ThemePreference | null
  const themePreference = ref<ThemePreference>(
    storedTheme && VALID_THEME_PREFS.includes(storedTheme) ? storedTheme : 'system',
  )
  const systemPrefersDark = ref<boolean>(readSystemPrefersDark())

  const storedScale = localStorage.getItem('sqlm:fontScale') as FontScale | null
  const fontScale = ref<FontScale>(storedScale && VALID_SCALES.includes(storedScale) ? storedScale : 'standard')

  const sidebarWidth = ref<number>(Number(localStorage.getItem('sqlm:sidebarWidth')) || 288)
  const defaultPageSize = ref<number>(Number(localStorage.getItem('sqlm:pageSize')) || 200)
  const showWhitespace = ref<boolean>(localStorage.getItem('sqlm:showWhitespace') === 'true')
  const settingsModalOpen = ref<boolean>(false)

  // Listen to OS color-scheme changes while in 'system' mode
  if (typeof window !== 'undefined' && typeof window.matchMedia === 'function') {
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const onSchemeChange = (ev: MediaQueryListEvent) => {
      systemPrefersDark.value = ev.matches
    }
    if (typeof mq.addEventListener === 'function') {
      mq.addEventListener('change', onSchemeChange)
    } else if (typeof mq.addListener === 'function') {
      mq.addListener(onSchemeChange)
    }
  }

  // Resolved effective light/dark theme consumed by DOM, CodeMirror, etc.
  const theme = computed<ThemeMode>(() => {
    if (themePreference.value === 'system') {
      return systemPrefersDark.value ? 'dark' : 'light'
    }
    return themePreference.value
  })

  function applyTheme(mode: ThemeMode) {
    document.documentElement.setAttribute('data-theme', mode)
  }

  function applyFontScale(scale: FontScale) {
    document.documentElement.setAttribute('data-font-scale', scale)
  }

  function setThemePreference(pref: ThemePreference) {
    themePreference.value = pref
  }

  function toggleTheme() {
    themePreference.value = theme.value === 'dark' ? 'light' : 'dark'
  }

  function setFontScale(scale: FontScale) {
    fontScale.value = scale
  }

  function cycleFontScale() {
    const idx = VALID_SCALES.indexOf(fontScale.value)
    fontScale.value = VALID_SCALES[(idx + 1) % VALID_SCALES.length]
  }

  function toggleWhitespace() {
    showWhitespace.value = !showWhitespace.value
  }

  function setSidebarWidth(px: number) {
    const clamped = Math.max(220, Math.min(480, Math.round(px)))
    sidebarWidth.value = clamped
    localStorage.setItem('sqlm:sidebarWidth', String(clamped))
  }

  function openSettingsModal() {
    settingsModalOpen.value = true
  }

  function closeSettingsModal() {
    settingsModalOpen.value = false
  }

  watch(
    themePreference,
    (val) => {
      localStorage.setItem('sqlm:theme', val)
    },
    { immediate: true },
  )

  watch(
    theme,
    (resolved) => {
      applyTheme(resolved)
    },
    { immediate: true },
  )

  watch(
    fontScale,
    (val) => {
      localStorage.setItem('sqlm:fontScale', val)
      applyFontScale(val)
    },
    { immediate: true },
  )

  watch(showWhitespace, (val) => {
    localStorage.setItem('sqlm:showWhitespace', String(val))
  })

  return {
    theme,
    themePreference,
    systemPrefersDark,
    fontScale,
    sidebarWidth,
    defaultPageSize,
    showWhitespace,
    settingsModalOpen,
    setThemePreference,
    toggleTheme,
    setFontScale,
    cycleFontScale,
    toggleWhitespace,
    setSidebarWidth,
    openSettingsModal,
    closeSettingsModal,
  }
})
