<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { CheckCircle2, FolderOpen, Loader2, Shield, X, XCircle } from 'lucide-vue-next'
import { api, EngineType, parseAppError, type TestConnectionResult } from '@/api/client'
import { useConnectionsStore } from '@/stores/connections'
import { engineLabel, envColorHex, envColorLabel, fileBaseName } from '@/utils/format'

const connStore = useConnectionsStore()

const form = reactive({
  id: '',
  name: '',
  driver: EngineType.EngineMySQL,
  envColor: 'default',
  readOnly: false,
  host: '127.0.0.1',
  port: 3306,
  username: 'root',
  password: '',
  updatePassword: true,
  defaultDatabase: '',
  filePath: '',
  tlsMode: 'disable',
  connectTimeoutS: 10,
})

const testing = ref(false)
const saving = ref(false)
const pickingFile = ref(false)
const testResult = ref<TestConnectionResult | null>(null)
const formError = ref('')

const isSQLite = computed(() => form.driver === EngineType.EngineSQLite)

const driverOptions: { value: EngineType; label: string }[] = [
  { value: EngineType.EngineMySQL, label: 'MySQL' },
  { value: EngineType.EnginePostgres, label: 'PostgreSQL' },
  { value: EngineType.EngineMariaDB, label: 'MariaDB' },
  { value: EngineType.EngineSQLite, label: 'SQLite' },
]

watch(
  () => [connStore.modalOpen, connStore.editingConnection] as const,
  ([open, editing]) => {
    if (!open) return
    testResult.value = null
    formError.value = ''
    if (editing) {
      form.id = editing.id
      form.name = editing.name
      form.driver = editing.driver
      form.envColor = editing.envColor || 'default'
      form.readOnly = editing.readOnly
      form.host = editing.params.host || '127.0.0.1'
      form.port = editing.params.port || (editing.driver === EngineType.EnginePostgres ? 5432 : 3306)
      form.username = editing.params.username || ''
      form.password = ''
      form.updatePassword = false // Preserve existing stored credential unless user enters a new one
      form.defaultDatabase = editing.params.defaultDatabase || ''
      form.filePath = editing.params.filePath || ''
      form.tlsMode = editing.params.tls?.mode || 'disable'
      form.connectTimeoutS = editing.params.connectTimeoutS || 10
    } else {
      form.id = ''
      form.name = ''
      form.driver = EngineType.EngineMySQL
      form.envColor = 'default'
      form.readOnly = false
      form.host = '127.0.0.1'
      form.port = 3306
      form.username = 'root'
      form.password = ''
      form.updatePassword = true
      form.defaultDatabase = ''
      form.filePath = ''
      form.tlsMode = 'disable'
      form.connectTimeoutS = 10
    }
  },
  { immediate: true },
)

function onDriverChange(drv: EngineType) {
  form.driver = drv
  testResult.value = null
  formError.value = ''
  if (!form.id) {
    if (drv === EngineType.EnginePostgres) {
      if (form.port === 3306) form.port = 5432
      if (form.username === 'root') form.username = 'postgres'
    } else if (drv === EngineType.EngineMySQL || drv === EngineType.EngineMariaDB) {
      if (form.port === 5432) form.port = 3306
      if (form.username === 'postgres') form.username = 'root'
    }
  }
}

async function pickSQLiteFile() {
  pickingFile.value = true
  formError.value = ''
  try {
    const selected = await api.pickSQLiteFile()
    if (selected && selected.trim()) {
      form.filePath = selected.trim()
      if (!form.name.trim()) {
        form.name = fileBaseName(form.filePath)
      }
    }
  } catch (err) {
    const e = parseAppError(err)
    formError.value = e.message
  } finally {
    pickingFile.value = false
  }
}

function buildRequest() {
  if (form.driver === EngineType.EngineSQLite) {
    const fp = form.filePath.trim()
    return {
      id: form.id || undefined,
      name: form.name.trim() || fileBaseName(fp) || 'SQLite',
      driver: form.driver,
      envColor: form.envColor,
      readOnly: form.readOnly,
      params: {
        host: '',
        port: 0,
        username: '',
        defaultDatabase: '',
        filePath: fp,
        tls: { mode: 'disable' },
        connectTimeoutS: Number(form.connectTimeoutS) || 10,
      },
      password: '',
      updatePassword: false,
    }
  }

  return {
    id: form.id || undefined,
    name: form.name.trim() || `${engineLabel(form.driver)} ${form.host}:${form.port}`,
    driver: form.driver,
    envColor: form.envColor,
    readOnly: form.readOnly,
    params: {
      host: form.host.trim() || '127.0.0.1',
      port: Number(form.port) || (form.driver === EngineType.EnginePostgres ? 5432 : 3306),
      username: form.username.trim(),
      defaultDatabase: form.defaultDatabase.trim(),
      filePath: '',
      tls: { mode: form.tlsMode },
      connectTimeoutS: Number(form.connectTimeoutS) || 10,
    },
    password: form.password,
    updatePassword: form.id ? form.password.length > 0 || form.updatePassword : true,
  }
}

