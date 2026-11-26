<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { ArrowDown, ArrowUp, Copy, Eye, KeyRound, Pilcrow } from 'lucide-vue-next'
import { EngineType, ValueKind, type CellValue, type ColumnMeta } from '@/api/types'
import { useDragResize } from '@/composables/useDragResize'
import { useSettingsStore } from '@/stores/settings'
import { copyToClipboard } from '@/utils/clipboard'
import {
  cellToPlainText,
  estimateColumnWidth,
  formatBytes,
  formatExportCaveat,
  inspectExportFidelity,
  isNumericKind,
  rowsToCSV,
  rowsToInsertSQL,
  rowsToJSON,
  rowsToTSV,
} from '@/utils/format'
import type { TableTarget } from '@/utils/tableMeta'
import CellText from './CellText.vue'

export interface GridCopyNotice {
  kind: 'success' | 'warning'
  message: string
  detail?: string
}

const props = withDefaults(
  defineProps<{
    columns: ColumnMeta[]
    rows: (CellValue[] | null)[]
    rowNumberOffset?: number
    sortable?: boolean
    sortColumn?: string
    sortDir?: '' | 'asc' | 'desc'
    tableTarget?: TableTarget | null
    dialect?: EngineType
    loading?: boolean
    active?: boolean
  }>(),
  {
    rowNumberOffset: 0,
    sortable: false,
    sortColumn: '',
    sortDir: '',
    tableTarget: null,
    dialect: undefined,
    loading: false,
    active: true,
  },
)

const emit = defineEmits<{
  (e: 'sort', colName: string): void
  (e: 'inspect', payload: { column: ColumnMeta; cell: CellValue; rowIndex: number }): void
  (e: 'copied', notice: GridCopyNotice): void
}>()

const settings = useSettingsStore()
const { startDrag } = useDragResize()

const scrollContainerRef = ref<HTMLElement | null>(null)
let savedScrollTop = 0
let savedScrollLeft = 0
const colWidths = ref<Record<number, number>>({})
const lastColSig = ref('')
const widthsSeededFromRows = ref(false)

// Selection state (supports single cell and Shift+Click rectangular selection)
interface CellCoord {
  row: number
  col: number
}
const anchorCell = ref<CellCoord | null>(null)
const focusCell = ref<CellCoord | null>(null)

const columnSignature = computed(() =>
  props.columns.map((c) => `${c.name}:${c.databaseType}:${c.kind}:${c.isPrimaryKey ? '1' : '0'}`).join('|'),
)

function computeInitialWidths() {
  const next: Record<number, number> = {}
  props.columns.forEach((col, idx) => {
    next[idx] = estimateColumnWidth(col, props.rows, idx, { maxSampleRows: 100, autoFit: false })
  })
  colWidths.value = next
}

// Recalculate column widths when column structure changes OR when initial rows arrive for a column set.
// Preserves user-resized widths when paging within the same result/table, while resetting stale scroll
// offsets whenever a new result set or page replaces the displayed rows (including in background tabs).
watch(
  [columnSignature, () => props.rows],
  ([sig, rows], prev) => {
    const prevRows = prev?.[1]
    const colChanged = sig !== lastColSig.value
    const rowsChanged = prevRows !== undefined && rows !== prevRows

    if (colChanged) {
      lastColSig.value = sig
      widthsSeededFromRows.value = rows.length > 0
      computeInitialWidths()
      anchorCell.value = null
      focusCell.value = null
      savedScrollTop = 0
      savedScrollLeft = 0
      if (scrollContainerRef.value) {
        scrollContainerRef.value.scrollTop = 0
        scrollContainerRef.value.scrollLeft = 0
      }
      return
    }

    if (rowsChanged) {
      savedScrollTop = 0
      if (!props.active) {
        savedScrollLeft = 0
      }
      if (scrollContainerRef.value) {
        scrollContainerRef.value.scrollTop = 0
        if (!props.active) {
          scrollContainerRef.value.scrollLeft = 0
        }
      }
    }

    if (!widthsSeededFromRows.value && rows.length > 0) {
      widthsSeededFromRows.value = true
      computeInitialWidths()
    }
  },
  { immediate: true },
)

const rowHeight = computed(() => settings.densityMetrics.gridRowHeight)
const ROW_NUM_WIDTH = 56
const HEADER_HEIGHT = 48

const rowVirtualizer = useVirtualizer(
  computed(() => ({
    count: props.rows.length,
    getScrollElement: () => scrollContainerRef.value,
    estimateSize: () => rowHeight.value,
    overscan: 14,
    initialRect: { width: 1000, height: 480 },
  })),
)

