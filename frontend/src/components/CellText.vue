<script setup lang="ts">
import { computed } from 'vue'
import { ValueKind, type CellValue } from '@/api/types'
import { formatBytes, isEmptyStringCell, tokenizeWhitespace } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    cell: CellValue | null | undefined
    overrideText?: string
    showWhitespace?: boolean
    preserveLineBreaks?: boolean
    showTruncatedBadge?: boolean
    hideBinaryPrefix?: boolean
    singleLine?: boolean
  }>(),
  {
    overrideText: undefined,
    showWhitespace: false,
    preserveLineBreaks: false,
    showTruncatedBadge: false,
    hideBinaryPrefix: false,
    singleLine: true,
  },
)

const isNull = computed(() => !props.cell || props.cell.kind === ValueKind.KindNull)
const isEmpty = computed(() => props.overrideText === undefined && isEmptyStringCell(props.cell))
const isBinary = computed(() => props.cell?.kind === ValueKind.KindBinary)
const displayText = computed(() => props.overrideText ?? props.cell?.text ?? '')
const tokens = computed(() => {
  if (!props.showWhitespace || isBinary.value) return null
  return tokenizeWhitespace(displayText.value, props.preserveLineBreaks)
})
</script>

<template>
  <!-- 1. NULL: clean plain gray italic text (no pill) -->
  <span
    v-if="isNull"
    data-cell-kind="null"
    class="italic font-mono-tabular select-none text-ink-faint"
  >NULL</span>

  <!-- 2. Empty String "": borderless pale indicator distinct from NULL -->
  <span
    v-else-if="isEmpty"
    data-cell-kind="empty-string"
    class="font-mono-tabular select-none text-ink-faint"
    title="空字符串（长度 0，非 NULL）"
  >""</span>

  <!-- 3. Binary / BLOB: muted size prefix + hex preview -->
  <span
    v-else-if="isBinary && !hideBinaryPrefix"
    data-cell-kind="binary"
    class="inline-flex items-center gap-1.5 min-w-0 font-mono-tabular text-xs"
    :class="singleLine ? 'truncate' : ''"
    title="双击弹出浮窗查看完整十六进制或下载"
  >
    <span class="text-ink-muted shrink-0">BLOB {{ formatBytes(cell?.byteLength) }}</span>
    <span v-if="cell?.text" class="truncate text-ink">{{ cell.text }}</span>
  </span>

  <!-- 4. All non-null textual/numeric/JSON/datetime/bool values -->
  <span
    v-else
    :data-cell-kind="cell?.kind"
    class="min-w-0"
    :class="[
      singleLine ? 'inline-flex items-center max-w-full' : '',
      cell?.kind === ValueKind.KindJSON ? 'font-mono-tabular' : '',
    ]"
    :title="singleLine ? displayText : undefined"
  >
    <span :class="singleLine ? 'truncate' : ''">
      <template v-if="tokens">
        <template v-for="(tok, tIdx) in tokens" :key="tIdx"><span
          v-if="tok.kind !== 'text'"
          :data-ws-token="tok.kind"
          class="font-mono-tabular"
          :class="tok.kind === 'special' ? 'text-warning' : 'text-ink-faint'"
        >{{ tok.text }}</span><template v-else>{{ tok.text }}</template><template v-if="tok.lineBreakAfter">{{ '\n' }}</template></template>
      </template>
      <template v-else>{{ displayText }}</template>
    </span>

    <span
      v-if="showTruncatedBadge && cell?.truncated"
      data-cell-truncated="true"
      class="ml-1.5 shrink-0 px-1 rounded text-[10px] font-mono-tabular bg-warning-soft text-warning"
      title="该单元格内容已在预览中被后端截断，双击查看或下载完整原文"
    >截断</span>
  </span>
</template>
