<script setup lang="ts">
import { ChevronLeft, ChevronRight } from 'lucide-vue-next'

withDefaults(
  defineProps<{
    page: number
    pageSize: number
    hasMore: boolean
    loading?: boolean
    pageSizeOptions?: number[]
  }>(),
  {
    loading: false,
    pageSizeOptions: () => [100, 200, 500, 1000],
  },
)

const emit = defineEmits<{
  (e: 'update:page', page: number): void
  (e: 'update:pageSize', size: number): void
}>()

function onPageSizeChange(ev: Event) {
  const val = Number((ev.target as HTMLSelectElement).value)
  if (val > 0) {
    emit('update:pageSize', val)
  }
}
</script>

<template>
  <div class="flex flex-wrap items-center gap-2 text-xs">
    <slot name="prefix" />

    <button
      type="button"
      class="btn"
      :disabled="page <= 1 || loading"
      @click="emit('update:page', page - 1)"
    >
      <ChevronLeft class="w-3.5 h-3.5" />
      上一页
    </button>

    <span class="font-mono-tabular text-[13px] px-1">第 {{ page }} 页</span>

    <button
      type="button"
      class="btn"
      :disabled="!hasMore || loading"
      @click="emit('update:page', page + 1)"
    >
      下一页
      <ChevronRight class="w-3.5 h-3.5" />
    </button>

    <select
      :value="pageSize"
      class="input-field select-sm min-w-[108px]"
      :disabled="loading"
      @change="onPageSizeChange"
    >
      <option v-for="size in pageSizeOptions" :key="size" :value="size">
        {{ size }} 行/页
      </option>
    </select>

    <slot name="summary" />
  </div>
</template>
