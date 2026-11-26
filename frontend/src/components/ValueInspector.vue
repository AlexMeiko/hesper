<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Braces, Copy, Download, Pilcrow } from 'lucide-vue-next'
import { streamUrl, ValueKind } from '@/api/types'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { copyToClipboard } from '@/utils/clipboard'
import { formatBytes, formatHexDump, isEmptyStringCell } from '@/utils/format'
import BaseModal from './BaseModal.vue'
import CellText from './CellText.vue'

const settings = useSettingsStore()
const uiStore = useUiStore()

const item = computed(() => uiStore.inspectedCell)

const streamLoading = ref(false)
const streamError = ref('')
const fullText = ref('')
const binaryHex = ref('')
const binaryBytesLen = ref(0)
const prettyJson = ref(true)

watch(
  item,
  async (val) => {
    streamError.value = ''
    fullText.value = ''
    binaryHex.value = ''
    binaryBytesLen.value = 0
    prettyJson.value = true
    if (!val) return

    const { cell } = val
    if (cell.streamRef) {
      streamLoading.value = true
      try {
        const res = await fetch(streamUrl(cell.streamRef))
        if (!res.ok) {
          streamError.value = `按需读取大对象失败 (HTTP ${res.status})，可能已随快照释放`
          return
        }
        if (cell.kind === ValueKind.KindBinary) {
          const buf = new Uint8Array(await res.arrayBuffer())
          binaryBytesLen.value = buf.byteLength
          binaryHex.value = formatHexDump(buf)
        } else {
          fullText.value = await res.text()
        }
      } catch (err) {
        streamError.value = `加载流式内容失败: ${String(err)}`
      } finally {
        streamLoading.value = false
      }
    }
  },
  { immediate: true },
)

const rawText = computed(() => {
  if (!item.value) return ''
  const { cell } = item.value
  if (cell.kind === ValueKind.KindNull) return 'NULL'
  if (cell.kind === ValueKind.KindBinary) {
    return binaryHex.value || cell.text || ''
  }
  return fullText.value || cell.text || ''
})

const displayContent = computed(() => {
  if (!item.value) return ''
  const { cell } = item.value
  if (cell.kind === ValueKind.KindJSON && prettyJson.value) {
    try {
      return JSON.stringify(JSON.parse(rawText.value), null, 2)
    } catch {
      return rawText.value
    }
  }
  return rawText.value
})

const lengthSummary = computed(() => {
  if (!item.value) return ''
  const { cell } = item.value
  if (cell.kind === ValueKind.KindNull) return 'NULL'
  if (cell.kind === ValueKind.KindBinary) {
    return formatBytes(binaryBytesLen.value || cell.byteLength)
  }
  const len = rawText.value.length
  const bytes = new TextEncoder().encode(rawText.value).length
  return bytes !== len ? `${len} 字符 · ${formatBytes(bytes)}` : `${len} 字符`
})

const modalSubtitle = computed(() => {
  if (!item.value) return ''
  const col = item.value.column
  return `第 ${item.value.rowIndex + 1} 行 · ${col.databaseType || col.kind} · ${lengthSummary.value}`
})

async function copyContent() {
  if (!item.value) return
  // Strictly copies raw / formatted value without whitespace visualization glyphs!
  if (await copyToClipboard(displayContent.value)) {
    const isPreviewOnly = Boolean(item.value.cell.truncated && !fullText.value)
    uiStore.notify(
      isPreviewOnly ? 'warning' : 'success',
      isPreviewOnly ? '已复制预览文本（后端截断）' : '已复制完整原文',
    )
  }
}
</script>

<template>
  <BaseModal
    :open="Boolean(item)"
    :title="item?.column.name ?? ''"
    :subtitle="modalSubtitle"
    max-width-class="max-w-2xl"
    max-height-class="max-h-[78vh]"
    test-id="value-inspector-modal"
    @close="uiStore.closeInspector()"
  >
    <template #toolbar>
      <div
        v-if="item"
        class="px-4 py-2 border-b border-border bg-surface flex flex-wrap items-center justify-between gap-2 text-xs shrink-0"
      >
        <div class="flex items-center gap-1.5 text-[11px] text-ink-muted">
          <span>只读预览</span>
          <span v-if="isEmptyStringCell(item.cell)">· 空字符串（长度 0，非 NULL）</span>
          <span v-else-if="item.cell.truncated && !fullText" class="text-warning">
            · 当前仅展示截断预览{{ item.cell.streamRef ? '' : '（无流式原文句柄）' }}
          </span>
        </div>

        <div class="flex items-center gap-1.5">
          <button
            v-if="item.cell.kind === ValueKind.KindJSON"
            type="button"
            class="btn btn-xs"
            :class="prettyJson ? 'btn-active' : ''"
            @click="prettyJson = !prettyJson"
          >
            <Braces class="w-3 h-3" />
            {{ prettyJson ? '已格式化' : '原始 JSON' }}
          </button>

          <button
            v-if="item.cell.kind !== ValueKind.KindNull && item.cell.kind !== ValueKind.KindBinary"
            type="button"
            class="btn btn-xs"
            :class="settings.showWhitespace ? 'btn-active' : ''"
            title="显示/隐藏空格、制表符、换行与零宽字符"
            @click="settings.toggleWhitespace()"
          >
            <Pilcrow class="w-3 h-3" />
            空白符
          </button>

          <button type="button" class="btn btn-xs" @click="copyContent">
            <Copy class="w-3 h-3" />
            复制原文
          </button>

          <a
            v-if="item.cell.streamRef"
            :href="streamUrl(item.cell.streamRef, true)"
            download
            class="btn btn-xs no-underline"
          >
            <Download class="w-3 h-3" />
            下载
          </a>
        </div>
      </div>
    </template>

    <!-- Scrollable Full-Value Body -->
    <div v-if="item" class="flex-1 min-h-0 overflow-y-auto p-4 bg-canvas">
      <div v-if="streamLoading" class="text-xs py-10 text-center text-ink-muted">
        正在通过 /stream 按需加载完整数据...
      </div>

      <div v-else-if="streamError" class="rounded-md p-3 text-xs bg-danger-soft text-danger">
        {{ streamError }}
      </div>

      <div v-else-if="item.cell.kind === ValueKind.KindNull" class="py-8 text-center text-sm">
        <CellText :cell="item.cell" />
      </div>

      <div
        v-else-if="isEmptyStringCell(item.cell)"
        class="py-8 text-center font-mono-tabular text-xs text-ink-faint"
      >
        ""（空字符串，长度为 0，与 NULL 不同）
      </div>

      <pre
        v-else
        data-testid="value-inspector-content"
        class="m-0 font-mono-tabular whitespace-pre-wrap break-all select-text leading-relaxed text-ink"
        style="font-size: var(--editor-font-size)"
      ><CellText
        :cell="item.cell"
        :override-text="displayContent"
        :show-whitespace="settings.showWhitespace"
        :preserve-line-breaks="true"
        :hide-binary-prefix="true"
        :single-line="false"
      /></pre>
    </div>
  </BaseModal>
</template>
