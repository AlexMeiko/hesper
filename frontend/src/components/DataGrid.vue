<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useVirtualizer } from '@tanstack/vue-virtual'
import { ArrowDown, ArrowUp, Copy, Eye, KeyRound, Pilcrow } from 'lucide-vue-next'
import { ValueKind, type CellValue, type ColumnMeta } from '@/api/client'
import { useSettingsStore } from '@/stores/settings'
import {
  cellToPlainText,
  estimateColumnWidth,
  formatBytes,
  isEmptyStringCell,
  isNumericKind,
  rowsToCSV,
  rowsToInsertSQL,
  rowsToJSON,
  rowsToTSV,
  tokenizeWhitespace,
} from '@/utils/format'

const props = withDefaults(
  defineProps<{
    columns: ColumnMeta[]
    rows: (CellValue[] | null)[]
    rowNumberOffset?: number
    sortable?: boolean
    sortColumn?: string
    sortDir?: '' | 'asc' | 'desc'
    tableName?: string
    loading?: boolean
  }>(),
  {
    rowNumberOffset: 0,
    sortable: false,
    sortColumn: '',
    sortDir: '',
    tableName: '',
    loading: false,
  },
)

const emit = defineEmits<{
  (e: 'sort', colName: string): void
  (e: 'inspect', payload: { column: ColumnMeta; cell: CellValue; rowIndex: number }): void
  (e: 'copied', label: string): void
}>()

const settings = useSettingsStore()

const scrollContainerRef = ref<HTMLElement | null>(null)
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
// Preserves user-resized widths when paging within the same result/table!
watch(
  [columnSignature, () => props.rows],
  ([sig, rows]) => {
    if (sig !== lastColSig.value) {
      lastColSig.value = sig
      widthsSeededFromRows.value = rows.length > 0
      computeInitialWidths()
      anchorCell.value = null
      focusCell.value = null
      return
    }
    if (!widthsSeededFromRows.value && rows.length > 0) {
      widthsSeededFromRows.value = true
      computeInitialWidths()
    }
  },
  { immediate: true },
)

const rowHeight = computed(() => {
  switch (settings.fontScale) {
    case 'compact':
      return 28
    case 'comfortable':
      return 34
    default:
      return 31
  }
})
const ROW_NUM_WIDTH = 56

const rowVirtualizer = useVirtualizer(
  computed(() => ({
    count: props.rows.length,
    getScrollElement: () => scrollContainerRef.value,
    estimateSize: () => rowHeight.value,
    overscan: 14,
    initialRect: { width: 1000, height: 480 },
  })),
)

watch(
  () => settings.fontScale,
  () => {
    rowVirtualizer.value.measure()
  },
)

const virtualRows = computed(() => rowVirtualizer.value.getVirtualItems())
const totalHeight = computed(() => rowVirtualizer.value.getTotalSize())

const totalWidth = computed(() => {
  let w = ROW_NUM_WIDTH
  for (let i = 0; i < props.columns.length; i++) {
    w += colWidths.value[i] ?? 160
  }
  return w
})

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
  const cell = props.rows[b.r1]?.[b.c1] ?? { kind: ValueKind.KindNull }
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
  const cell = props.rows[r]?.[c] ?? { kind: ValueKind.KindNull }
  if (col) {
    emit('inspect', { column: col, cell, rowIndex: r + props.rowNumberOffset })
  }
}

// Column drag resize (up to 960px)
function startResize(colIdx: number, ev: MouseEvent) {
  ev.stopPropagation()
  ev.preventDefault()
  const startX = ev.clientX
  const startWidth = colWidths.value[colIdx] ?? 160

  const onMove = (moveEv: MouseEvent) => {
    const next = Math.max(64, Math.min(960, startWidth + (moveEv.clientX - startX)))
    colWidths.value = { ...colWidths.value, [colIdx]: next }
  }
  const onUp = () => {
    window.removeEventListener('mousemove', onMove)
    window.removeEventListener('mouseup', onUp)
  }
  window.addEventListener('mousemove', onMove)
  window.addEventListener('mouseup', onUp)
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

async function writeClipboard(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    emit('copied', label)
  } catch {
    const el = document.createElement('textarea')
    el.value = text
    document.body.appendChild(el)
    el.select()
    document.execCommand('copy')
    document.body.removeChild(el)
    emit('copied', label)
  }
  copyMenuOpen.value = false
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
  const lines: string[] = []
  for (let r = b.r1; r <= b.r2; r++) {
    const vals: string[] = []
    for (let c = b.c1; c <= b.c2; c++) {
      // Strictly copies untouched raw cell.text (never visual whitespace symbols!)
      vals.push(cellToPlainText(props.rows[r]?.[c]))
    }
    lines.push(vals.join('\t'))
  }
  void writeClipboard(lines.join('\n'), '已复制选中单元格原文')
}

