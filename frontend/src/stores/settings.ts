import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

export type ThemeMode = 'light' | 'dark'

export const useSettingsStore = defineStore('settings', () => {
  const storedTheme = (localStorage.getItem('sqlm:theme') as ThemeMode | null) || 'light'
  const theme = ref<ThemeMode>(storedTheme === 'dark' ? 'dark' : 'light')
  const sidebarWidth = ref<number>(Number(localStorage.getItem('sqlm:sidebarWidth')) || 276)
  const defaultPageSize = ref<number>(Number(localStorage.getItem('sqlm:pageSize')) || 200)
  const showWhitespace = ref<boolean>(localStorage.getItem('sqlm:showWhitespace') === 'true')

  function applyTheme(mode: ThemeMode) {
    document.documentElement.setAttribute('data-theme', mode)
  }

  function toggleTheme() {
    theme.value = theme.value === 'light' ? 'dark' : 'light'
  }

  function toggleWhitespace() {
    showWhitespace.value = !showWhitespace.value
  }

  function setSidebarWidth(px: number) {
    const clamped = Math.max(210, Math.min(480, Math.round(px)))
    sidebarWidth.value = clamped
    localStorage.setItem('sqlm:sidebarWidth', String(clamped))
  }

  watch(
    theme,
    (val) => {
      localStorage.setItem('sqlm:theme', val)
      applyTheme(val)
    },
    { immediate: true },
  )

  watch(showWhitespace, (val) => {
    localStorage.setItem('sqlm:showWhitespace', String(val))
  })

  return {
    theme,
    sidebarWidth,
    defaultPageSize,
    showWhitespace,
    toggleTheme,
    toggleWhitespace,
    setSidebarWidth,
  }
})
