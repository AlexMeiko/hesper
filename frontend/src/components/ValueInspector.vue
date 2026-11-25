<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Braces, Copy, Download, Pilcrow, X } from 'lucide-vue-next'
import { api, ValueKind } from '@/api/client'
import { useSettingsStore } from '@/stores/settings'
import type { InspectedCell } from '@/stores/workspace'
import { formatBytes, formatHexDump, isEmptyStringCell, tokenizeWhitespace } from '@/utils/format'

const props = defineProps<{
  item: InspectedCell | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'copied', label: string): void
}>()

const settings = useSettingsStore()

const streamLoading = ref(false)
const streamError = ref('')
const fullText = ref('')
const binaryHex = ref('')
const binaryBytesLen = ref(0)
const prettyJson = ref(true)

watch(
  () => props.item,
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
        const res = await fetch(api.streamUrl(cell.streamRef))
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
  if (!props.item) return ''
  const { cell } = props.item
  if (cell.kind === ValueKind.KindNull) return 'NULL'
  if (cell.kind === ValueKind.KindBinary) {
    return binaryHex.value || cell.text || ''
  }
  return fullText.value || cell.text || ''
})

const displayContent = computed(() => {
  if (!props.item) return ''
  const { cell } = props.item
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
  if (!props.item) return ''
  const { cell } = props.item
  if (cell.kind === ValueKind.KindNull) return 'NULL'
  if (cell.kind === ValueKind.KindBinary) {
    return formatBytes(binaryBytesLen.value || cell.byteLength)
  }
  const len = rawText.value.length
  const bytes = new TextEncoder().encode(rawText.value).length
  return bytes !== len ? `${len} 字符 · ${formatBytes(bytes)}` : `${len} 字符`
})

async function copyContent() {
  if (!props.item) return
  try {
    // Strictly copies raw / formatted value without whitespace visualization glyphs!
    await navigator.clipboard.writeText(displayContent.value)
    emit('copied', '已复制完整原文')
  } catch {
    const el = document.createElement('textarea')
    el.value = displayContent.value
    document.body.appendChild(el)
    el.select()
    document.execCommand('copy')
    document.body.removeChild(el)
    emit('copied', '已复制完整原文')
  }
}

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Escape' && props.item) {
    ev.preventDefault()
    ev.stopPropagation()
    emit('close')
  }
}

onMounted(() => window.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
  <div
    v-if="item"
    data-testid="value-inspector-modal"
    class="fixed inset-0 z-50 flex items-center justify-center p-6"
    style="background: rgba(15, 23, 42, 0.42); backdrop-filter: blur(2px)"
    @click.self="emit('close')"
  >
    <div
      class="w-full max-w-2xl max-h-[78vh] rounded-xl border shadow-2xl flex flex-col overflow-hidden"
      style="background: var(--bg-elevated); border-color: var(--border-strong)"
    >
      <!-- Header -->
      <div
        class="px-4 py-3 border-b flex items-center justify-between gap-3 shrink-0"
        style="border-color: var(--border-subtle)"
      >
        <div class="min-w-0">
          <div class="font-semibold text-sm truncate" style="color: var(--text-primary)">
            {{ item.column.name }}
          </div>
          <div class="text-[11px] font-mono-tabular truncate" style="color: var(--text-muted)">
            第 {{ item.rowIndex + 1 }} 行 · {{ item.column.databaseType || item.column.kind }} · {{ lengthSummary }}
          </div>
        </div>
        <button
          type="button"
          class="btn btn-ghost !w-7 !h-7 !p-0 shrink-0"
          title="关闭浮窗 (Esc)"
          @click="emit('close')"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Toolbar -->
      <div
        class="px-4 py-2 border-b flex flex-wrap items-center justify-between gap-2 text-xs shrink-0"
        style="background: var(--bg-surface); border-color: var(--border-subtle)"
      >
        <div class="flex items-center gap-1.5 text-[11px]" style="color: var(--text-muted)">
          <span>只读预览</span>
          <span v-if="isEmptyStringCell(item.cell)">· 空字符串（长度 0，非 NULL）</span>
        </div>

        <div class="flex items-center gap-1.5">
          <button
            v-if="item.cell.kind === ValueKind.KindJSON"
            type="button"
            class="btn !h-6 !px-2 !text-[11px]"
            :class="prettyJson ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]' : ''"
            @click="prettyJson = !prettyJson"
          >
            <Braces class="w-3 h-3" />
            {{ prettyJson ? '已格式化' : '原始 JSON' }}
          </button>

          <button
            v-if="item.cell.kind !== ValueKind.KindNull && item.cell.kind !== ValueKind.KindBinary"
            type="button"
            class="btn !h-6 !px-2 !text-[11px]"
            :class="
              settings.showWhitespace
                ? '!border-[var(--accent)] !bg-[var(--accent-soft)] !text-[var(--accent-text)]'
                : ''
            "
            title="显示/隐藏空格、制表符、换行与零宽字符"
            @click="settings.toggleWhitespace()"
          >
            <Pilcrow class="w-3 h-3" />
            空白符
          </button>

          <button type="button" class="btn !h-6 !px-2 !text-[11px]" @click="copyContent">
            <Copy class="w-3 h-3" />
            复制原文
          </button>

          <a
            v-if="item.cell.streamRef"
            :href="api.streamUrl(item.cell.streamRef, true)"
            download
            class="btn !h-6 !px-2 !text-[11px] no-underline"
          >
            <Download class="w-3 h-3" />
            下载
          </a>
        </div>
      </div>

      <!-- Scrollable Full-Value Body -->
      <div class="flex-1 min-h-0 overflow-y-auto p-4" style="background: var(--bg-canvas)">
        <div v-if="streamLoading" class="text-xs py-10 text-center" style="color: var(--text-muted)">
          正在通过 /stream 按需加载完整数据...
        </div>

        <div
          v-else-if="streamError"
          class="rounded-md p-3 text-xs"
          style="background: var(--danger-soft); color: var(--danger)"
        >
          {{ streamError }}
        </div>

        <div
          v-else-if="item.cell.kind === ValueKind.KindNull"
          class="py-8 text-center italic font-mono-tabular text-sm"
          style="color: var(--text-faint)"
        >
          NULL
        </div>

        <div
          v-else-if="isEmptyStringCell(item.cell)"
          class="py-8 text-center font-mono-tabular text-xs"
          style="color: var(--text-faint)"
        >
          ""（空字符串，长度为 0，与 NULL 不同）
        </div>

        <pre
          v-else
          data-testid="value-inspector-content"
          class="m-0 font-mono-tabular text-xs whitespace-pre-wrap break-all select-text leading-relaxed"
          style="color: var(--text-primary)"
        ><template v-if="settings.showWhitespace && item.cell.kind !== ValueKind.KindBinary"><template
            v-for="(tok, tIdx) in tokenizeWhitespace(displayContent, true)"
            :key="tIdx"
          ><span
            v-if="tok.kind !== 'text'"
            :data-ws-token="tok.kind"
            :style="{ color: tok.kind === 'special' ? 'var(--warning)' : 'var(--text-faint)' }"
          >{{ tok.text }}</span><template v-else>{{ tok.text }}</template><template v-if="tok.lineBreakAfter">{{ '\n' }}</template></template></template><template v-else>{{ displayContent }}</template></pre>
      </div>
    </div>
  </div>
</template>
