<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Compartment, EditorState } from '@codemirror/state'
import { EditorView, highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers, placeholder } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { autocompletion, completionKeymap } from '@codemirror/autocomplete'
import { bracketMatching, defaultHighlightStyle, HighlightStyle, syntaxHighlighting } from '@codemirror/language'
import { searchKeymap, highlightSelectionMatches } from '@codemirror/search'
import { MariaSQL, MySQL, PostgreSQL, SQLite, sql, type SQLNamespace } from '@codemirror/lang-sql'
import { tags } from '@lezer/highlight'
import { EngineType, type CompletionCatalog } from '@/api/client'
import type { ThemeMode } from '@/stores/settings'

const props = defineProps<{
  modelValue: string
  dialect: EngineType
  catalog?: CompletionCatalog | null
  theme: ThemeMode
  readOnly?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
  (e: 'run-current', payload: { sqlOverride?: string; runMode: 'current' | 'selection'; cursorOffset: number }): void
  (e: 'run-all'): void
}>()

const containerRef = ref<HTMLElement | null>(null)
let view: EditorView | null = null
const languageCompartment = new Compartment()
const themeCompartment = new Compartment()

const darkHighlight = HighlightStyle.define([
  { tag: tags.keyword, color: '#60a5fa', fontWeight: '600' },
  { tag: tags.string, color: '#86efac' },
  { tag: tags.number, color: '#fbbf24' },
  { tag: tags.comment, color: '#64748b', fontStyle: 'italic' },
  { tag: tags.operator, color: '#94a3b8' },
  { tag: tags.typeName, color: '#c084fc' },
  { tag: tags.propertyName, color: '#e2e8f0' },
])

function resolveDialect(dialect: EngineType) {
  switch (dialect) {
    case EngineType.EnginePostgres:
      return PostgreSQL
    case EngineType.EngineMariaDB:
      return MariaSQL
    case EngineType.EngineSQLite:
      return SQLite
    default:
      return MySQL
  }
}

function buildSqlExtension() {
  const langDialect = resolveDialect(props.dialect)
  const schema: SQLNamespace = {}
  if (props.catalog?.tables) {
    for (const [tbl, cols] of Object.entries(props.catalog.tables)) {
      schema[tbl] = cols ?? []
    }
  }
  return sql({
    dialect: langDialect,
    schema,
    upperCaseKeywords: true,
  })
}

function buildThemeExtension() {
  const isDark = props.theme === 'dark'
  return [
    EditorView.theme(
      {
        '&': { height: '100%' },
        '.cm-scroller': { overflow: 'auto', fontFamily: 'var(--font-mono)' },
        '.cm-cursor, .cm-dropCursor': { borderLeftColor: isDark ? '#93c5fd' : '#2563eb' },
        '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': {
          backgroundColor: isDark ? 'rgba(59, 130, 246, 0.28)' : 'rgba(37, 99, 235, 0.16)',
        },
      },
      { dark: isDark },
    ),
    syntaxHighlighting(isDark ? darkHighlight : defaultHighlightStyle, { fallback: true }),
  ]
}

function triggerRunCurrent() {
  if (!view) return false
  const sel = view.state.selection.main
  if (sel.from !== sel.to) {
    const selectedText = view.state.sliceDoc(sel.from, sel.to)
    emit('run-current', { sqlOverride: selectedText, runMode: 'selection', cursorOffset: 0 })
  } else {
    emit('run-current', { runMode: 'current', cursorOffset: sel.head })
  }
  return true
}

function getExecutionContext(): { sqlOverride?: string; runMode: 'current' | 'selection'; cursorOffset: number } {
  if (!view) return { runMode: 'current', cursorOffset: 0 }
  const sel = view.state.selection.main
  if (sel.from !== sel.to) {
    return {
      sqlOverride: view.state.sliceDoc(sel.from, sel.to),
      runMode: 'selection',
      cursorOffset: 0,
    }
  }
  return { runMode: 'current', cursorOffset: sel.head }
}

defineExpose({ getExecutionContext })

onMounted(() => {
  if (!containerRef.value) return
  const state = EditorState.create({
    doc: props.modelValue,
    extensions: [
      lineNumbers(),
      highlightActiveLine(),
      highlightActiveLineGutter(),
      history(),
      bracketMatching(),
      highlightSelectionMatches(),
      autocompletion(),
      placeholder('-- 输入 SQL，按 Ctrl+Enter 运行光标所在语句或选中片段，Ctrl+Shift+Enter 运行全部...'),
      languageCompartment.of(buildSqlExtension()),
      themeCompartment.of(buildThemeExtension()),
      keymap.of([
        {
          key: 'Mod-Enter',
          run: () => triggerRunCurrent(),
        },
        {
          key: 'Mod-Shift-Enter',
          run: () => {
            emit('run-all')
            return true
          },
        },
        indentWithTab,
        ...completionKeymap,
        ...searchKeymap,
        ...historyKeymap,
        ...defaultKeymap,
      ]),
      EditorView.updateListener.of((update) => {
        if (update.docChanged) {
          emit('update:modelValue', update.state.doc.toString())
        }
      }),
    ],
  })

  view = new EditorView({
    state,
    parent: containerRef.value,
  })
})

watch(
  () => props.modelValue,
  (next) => {
    if (!view) return
    const cur = view.state.doc.toString()
    if (cur !== next) {
      view.dispatch({
        changes: { from: 0, to: cur.length, insert: next },
      })
    }
  },
)

watch(
  () => [props.dialect, props.catalog],
  () => {
    if (!view) return
    view.dispatch({
      effects: languageCompartment.reconfigure(buildSqlExtension()),
    })
  },
)

watch(
  () => props.theme,
  () => {
    if (!view) return
    view.dispatch({
      effects: themeCompartment.reconfigure(buildThemeExtension()),
    })
  },
)

onBeforeUnmount(() => {
  view?.destroy()
  view = null
})
</script>

<template>
  <div ref="containerRef" class="w-full h-full min-h-0 overflow-hidden" />
</template>