function copyAsTSV() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboard(rowsToTSV(cols, rows), '已复制为 TSV')
}

function copyAsCSV() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboard(rowsToCSV(cols, rows), '已复制为 CSV')
}

function copyAsJSON() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboard(rowsToJSON(cols, rows), '已复制为 JSON（大数保真）')
}

function copyAsInsert() {
  const { cols, rows } = getTargetRowsAndCols()
  void writeClipboard(rowsToInsertSQL(props.tableName, cols, rows), '已复制为 INSERT SQL')
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
  return `${(cell.text ?? '').length} 字符`
}
</script>

<template>
  <div
    class="flex flex-col flex-1 min-h-0 overflow-hidden select-none"
    tabindex="0"
    @keydown="onGridKeydown"
  >
    <!-- Virtual Scrollable Grid Viewport (horizontal & vertical scrolling) -->
    <div
      ref="scrollContainerRef"
      class="flex-1 min-h-0 overflow-auto relative"
      style="background: var(--bg-canvas)"
    >
      <div
        v-if="columns.length === 0 && !loading"
        class="h-full flex items-center justify-center text-xs"
        style="color: var(--text-muted)"
      >
        暂无可显示的结果列
      </div>

      <div v-else :style="{ width: `${totalWidth}px`, minWidth: '100%' }">
        <!-- Sticky Column Header -->
        <div
          class="sticky top-0 z-20 flex border-b"
          style="background: var(--bg-surface); border-color: var(--border-subtle)"
        >
          <!-- Sticky Left Row-Number Header -->
          <div
            class="sticky left-0 z-30 shrink-0 flex items-center justify-center text-xs font-mono-tabular border-r"
            :style="{
              width: `${ROW_NUM_WIDTH}px`,
              background: 'var(--bg-surface)',
              borderColor: 'var(--border-subtle)',
              color: 'var(--text-faint)',
            }"
          >
            #
          </div>

          <div
            v-for="(col, colIdx) in columns"
            :key="`${col.name}-${colIdx}`"
            :data-col-idx="colIdx"
            class="shrink-0 relative flex flex-col justify-center px-2.5 py-2 border-r group"
            :class="sortable ? 'cursor-pointer hover:bg-[var(--bg-subtle)]' : ''"
            :style="{ width: `${colWidths[colIdx] ?? 160}px`, borderColor: 'var(--border-subtle)' }"
            @click="sortable && emit('sort', col.name)"
          >
            <div class="flex items-center gap-1 min-w-0">
              <KeyRound
                v-if="col.isPrimaryKey"
                class="w-3.5 h-3.5 shrink-0"
                style="color: var(--warning)"
                title="主键列"
              />
              <span
                class="font-medium truncate leading-snug"
                style="color: var(--text-primary); font-size: var(--grid-header-font-size)"
                :title="col.name"
              >
                {{ col.name }}
              </span>
              <span v-if="sortable && sortColumn === col.name" class="ml-auto shrink-0" style="color: var(--accent)">
                <ArrowUp v-if="sortDir === 'asc'" class="w-3.5 h-3.5" />
                <ArrowDown v-else-if="sortDir === 'desc'" class="w-3.5 h-3.5" />
              </span>
            </div>
            <div
              class="font-mono-tabular truncate leading-tight mt-0.5"
              style="color: var(--text-faint); font-size: var(--grid-type-font-size)"
            >
              {{ col.databaseType || col.kind }}
            </div>

            <!-- Column Resize & Double-Click Auto-Fit Handle -->
            <div
              class="absolute top-0 right-0 bottom-0 w-1.5 cursor-col-resize hover:bg-[var(--accent)] opacity-0 group-hover:opacity-60 transition-opacity"
              title="拖动调宽 · 双击自动适配该列宽度"
              @mousedown="startResize(colIdx, $event)"
              @dblclick.stop="autoFitColumn(colIdx)"
              @click.stop
            />
          </div>
        </div>

        <!-- Empty Rows Hint -->
        <div
          v-if="rows.length === 0 && !loading"
          class="py-12 text-center text-sm"
          style="color: var(--text-muted)"
        >
          查询返回 0 行数据
        </div>

        <!-- Virtualized Rows Container (strictly single-line rows) -->
        <div v-else class="relative" :style="{ height: `${totalHeight}px` }">
          <div
            v-for="vRow in virtualRows"
            :key="vRow.index"
            class="absolute left-0 right-0 flex border-b"
            :style="{
              height: `${rowHeight}px`,
              transform: `translateY(${vRow.start}px)`,
              borderColor: 'var(--border-subtle)',
              background: vRow.index % 2 === 1 ? 'var(--bg-surface)' : 'var(--bg-canvas)',
            }"
          >
            <!-- Sticky Left Row Number -->
            <div
              class="sticky left-0 z-10 shrink-0 flex items-center justify-center text-xs font-mono-tabular border-r"
              :style="{
                width: `${ROW_NUM_WIDTH}px`,
                borderColor: 'var(--border-subtle)',
                color: 'var(--text-faint)',
                background: 'var(--bg-surface)',
              }"
            >
              {{ rowNumberOffset + vRow.index + 1 }}
            </div>

            <!-- Data Cells (clean pill-free typography, strictly single-line) -->
            <div
              v-for="(col, colIdx) in columns"
              :key="colIdx"
              class="shrink-0 flex items-center px-2.5 border-r overflow-hidden whitespace-nowrap"
              :class="[
                isNumericKind(col.kind) ? 'justify-end font-mono-tabular' : '',
                col.kind === ValueKind.KindDateTime ? 'font-mono-tabular' : '',
              ]"
              :style="{
                width: `${colWidths[colIdx] ?? 160}px`,
                fontSize: 'var(--grid-font-size)',
                borderColor: 'var(--border-subtle)',
                color: 'var(--text-primary)',
                background: isCellSelected(vRow.index, colIdx) ? 'var(--bg-selected)' : undefined,
                boxShadow: isCellSelected(vRow.index, colIdx) ? 'inset 0 0 0 1px var(--accent)' : undefined,
              }"
              @click="onCellClick(vRow.index, colIdx, $event)"
              @dblclick="onCellDblClick(vRow.index, colIdx)"
            >
              <!-- 1. NULL: clean plain gray italic text (no pill) -->
              <span
                v-if="!rows[vRow.index]?.[colIdx] || rows[vRow.index]?.[colIdx]?.kind === ValueKind.KindNull"
                data-cell-kind="null"
                class="italic font-mono-tabular select-none"
                style="color: var(--text-faint)"
              >
                NULL
              </span>

              <!-- 2. Empty String "": clean borderless pale text (distinct from NULL and non-empty values) -->
              <span
                v-else-if="isEmptyStringCell(rows[vRow.index]?.[colIdx])"
                data-cell-kind="empty-string"
                class="font-mono-tabular select-none"
                style="color: var(--text-faint)"
                title="空字符串（长度 0，非 NULL）"
              >
                ""
              </span>

              <!-- 3. Binary / BLOB: clean muted prefix + hex preview (no pill) -->
              <span
                v-else-if="rows[vRow.index]?.[colIdx]?.kind === ValueKind.KindBinary"
                data-cell-kind="binary"
                class="truncate font-mono-tabular text-xs"
                title="双击弹出浮窗查看完整十六进制或下载"
              >
                <span style="color: var(--text-muted)">BLOB {{ formatBytes(rows[vRow.index]?.[colIdx]?.byteLength) }}</span>
                <span v-if="rows[vRow.index]?.[colIdx]?.text" class="ml-1.5" style="color: var(--text-primary)">
                  {{ rows[vRow.index]?.[colIdx]?.text }}
                </span>
              </span>

              <!-- 4. All non-null values (BIGINT / DECIMAL / String / JSON / DateTime / Bool): high-contrast primary color, strictly single-line -->
              <span
                v-else
                :data-cell-kind="rows[vRow.index]?.[colIdx]?.kind"
                class="truncate"
                :class="rows[vRow.index]?.[colIdx]?.kind === ValueKind.KindJSON ? 'font-mono-tabular' : ''"
                :title="rows[vRow.index]?.[colIdx]?.text"
              >
                <template v-if="settings.showWhitespace">
                  <template
                    v-for="(tok, tIdx) in tokenizeWhitespace(rows[vRow.index]?.[colIdx]?.text ?? '', false)"
                    :key="tIdx"
                  >
                    <span
                      v-if="tok.kind !== 'text'"
                      :data-ws-token="tok.kind"
                      class="font-mono-tabular"
                      :style="{ color: tok.kind === 'special' ? 'var(--warning)' : 'var(--text-faint)' }"
                    >{{ tok.text }}</span>
                    <template v-else>{{ tok.text }}</template>
                  </template>
                </template>
                <template v-else>
                  {{ rows[vRow.index]?.[colIdx]?.text }}
                </template>
              </span>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Bottom Status Bar: Single-Cell Full Preview OR Multi-Cell Stats + Whitespace Toggle + Copy/Export -->
    <div
      class="shrink-0 h-9 px-3 border-t flex items-center justify-between gap-3 text-xs"
      style="background: var(--bg-surface); border-color: var(--border-subtle); color: var(--text-muted)"
    >
      <!-- Left: Live Value Preview (single cell) OR Multi-Cell Selection Stats -->
      <div class="flex items-center gap-2 min-w-0 flex-1 overflow-hidden">
        <template v-if="singleSelectedCell">
          <span class="shrink-0 font-medium" style="color: var(--text-secondary)">
            {{ singleSelectedCell.column.name }}:
          </span>
          <span
            data-testid="selected-cell-preview"
            class="truncate font-mono-tabular select-text"
            :style="{
              color:
                singleSelectedCell.cell.kind === ValueKind.KindNull || isEmptyStringCell(singleSelectedCell.cell)
                  ? 'var(--text-faint)'
                  : 'var(--text-primary)',
            }"
            :title="cellToPlainText(singleSelectedCell.cell)"
          >
            <template v-if="singleSelectedCell.cell.kind === ValueKind.KindNull">NULL</template>
            <template v-else-if="isEmptyStringCell(singleSelectedCell.cell)">""</template>
            <template v-else-if="settings.showWhitespace">
              <template
                v-for="(tok, tIdx) in tokenizeWhitespace(singleSelectedCell.cell.text ?? '', false)"
                :key="tIdx"
              >
                <span
                  v-if="tok.kind !== 'text'"
                  :style="{ color: tok.kind === 'special' ? 'var(--warning)' : 'var(--text-faint)' }"
                >{{ tok.text }}</span>
                <template v-else>{{ tok.text }}</template>
              </template>
            </template>
            <template v-else>{{ singleSelectedCell.cell.text }}</template>
          </span>
          <span class="shrink-0 font-mono-tabular text-[11px]" style="color: var(--text-faint)">
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
          <span v-if="selectionStats.hasPrecisionWarning" style="color: var(--warning)">
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
          class="btn !h-7 !px-2 !text-xs"
          :class="
            settings.showWhitespace
              ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]'
              : 'btn-ghost'
          "
          title="显示/隐藏不可见字符（空格 · / 制表符 → / 换行 ↵ / 零宽字符），不影响复制原文"
          @click="settings.toggleWhitespace()"
        >
          <Pilcrow class="w-3.5 h-3.5" />
          空白符
        </button>

        <button
          v-if="selectionBounds"
          type="button"
          class="btn btn-ghost !h-7 !px-2 !text-xs"
          title="浮窗查看完整内容 (Space)"
          @click="inspectSelectedCell"
        >
          <Eye class="w-3.5 h-3.5" />
          查看全文
        </button>

        <button
          v-if="selectionBounds"
          type="button"
          class="btn btn-ghost !h-7 !px-2 !text-xs"
          title="复制原始值 (Ctrl+C)"
          @click="copySelectedCell"
        >
          <Copy class="w-3.5 h-3.5" />
          复制值
        </button>

        <div class="relative">
          <button
            type="button"
            class="btn btn-ghost !h-7 !px-2 !text-xs"
            :disabled="rows.length === 0"
            @click="copyMenuOpen = !copyMenuOpen"
          >
            导出 / 复制 ▾
          </button>

          <div
            v-if="copyMenuOpen"
            class="absolute right-0 bottom-7 z-30 w-44 rounded-lg border py-1 shadow-lg"
            style="background: var(--bg-elevated); border-color: var(--border-strong)"
            @mouseleave="copyMenuOpen = false"
          >
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] cursor-pointer border-0 bg-transparent"
              style="color: var(--text-primary)"
              @click="copyAsTSV"
            >
              复制为 TSV (Excel 友好)
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] cursor-pointer border-0 bg-transparent"
              style="color: var(--text-primary)"
              @click="copyAsCSV"
            >
              复制为 CSV
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] cursor-pointer border-0 bg-transparent"
              style="color: var(--text-primary)"
              @click="copyAsJSON"
            >
              复制为 JSON（大数保真）
            </button>
            <button
              type="button"
              class="w-full text-left px-3 py-1.5 text-xs hover:bg-[var(--bg-subtle)] cursor-pointer border-0 bg-transparent"
              style="color: var(--text-primary)"
              @click="copyAsInsert"
            >
              复制为 INSERT SQL
            </button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