const colVirtualizer = useVirtualizer(
  computed(() => ({
    horizontal: true,
    count: props.columns.length,
    getScrollElement: () => scrollContainerRef.value,
    estimateSize: (index: number) => colWidths.value[index] ?? 160,
    paddingStart: ROW_NUM_WIDTH,
    overscan: 6,
    initialRect: { width: 1000, height: 480 },
  })),
)

watch(
  () => settings.fontScale,
  () => {
    rowVirtualizer.value.measure()
  },
)

watch(
  colWidths,
  () => {
    colVirtualizer.value.measure()
  },
  { flush: 'sync' },
)

function onViewportScroll() {
  if (!props.active || !scrollContainerRef.value) return
  savedScrollTop = scrollContainerRef.value.scrollTop
  savedScrollLeft = scrollContainerRef.value.scrollLeft
}

watch(
  () => props.active,
  async (isActive, wasActive) => {
    if (isActive && !wasActive && scrollContainerRef.value) {
      await nextTick()
      if (!scrollContainerRef.value) return
      scrollContainerRef.value.scrollTop = savedScrollTop
      scrollContainerRef.value.scrollLeft = savedScrollLeft
      rowVirtualizer.value.measure()
      colVirtualizer.value.measure()
    }
  },
)

const virtualRows = computed(() => rowVirtualizer.value.getVirtualItems())
const virtualCols = computed(() => colVirtualizer.value.getVirtualItems())
const totalHeight = computed(() => rowVirtualizer.value.getTotalSize())
const totalWidth = computed(() => Math.max(ROW_NUM_WIDTH, colVirtualizer.value.getTotalSize()))

function cellAt(r: number, c: number): CellValue | undefined {
  return props.rows[r]?.[c]
}

const selectionBounds = computed(() => {
  if (!anchorCell.value || !focusCell.value) return null
  return {
    r1: Math.min(anchorCell.value.row, focusCell.value.row),
    r2: Math.max(anchorCell.value.row, focusCell.value.row),
    c1: Math.min(anchorCell.value.col, focusCell.value.col),
    c2: Math.max(anchorCell.value.col, focusCell.value.col),
  }
})

const singleSelectedCell = computed(() => {
  const b = selectionBounds.value
  if (!b || b.r1 !== b.r2 || b.c1 !== b.c2) return null
  const col = props.columns[b.c1]
  if (!col) return null
  const cell = cellAt(b.r1, b.c1) ?? { kind: ValueKind.KindNull }
  return { row: b.r1, colIdx: b.c1, column: col, cell }
})

function isCellSelected(r: number, c: number): boolean {
  const b = selectionBounds.value
  if (!b) return false
  return r >= b.r1 && r <= b.r2 && c >= b.c1 && c <= b.c2
}

function onCellClick(r: number, c: number, ev: MouseEvent) {
  if (ev.shiftKey && anchorCell.value) {
    focusCell.value = { row: r, col: c }
  } else {
    anchorCell.value = { row: r, col: c }
    focusCell.value = { row: r, col: c }
  }
}

function onCellDblClick(r: number, c: number) {
  const col = props.columns[c]
  const cell = cellAt(r, c) ?? { kind: ValueKind.KindNull }
  if (col) {
    emit('inspect', { column: col, cell, rowIndex: r + props.rowNumberOffset })
  }
}

// Column drag resize (up to 960px) using shared useDragResize composable
function startColumnResize(colIdx: number, ev: MouseEvent) {
  ev.stopPropagation()
  const startWidth = colWidths.value[colIdx] ?? 160
  startDrag(ev, {
    axis: 'x',
    onMove: (delta) => {
      const next = Math.max(64, Math.min(960, startWidth + delta))
      colWidths.value = { ...colWidths.value, [colIdx]: next }
    },
  })
}

// Double-click column separator to Auto-Fit to longest value in current page (up to 800px)
function autoFitColumn(colIdx: number) {
  const col = props.columns[colIdx]
  if (!col) return
  const fit = estimateColumnWidth(col, props.rows, colIdx, { autoFit: true })
  colWidths.value = { ...colWidths.value, [colIdx]: fit }
}

