<script setup lang="ts">
import { computed } from 'vue'
import { AlertOctagon, AlertTriangle, Ban, Info, RefreshCw, RotateCcw, ShieldAlert } from 'lucide-vue-next'
import { ErrorCode, SessionState, type AppError } from '@/api/types'

const props = defineProps<{
  error?: AppError | null
  sessionState?: SessionState
  snapshotExpired?: boolean
  expiredMessage?: string
  noticeMessage?: string
}>()

const emit = defineEmits<{
  (e: 'reconnect'): void
  (e: 'rollback'): void
  (e: 'rerun-expired'): void
}>()

const isCommitUnknown = computed(
  () =>
    props.sessionState === SessionState.SessionCommitUnknown ||
    props.error?.code === ErrorCode.ErrCommitOutcomeUnknown,
)

const isSessionLost = computed(
  () =>
    !isCommitUnknown.value &&
    (props.sessionState === SessionState.SessionLost || props.error?.code === ErrorCode.ErrSessionLost),
)

const isFailedTx = computed(
  () => !isCommitUnknown.value && !isSessionLost.value && props.sessionState === SessionState.SessionAliveFailedTx,
)

const isCanceled = computed(() => props.error?.code === ErrorCode.ErrStatementCanceled)
const isReadOnlyDenied = computed(() => props.error?.code === ErrorCode.ErrReadOnlyDenied)
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <!-- 1. HIGHEST SEVERITY: Commit / Write Outcome Unknown — NEVER show a retry button! -->
    <div
      v-if="isCommitUnknown"
      data-testid="commit-unknown-banner"
      class="flex items-start justify-between gap-3 rounded-lg border border-danger bg-danger-soft text-danger px-3 py-2.5 text-xs"
    >
      <div class="flex items-start gap-2.5 min-w-0">
        <AlertOctagon class="w-4 h-4 shrink-0 mt-0.5" />
        <div class="space-y-0.5 min-w-0">
          <div class="font-semibold text-[13px]">写入或提交结果未知（请勿自动重试）</div>
          <p class="m-0 leading-relaxed opacity-95">
            {{
              error?.message ||
              '请求发出后连接中断，无法确认数据库是否已完成提交。请先核对目标表数据状态，切勿盲目重复执行写入。'
            }}
          </p>
          <p v-if="error?.detail" class="m-0 font-mono-tabular text-[11px] opacity-80 break-all">
            {{ error.detail }}
          </p>
        </div>
      </div>
      <div class="flex items-center gap-2 shrink-0">
        <button
          type="button"
          data-testid="commit-unknown-reconnect-btn"
          class="btn border-danger text-danger"
          @click="emit('reconnect')"
        >
          <RefreshCw class="w-3.5 h-3.5" />
          我已核对，建立新会话
        </button>
      </div>
    </div>

    <!-- 2. Session Lost (unconfirmed cancel, force abort, or network drop before write) -->
    <div
      v-else-if="isSessionLost"
      data-testid="session-lost-banner"
      class="flex items-start justify-between gap-3 rounded-lg border border-warning bg-warning-soft text-warning px-3 py-2 text-xs"
    >
      <div class="flex items-start gap-2.5 min-w-0">
        <AlertTriangle class="w-4 h-4 shrink-0 mt-0.5" />
        <div class="space-y-0.5 min-w-0">
          <div class="font-semibold">原会话已结束</div>
          <p class="m-0 opacity-95">
            {{ error?.message || '物理连接已断开，原会话的临时表、会话变量与未提交事务均已失效。' }}
          </p>
          <p v-if="error?.detail" class="m-0 font-mono-tabular text-[11px] opacity-80 break-all">
            {{ error.detail }}
          </p>
        </div>
      </div>
      <button type="button" class="btn shrink-0" @click="emit('reconnect')">
        <RefreshCw class="w-3.5 h-3.5" />
        在新会话中重连
      </button>
    </div>

    <!-- 3. Failed Transaction (e.g. PostgreSQL after error/cancel inside BEGIN) -->
    <div
      v-if="isFailedTx"
      data-testid="failed-tx-banner"
      class="flex items-center justify-between gap-3 rounded-lg border border-warning bg-warning-soft text-warning px-3 py-2 text-xs"
    >
      <div class="flex items-center gap-2 min-w-0">
        <AlertTriangle class="w-4 h-4 shrink-0" />
        <span>当前事务处于中止状态，后续语句将被数据库拒绝，需先执行 ROLLBACK 恢复会话。</span>
      </div>
      <button type="button" class="btn shrink-0" @click="emit('rollback')">
        <RotateCcw class="w-3.5 h-3.5" />
        执行 ROLLBACK
      </button>
    </div>

    <!-- 4. Statement Canceled (Session Preserved) -->
    <div
      v-if="isCanceled && !isSessionLost && !isCommitUnknown"
      data-testid="canceled-banner"
      class="flex items-start gap-2.5 rounded-lg border border-accent bg-accent-soft text-accent-text px-3 py-2 text-xs"
    >
      <Ban class="w-4 h-4 shrink-0 mt-0.5" />
      <div class="min-w-0">
        <span class="font-semibold">语句已取消：</span>
        <span>{{ error?.message || '当前语句已停止，当前标签页会话连接保留。' }}</span>
        <span v-if="error?.detail" class="ml-1.5 font-mono-tabular opacity-80">({{ error.detail }})</span>
      </div>
    </div>

    <!-- 5. Read-Only Denied -->
    <div
      v-else-if="isReadOnlyDenied && !isCommitUnknown && !isSessionLost"
      data-testid="readonly-banner"
      class="flex items-start gap-2.5 rounded-lg border border-warning bg-warning-soft text-warning px-3 py-2 text-xs"
    >
      <ShieldAlert class="w-4 h-4 shrink-0 mt-0.5" />
      <div class="min-w-0 space-y-0.5">
        <div class="font-semibold">只读保护或数据库权限拒绝写入</div>
        <p class="m-0">{{ error?.message }}</p>
        <p v-if="error?.detail" class="m-0 font-mono-tabular text-[11px] opacity-85 break-all">{{ error.detail }}</p>
      </div>
    </div>

    <!-- 6. Other SQL / Execution Errors -->
    <div
      v-else-if="error && !isCommitUnknown && !isSessionLost"
      data-testid="error-banner"
      class="flex items-start gap-2.5 rounded-lg border border-danger bg-danger-soft text-danger px-3 py-2 text-xs"
    >
      <AlertOctagon class="w-4 h-4 shrink-0 mt-0.5" />
      <div class="min-w-0 space-y-0.5">
        <div class="font-semibold">
          {{ error.message }}
          <span v-if="error.sqlState" class="ml-1.5 font-mono-tabular text-[11px] opacity-80">
            SQLSTATE {{ error.sqlState }}
          </span>
          <span v-if="error.line" class="ml-1.5 font-mono-tabular text-[11px] opacity-80">
            第 {{ error.line }} 行
          </span>
        </div>
        <p v-if="error.detail" class="m-0 font-mono-tabular text-[11px] opacity-90 break-all">{{ error.detail }}</p>
      </div>
    </div>

    <!-- 7. Snapshot Expired (Execute-Once Rule: NEVER auto-rerun, explicit button only) -->
    <div
      v-if="snapshotExpired"
      data-testid="snapshot-expired-banner"
      class="flex items-center justify-between gap-3 rounded-lg border border-warning bg-warning-soft text-warning px-3 py-2 text-xs"
    >
      <div class="flex items-center gap-2 min-w-0">
        <Info class="w-4 h-4 shrink-0" />
        <span>
          {{ expiredMessage || '该次查询快照已过期（翻页不会自动重跑原 SQL，以避免重复产生副作用）。' }}
        </span>
      </div>
      <button type="button" class="btn shrink-0" @click="emit('rerun-expired')">
        <RefreshCw class="w-3.5 h-3.5" />
        重新执行 SQL
      </button>
    </div>

    <!-- 8. Calm Notice (e.g. Cancel sent / Reconnected) -->
    <div
      v-else-if="noticeMessage && !error && !isCommitUnknown && !isSessionLost"
      class="flex items-center gap-2 rounded-lg border border-border bg-subtle text-ink-secondary px-3 py-1.5 text-xs"
    >
      <Info class="w-3.5 h-3.5 shrink-0" />
      <span>{{ noticeMessage }}</span>
    </div>
  </div>
</template>