async function runTest() {
  testing.value = true
  testResult.value = null
  formError.value = ''
  try {
    testResult.value = await api.testConnection(buildRequest())
  } catch (err) {
    const e = parseAppError(err)
    formError.value = e.message
  } finally {
    testing.value = false
  }
}

async function onSubmit() {
  saving.value = true
  formError.value = ''
  try {
    const saved = await connStore.saveConnection(buildRequest())
    connStore.closeConnectionModal()
    if (!form.id) {
      void connStore.connect(saved.id)
    }
  } catch (err) {
    const e = parseAppError(err)
    formError.value = e.detail ? `${e.message} (${e.detail})` : e.message
  } finally {
    saving.value = false
  }
}

const envColors = ['default', 'green', 'amber', 'red'] as const
</script>

<template>
  <div
    v-if="connStore.modalOpen"
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    style="background: rgba(15, 23, 42, 0.45); backdrop-filter: blur(2px)"
    @click.self="connStore.closeConnectionModal()"
  >
    <div
      class="w-full max-w-xl rounded-xl border shadow-2xl overflow-hidden flex flex-col max-h-[92vh]"
      style="background: var(--bg-elevated); border-color: var(--border-strong)"
    >
      <!-- Header -->
      <div
        class="px-5 py-3.5 border-b flex items-center justify-between"
        style="border-color: var(--border-subtle)"
      >
        <div class="font-semibold text-sm" style="color: var(--text-primary)">
          {{ form.id ? '编辑数据库连接' : '新建数据库连接' }}
        </div>
        <button type="button" class="btn btn-ghost !w-7 !h-7 !p-0" @click="connStore.closeConnectionModal()">
          <X class="w-4 h-4" />
        </button>
      </div>

      <!-- Body -->
      <form class="p-5 space-y-4 overflow-y-auto" @submit.prevent="onSubmit">
        <!-- 4-Engine Driver Selector -->
        <div class="grid grid-cols-4 gap-2">
          <button
            v-for="opt in driverOptions"
            :key="opt.value"
            type="button"
            class="h-10 px-3 rounded-lg border font-medium text-xs cursor-pointer outline-none transition-colors"
            :style="{
              borderColor: form.driver === opt.value ? 'var(--accent)' : 'var(--border-subtle)',
              background: form.driver === opt.value ? 'var(--accent-soft)' : 'var(--bg-canvas)',
              color: form.driver === opt.value ? 'var(--accent-text)' : 'var(--text-primary)',
            }"
            @click="onDriverChange(opt.value)"
          >
            {{ opt.label }}
          </button>
        </div>

        <!-- Name -->
        <label class="flex flex-col gap-1.5 text-xs">
          <span style="color: var(--text-secondary)">连接名称</span>
          <input
            v-model="form.name"
            type="text"
            class="input-field"
            :placeholder="isSQLite ? '例如：本地业务库 (app.db)' : '例如：生产订单库 (只读)'"
          />
        </label>

        <!-- Environment Color -->
        <div class="flex flex-col gap-1.5 text-xs">
          <span style="color: var(--text-secondary)">环境色标识</span>
          <div class="grid grid-cols-4 gap-2">
            <button
              v-for="c in envColors"
              :key="c"
              type="button"
              class="h-9 px-3 rounded-lg border text-xs font-medium flex items-center justify-center gap-2 cursor-pointer outline-none transition-colors"
              :style="{
                borderColor: form.envColor === c ? envColorHex(c) : 'var(--border-subtle)',
                background: form.envColor === c ? 'var(--bg-subtle)' : 'var(--bg-canvas)',
                color: 'var(--text-primary)',
              }"
              @click="form.envColor = c"
            >
              <span class="w-2.5 h-2.5 rounded-full shrink-0" :style="{ background: envColorHex(c) }" />
              <span>{{ envColorLabel(c) }}</span>
            </button>
          </div>
        </div>

        <!-- SQLite Local File Mode -->
        <template v-if="isSQLite">
          <div class="flex flex-col gap-1.5 text-xs">
            <span style="color: var(--text-secondary)">SQLite 数据库文件路径</span>
            <div class="flex items-center gap-2">
              <input
                v-model="form.filePath"
                type="text"
                class="input-field font-mono-tabular flex-1"
                placeholder="例如 /path/to/database.db（支持 .db / .sqlite / .sqlite3）"
                required
              />
              <button
                type="button"
                class="btn !h-[34px] shrink-0"
                :disabled="pickingFile"
                @click="pickSQLiteFile"
              >
                <Loader2 v-if="pickingFile" class="w-3.5 h-3.5 animate-spin" />
                <FolderOpen v-else class="w-3.5 h-3.5" />
                选择文件...
              </button>
            </div>
            <div class="text-[11px]" style="color: var(--text-muted)">
              直接读取已存在的本地 SQLite 文件；若路径不存在将直接报错，不会静默创建空库文件。
            </div>
          </div>
        </template>

        <!-- Network Engine Mode (MySQL / PostgreSQL / MariaDB) -->
        <template v-else>
          <!-- Host & Port -->
          <div class="grid grid-cols-12 gap-3">
            <label class="col-span-8 flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">主机地址 (Host)</span>
              <input v-model="form.host" type="text" class="input-field font-mono-tabular" required />
            </label>
            <label class="col-span-4 flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">端口 (Port)</span>
              <input v-model.number="form.port" type="number" class="input-field font-mono-tabular" required />
            </label>
          </div>

          <!-- Username & Password -->
          <div class="grid grid-cols-2 gap-3">
            <label class="flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">用户名</span>
              <input v-model="form.username" type="text" class="input-field font-mono-tabular" required />
            </label>
            <label class="flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">
                密码
                <span v-if="form.id" class="text-[11px]" style="color: var(--text-faint)">(留空则保持原密码)</span>
              </span>
              <input
                v-model="form.password"
                type="password"
                class="input-field font-mono-tabular"
                :placeholder="form.id ? '••••••••（未修改）' : '输入密码'"
              />
            </label>
          </div>

          <!-- Default DB & TLS -->
          <div class="grid grid-cols-2 gap-3">
            <label class="flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">默认数据库（可选）</span>
              <input
                v-model="form.defaultDatabase"
                type="text"
                class="input-field font-mono-tabular"
                :placeholder="form.driver === EngineType.EnginePostgres ? 'postgres' : '例如 app_db'"
              />
            </label>
            <label class="flex flex-col gap-1 text-xs">
              <span style="color: var(--text-secondary)">TLS 加密模式</span>
              <select v-model="form.tlsMode" class="input-field">
                <option value="disable">关闭 (disable)</option>
                <option value="prefer">优先 (prefer)</option>
                <option value="require">强制加密 (require)</option>
              </select>
            </label>
          </div>
        </template>

        <!-- Read-Only Protection Toggle -->
        <label
          class="flex items-start gap-2.5 rounded-lg border p-3 cursor-pointer"
          style="background: var(--bg-surface); border-color: var(--border-subtle)"
        >
          <input v-model="form.readOnly" type="checkbox" class="mt-0.5 accent-[var(--accent)]" />
          <div class="space-y-0.5 text-xs">
            <div class="font-medium flex items-center gap-1.5" style="color: var(--text-primary)">
              <Shield class="w-3.5 h-3.5" style="color: var(--warning)" />
              <template v-if="isSQLite">
                以只读模式打开文件（引擎级 mode=ro + query_only=ON）
              </template>
              <template v-else>
                启用客户端只读保护（防误写提示与只读会话）
              </template>
            </div>
            <div class="text-[11px] leading-relaxed" style="color: var(--text-muted)">
              <template v-if="isSQLite">
                SQLite 引擎将以只读权限打开文件并启用 query_only，从底层拦截所有写入与 DDL 操作。
              </template>
              <template v-else>
                客户端会自动拦截显式写语句并设置会话只读；生产环境的强只读约束请始终以数据库账号实际权限为准。
              </template>
            </div>
          </div>
        </label>

        <!-- Test Result Feedback -->
        <div
          v-if="testResult"
          class="rounded-lg border px-3 py-2 text-xs flex items-start gap-2"
          :style="{
            background: testResult.success ? 'var(--success-soft)' : 'var(--danger-soft)',
            borderColor: testResult.success ? 'var(--success)' : 'var(--danger)',
            color: testResult.success ? 'var(--success)' : 'var(--danger)',
          }"
        >
          <CheckCircle2 v-if="testResult.success" class="w-4 h-4 shrink-0 mt-0.5" />
          <XCircle v-else class="w-4 h-4 shrink-0 mt-0.5" />
          <div class="min-w-0">
            <div v-if="testResult.success" class="font-medium">
              连接成功 · {{ engineLabel(testResult.detectedType) }} {{ testResult.serverVersion }} ({{ testResult.latencyMs }} ms)
            </div>
            <template v-else>
              <div class="font-medium">{{ testResult.error?.message || '连接测试失败' }}</div>
              <div v-if="testResult.error?.detail" class="font-mono-tabular text-[11px] opacity-85 break-all mt-0.5">
                {{ testResult.error.detail }}
              </div>
            </template>
          </div>
        </div>

        <div
          v-if="formError"
          class="rounded-lg border px-3 py-2 text-xs"
          style="background: var(--danger-soft); border-color: var(--danger); color: var(--danger)"
        >
          {{ formError }}
        </div>

        <!-- Footer Buttons -->
        <div class="pt-2 flex items-center justify-between gap-2">
          <button type="button" class="btn" :disabled="testing" @click="runTest">
            <Loader2 v-if="testing" class="w-3.5 h-3.5 animate-spin" />
            测试连接
          </button>

          <div class="flex items-center gap-2">
            <button type="button" class="btn" @click="connStore.closeConnectionModal()">取消</button>
            <button type="submit" class="btn btn-primary" :disabled="saving">
              <Loader2 v-if="saving" class="w-3.5 h-3.5 animate-spin" />
              {{ form.id ? '保存修改' : '保存并连接' }}
            </button>
          </div>
        </div>
      </form>
    </div>
  </div>
</template>