// Live selection summary stats (safe for non-overflow numbers; never mutates display text!)
const selectionStats = computed(() => {
  const b = selectionBounds.value
  if (!b) return null
  let count = 0
  let nonNull = 0
  let numericCount = 0
  let numericSum = 0
  let hasPrecisionWarning = false

  for (let r = b.r1; r <= b.r2; r++) {
    const row = props.rows[r]
    for (let c = b.c1; c <= b.c2; c++) {
      count++
      const cell = row?.[c]
      if (!cell || cell.kind === ValueKind.KindNull) continue
      nonNull++
      if (isNumericKind(cell.kind) && cell.text) {
        if (cell.kind === ValueKind.KindBigInt || cell.kind === ValueKind.KindDecimal) {
          if (cell.text.replace(/[-.]/g, '').length > 15) {
            hasPrecisionWarning = true
            continue
          }
        }
        const n = Number(cell.text)
        if (Number.isFinite(n)) {
          numericCount++
          numericSum += n
        }
      }
    }
  }
  return { count, nonNull, numericCount, numericSum, hasPrecisionWarning }
})

const copyMenuOpen = ref(false)

async function writeClipboardWithFidelity(
  text: string,
  baseLabel: string,
  cols: ColumnMeta[],
  rows: (CellValue[] | null)[],
  format: 'raw' | 'tsv' | 'csv' | 'json' | 'sql',
) {
  const ok = await copyToClipboard(text)
  copyMenuOpen.value = false
  if (!ok) return
  const caveat = formatExportCaveat(inspectExportFidelity(cols, rows), format)
  emit('copied', {
    kind: caveat ? 'warning' : 'success',
    message: baseLabel,
    detail: caveat,
  })
}

function getTargetRowsAndCols(): { cols: ColumnMeta[]; rows: (CellValue[] | null)[] } {
  const b = selectionBounds.value
  if (!b || (b.r1 === b.r2 && b.c1 === b.c2 && props.rows.length > 1)) {
    return { cols: props.columns, rows: props.rows }
  }
  const cols = props.columns.slice(b.c1, b.c2 + 1)
  const rows = props.rows.slice(b.r1, b.r2 + 1).map((r) => (r ? r.slice(b.c1, b.c2 + 1) : null))
  return { cols, rows }
}

function copySelectedCell() {
  const b = selectionBounds.value
  if (!b) return
  const selCols = props.columns.slice(b.c1, b.c2 + 1)
  const selRows: (CellValue[] | null)[] = []
  const lines: string[] = []
  for (let r = b.r1; r <= b.r2; r++) {
    const rowSlice: CellValue[] = []
    const vals: string[] = []
    for (let c = b.c1; c <= b.c2; c++) {
      const cell = cellAt(r, c) ?? { kind: ValueKind.KindNull }
      rowSlice.push(cell)
      // Strictly copies untouched raw cell.text (never visual whitespace symbols!)
      vals.push(cellToPlainText(cell))
    }
    selRows.push(rowSlice)
    lines.push(vals.join('\t'))
  }
  void writeClipboardWithFidelity(lines.join('\n'), '已复制选中单元格原文', selCols, selRows, 'raw')
}

function copyAsTSV() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboardWithFidelity(rowsToTSV(cols, rows), '已复制为 TSV', cols, rows, 'tsv')
}

function copyAsCSV() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboardWithFidelity(rowsToCSV(cols, rows), '已复制为 CSV', cols, rows, 'csv')
}

function copyAsJSON() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboardWithFidelity(rowsToJSON(cols, rows), '已复制为 JSON（大数保真）', cols, rows, 'json')
}

function copyAsInsert() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboardWithFidelity(
    rowsToInsertSQL(props.tableTarget, cols, rows, props.dialect),
    '已复制为 INSERT SQL',
    cols,
    rows,
    'sql',
  )
}

function inspectSelectedCell() {
  const b = selectionBounds.value
  if (!b) return
  onCellDblClick(b.r1, b.c1)
}

function onGridKeydown(ev: KeyboardEvent) {
  if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === 'c' && selectionBounds.value) {
    ev.preventDefault()
    copySelectedCell()
  } else if ((ev.key === ' ' || ev.key === 'Enter') && singleSelectedCell.value) {
    ev.preventDefault()
    inspectSelectedCell()
  }
}

function singleCellLengthLabel(cell: CellValue): string {
  if (cell.kind === ValueKind.KindNull) return 'NULL'
  if (cell.kind === ValueKind.KindBinary) return formatBytes(cell.byteLength)
  const base = `${(cell.text ?? '').length} 字符`
  return cell.truncated ? `${base} · 预览已截断` : base
}
</script>

<template>
  <div
    class="flex flex-col flex-1 min-h-0 overflow-hidden select-none"
    tabindex="0"
    @keydown="onGridKeydown"
  >
    <!-- Virtual Scrollable Grid Viewport (row + column virtualization) -->
    <div ref="scrollContainerRef" class="flex-1 min-h-0 overflow-auto relative bg-canvas" @scroll="onViewportScroll">
      <div v-if="columns.length === 0 && !loading" class="h-full flex items-center justify-center text-xs text-ink-muted">
        暂无可显示的结果列
      </div>

      <div v-else :style="{ width: `${totalWidth}px`, minWidth: '100%' }">
        <!-- Sticky Column Header -->
        <div
          class="sticky top-0 z-20 relative border-b border-border bg-surface"
          :style="{ height: `${HEADER_HEIGHT}px` }"
        >
          <!-- Sticky Left Row-Number Header -->
          <div
            class="sticky left-0 z-30 h-full flex items-center justify-center text-xs font-mono-tabular border-r border-border bg-surface text-ink-faint"
            :style="{ width: `${ROW_NUM_WIDTH}px` }"
          >
            #
          </div>

          <div
            v-for="vCol in virtualCols"
            :key="`${columns[vCol.index]?.name}-${vCol.index}`"
            :data-col-idx="vCol.index"
            class="absolute top-0 bottom-0 left-0 flex flex-col justify-center px-2.5 border-r border-border bg-surface group"
            :class="sortable ? 'cursor-pointer hover:bg-subtle' : ''"
            :style="{
              width: `${vCol.size}px`,
              transform: `translateX(${vCol.start}px)`,
            }"
            @click="sortable && columns[vCol.index] && emit('sort', columns[vCol.index].name)"
          >
            <template v-if="columns[vCol.index]">
              <div class="flex items-center gap-1 min-w-0">
                <KeyRound
                  v-if="columns[vCol.index].isPrimaryKey"
                  class="w-3.5 h-3.5 shrink-0 text-warning"
                  title="主键列"
                />
                <span
                  class="font-medium truncate leading-snug text-ink"
                  style="font-size: var(--grid-header-font-size)"
                  :title="columns[vCol.index].name"
                >
                  {{ columns[vCol.index].name }}
                </span>
                <span v-if="sortable && sortColumn === columns[vCol.index].name" class="ml-auto shrink-0 text-accent">
                  <ArrowUp v-if="sortDir === 'asc'" class="w-3.5 h-3.5" />
                  <ArrowDown v-else-if="sortDir === 'desc'" class="w-3.5 h-3.5" />
                </span>
              </div>
              <div
                class="font-mono-tabular truncate leading-tight mt-0.5 text-ink-faint"
                style="font-size: var(--grid-type-font-size)"
              >
                {{ columns[vCol.index].databaseType || columns[vCol.index].kind }}
              </div>

              <!-- Column Resize & Double-Click Auto-Fit Handle -->
              <div
                class="absolute top-0 right-0 bottom-0 w-1.5 cursor-col-resize hover:bg-accent opacity-0 group-hover:opacity-60 transition-opacity"
                title="拖动调宽 · 双击自动适配该列宽度"
                @mousedown="startColumnResize(vCol.index, $event)"
                @dblclick.stop="autoFitColumn(vCol.index)"
                @click.stop
              />
            </template>
          </div>
        </div>

        <!-- Empty Rows Hint -->
        <div v-if="rows.length === 0 && !loading" class="py-12 text-center text-sm text-ink-muted">
          查询返回 0 行数据
        </div>

        <!-- Virtualized Rows Container (strictly single-line rows, virtualized columns) -->
        <div v-else class="relative" :style="{ height: `${totalHeight}px` }">
          <div
            v-for="vRow in virtualRows"
            :key="vRow.index"
            class="absolute left-0 right-0 border-b border-border"
            :class="vRow.index % 2 === 1 ? 'bg-surface' : 'bg-canvas'"
            :style="{
              height: `${rowHeight}px`,
              transform: `translateY(${vRow.start}px)`,
            }"
          >
            <!-- Sticky Left Row Number -->
            <div
              class="sticky left-0 z-10 h-full flex items-center justify-center text-xs font-mono-tabular border-r border-border bg-surface text-ink-faint"
              :style="{ width: `${ROW_NUM_WIDTH}px` }"
            >
              {{ rowNumberOffset + vRow.index + 1 }}
            </div>

            <!-- Virtualized Data Cells -->
            <div
              v-for="vCol in virtualCols"
              :key="vCol.index"
              class="absolute top-0 bottom-0 left-0 flex items-center px-2.5 border-r border-border overflow-hidden whitespace-nowrap text-ink"
              :class="[
                columns[vCol.index] && isNumericKind(columns[vCol.index].kind) ? 'justify-end font-mono-tabular' : '',
                columns[vCol.index]?.kind === ValueKind.KindDateTime ? 'font-mono-tabular' : '',
                isCellSelected(vRow.index, vCol.index) ? 'bg-selected ring-1 ring-inset ring-accent' : '',
              ]"
              :style="{
                width: `${vCol.size}px`,
                transform: `translateX(${vCol.start}px)`,
                fontSize: 'var(--grid-font-size)',
              }"
              @click="onCellClick(vRow.index, vCol.index, $event)"
              @dblclick="onCellDblClick(vRow.index, vCol.index)"
            >
              <CellText
                :cell="cellAt(vRow.index, vCol.index)"
                :show-whitespace="settings.showWhitespace"
                :show-truncated-badge="true"
              />
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Bottom Status Bar: Single-Cell Full Preview OR Multi-Cell Stats + Whitespace Toggle + Copy/Export -->
    <div class="shrink-0 h-9 px-3 border-t border-border bg-surface text-ink-muted flex items-center justify-between gap-3 text-xs">
      <!-- Left: Live Value Preview (single cell) OR Multi-Cell Selection Stats -->
      <div class="flex items-center gap-2 min-w-0 flex-1 overflow-hidden">
        <template v-if="singleSelectedCell">
          <span class="shrink-0 font-medium text-ink-secondary">
            {{ singleSelectedCell.column.name }}:
          </span>
          <span
            data-testid="selected-cell-preview"
            class="min-w-0 truncate font-mono-tabular select-text text-ink"
            :title="cellToPlainText(singleSelectedCell.cell)"
          >
            <CellText
              :cell="singleSelectedCell.cell"
              :show-whitespace="settings.showWhitespace"
              :show-truncated-badge="true"
            />
          </span>
          <span class="shrink-0 font-mono-tabular text-[11px] text-ink-faint">
            ({{ singleCellLengthLabel(singleSelectedCell.cell) }})
          </span>
        </template>

        <span v-else-if="selectionStats" class="flex items-center gap-2.5 font-mono-tabular truncate">
          <span>已选 {{ selectionStats.count }} 格</span>
          <span>非空 {{ selectionStats.nonNull }}</span>
          <span v-if="selectionStats.numericCount > 1">
            合计 {{ Number(selectionStats.numericSum.toFixed(6)) }} · 均值
            {{ Number((selectionStats.numericSum / selectionStats.numericCount).toFixed(6)) }}
          </span>
          <span v-if="selectionStats.hasPrecisionWarning" class="text-warning">
            (超高精度数已跳过浮点求和)
          </span>
        </span>

        <span v-else class="truncate">双击单元格弹出浮窗查看完整内容 · 双击列分隔线自动适配列宽</span>
      </div>

      <!-- Right: Whitespace Visibility Toggle, Inspect, Copy & Export -->
      <div class="flex items-center gap-1 shrink-0 relative">
        <button
          type="button"
          data-testid="toggle-whitespace-btn"
          class="btn btn-sm"
          :class="settings.showWhitespace ? 'btn-active' : 'btn-ghost'"
          title="显示/隐藏不可见字符（空格 · / 制表符 → / 换行 ↵ / 零宽字符），不影响复制原文"
          @click="settings.toggleWhitespace()"
        >
          <Pilcrow class="w-3.5 h-3.5" />
          空白符
        </button>

        <button
          v-if="selectionBounds"
          type="button"
          class="btn btn-ghost btn-sm"
          title="浮窗查看完整内容 (Space)"
          @click="inspectSelectedCell"
        >
          <Eye class="w-3.5 h-3.5" />
          查看全文
        </button>

        <button
          v-if="selectionBounds"
          type="button"
          class="btn btn-ghost btn-sm"
          title="复制原始值 (Ctrl+C)"
          @click="copySelectedCell"
        >
          <Copy class="w-3.5 h-3.5" />
          复制值
        </button>

        <div class="relative">
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            :disabled="rows.length === 0"
            @click="copyMenuOpen = !copyMenuOpen"
          >
            导出 / 复制 ▾
          </button>

          <div
            v-if="copyMenuOpen"
            class="absolute right-0 bottom-8 z-30 w-56 rounded-lg border border-border-strong bg-elevated py-1 shadow-lg"
            @mouseleave="copyMenuOpen = false"
          >
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle cursor-pointer border-0 bg-transparent"
              @click="copyAsTSV"
            >
              复制为 TSV（换行/制表符转空格）
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle cursor-pointer border-0 bg-transparent"
              @click="copyAsCSV"
            >
              复制为 CSV（NULL 与空串均为空）
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle cursor-pointer border-0 bg-transparent"
              @click="copyAsJSON"
            >
              复制为 JSON（大数保真）
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs text-ink hover:bg-subtle cursor-pointer border-0 bg-transparent"
              @click="copyAsInsert"
            >
              复制为 INSERT SQL（按方言引用）
            </button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
