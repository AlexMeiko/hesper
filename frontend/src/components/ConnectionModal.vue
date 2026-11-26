<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { CheckCircle2, ChevronDown, ChevronRight, FolderOpen, KeyRound, Loader2, Lock, Shield, XCircle } from 'lucide-vue-next'
import { api, parseAppError } from '@/api/client'
import { EngineType, ErrorCode, type SaveConnectionRequest, type TestConnectionResult } from '@/api/types'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { engineLabel, envColorHex, envColorLabel, fileBaseName } from '@/utils/format'
import { buildHostKeyConfirmOptions } from '@/utils/sshChallenge'
import { defaultPortForEngine } from '@/utils/tableMeta'
import BaseModal from './BaseModal.vue'

const connStore = useConnectionsStore()
const uiStore = useUiStore()

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
  // TLS
  tlsMode: 'disable',
  tlsCaCertPath: '',
  tlsCertPath: '',
  tlsKeyPath: '',
  // SSH Tunnel
  sshEnabled: false,
  sshHost: '',
  sshPort: 22,
  sshUsername: '',
  sshAuthMethod: 'password' as 'password' | 'key' | 'agent',
  sshPrivateKeyPath: '',
  sshSaveCredential: true,
  sshPassword: '',
  updateSshPassword: true,
  sshPassphrase: '',
  updateSshPassphrase: true,
  hasSavedSshPassword: false,
  hasSavedSshPassphrase: false,
  connectTimeoutS: 10,
  errorCharset: '',
})

const sshExpanded = ref(false)
const testing = ref(false)
const saving = ref(false)
const pickingFile = ref('')
const testResult = ref<TestConnectionResult | null>(null)
const formError = ref('')

const isSQLite = computed(() => form.driver === EngineType.EngineSQLite)
const showTLSCa = computed(() => form.tlsMode === 'verify-ca' || form.tlsMode === 'verify-full')
const showTLSMtls = computed(() => form.tlsMode !== 'disable')

const driverOptions: { value: EngineType; label: string }[] = [
  { value: EngineType.EngineMySQL, label: 'MySQL' },
  { value: EngineType.EnginePostgres, label: 'PostgreSQL' },
  { value: EngineType.EngineMariaDB, label: 'MariaDB' },
  { value: EngineType.EngineSQLite, label: 'SQLite' },
]

const sshAuthOptions: { value: 'password' | 'key' | 'agent'; label: string }[] = [
  { value: 'password', label: '密码' },
  { value: 'key', label: '私钥文件' },
  { value: 'agent', label: 'SSH Agent' },
]

watch(
  () => [uiStore.connectionModalOpen, uiStore.editingConnection] as const,
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
      form.port = editing.params.port || defaultPortForEngine(editing.driver) || 3306
      form.username = editing.params.username || ''
      form.password = ''
      form.updatePassword = false
      form.defaultDatabase = editing.params.defaultDatabase || ''
      form.filePath = editing.params.filePath || ''
      form.tlsMode = editing.params.tls?.mode || 'disable'
      form.tlsCaCertPath = editing.params.tls?.caCertPath || ''
      form.tlsCertPath = editing.params.tls?.certPath || ''
      form.tlsKeyPath = editing.params.tls?.keyPath || ''

      const ssh = editing.params.ssh
      form.sshEnabled = Boolean(ssh?.enabled)
      form.sshHost = ssh?.host || ''
      form.sshPort = ssh?.port || 22
      form.sshUsername = ssh?.username || ''
      form.sshAuthMethod = (ssh?.authMethod as 'password' | 'key' | 'agent') || 'password'
      form.sshPrivateKeyPath = ssh?.privateKeyPath || ''
      form.sshSaveCredential = ssh?.enabled ? Boolean(ssh.saveCredential) : true
      form.sshPassword = ''
      form.updateSshPassword = false
      form.sshPassphrase = ''
      form.updateSshPassphrase = false
      form.hasSavedSshPassword = Boolean(editing.hasSshPassword)
      form.hasSavedSshPassphrase = Boolean(editing.hasSshPassphrase)
      sshExpanded.value = form.sshEnabled

      form.connectTimeoutS = editing.params.connectTimeoutS || 10
      form.errorCharset = editing.params.errorCharset || ''
    } else {
      form.id = ''
      form.name = ''
      form.driver = EngineType.EngineMySQL
      form.envColor = 'default'
      form.readOnly = false
      form.host = '127.0.0.1'
      form.port = defaultPortForEngine(EngineType.EngineMySQL)
      form.username = 'root'
      form.password = ''
      form.updatePassword = true
      form.defaultDatabase = ''
      form.filePath = ''
      form.tlsMode = 'disable'
      form.tlsCaCertPath = ''
      form.tlsCertPath = ''
      form.tlsKeyPath = ''

      form.sshEnabled = false
      form.sshHost = ''
      form.sshPort = 22
      form.sshUsername = ''
      form.sshAuthMethod = 'password'
      form.sshPrivateKeyPath = ''
      form.sshSaveCredential = true
      form.sshPassword = ''
      form.updateSshPassword = true
      form.sshPassphrase = ''
      form.updateSshPassphrase = true
      form.hasSavedSshPassword = false
      form.hasSavedSshPassphrase = false
      sshExpanded.value = false

      form.connectTimeoutS = 10
      form.errorCharset = ''
    }
  },
  { immediate: true },
)

function onDriverChange(drv: EngineType) {
  const prevDefaultPort = defaultPortForEngine(form.driver)
  form.driver = drv
  testResult.value = null
  formError.value = ''
  if (drv === EngineType.EngineSQLite) {
    form.sshEnabled = false
    sshExpanded.value = false
  }
  if (!form.id) {
    const nextDefaultPort = defaultPortForEngine(drv)
    if (nextDefaultPort > 0 && (form.port === prevDefaultPort || form.port === 0)) {
      form.port = nextDefaultPort
    }
    if (drv === EngineType.EnginePostgres && form.username === 'root') {
      form.username = 'postgres'
    } else if ((drv === EngineType.EngineMySQL || drv === EngineType.EngineMariaDB) && form.username === 'postgres') {
      form.username = 'root'
    }
  }
}

async function pickSQLiteFile() {
  pickingFile.value = 'sqlite'
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
    formError.value = parseAppError(err).message
  } finally {
    pickingFile.value = ''
  }
}

async function pickLocalFile(kind: 'tls_ca' | 'tls_cert' | 'tls_key' | 'ssh_key') {
  pickingFile.value = kind
  formError.value = ''
  try {
    const selected = await api.pickLocalFile(kind)
    if (selected && selected.trim()) {
      const p = selected.trim()
      if (kind === 'tls_ca') form.tlsCaCertPath = p
      else if (kind === 'tls_cert') form.tlsCertPath = p
      else if (kind === 'tls_key') form.tlsKeyPath = p
      else if (kind === 'ssh_key') form.sshPrivateKeyPath = p
    }
  } catch (err) {
    formError.value = parseAppError(err).message
  } finally {
    pickingFile.value = ''
  }
}

function buildRequest(runtimeOverride?: { sshSecret?: string }): SaveConnectionRequest {
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

  // Strip hidden inapplicable TLS cert paths so backend ValidateTLSConfig never rejects stale hidden values.
  const tlsCa = showTLSCa.value ? form.tlsCaCertPath.trim() || undefined : undefined
  const tlsCert = showTLSMtls.value ? form.tlsCertPath.trim() || undefined : undefined
  const tlsKey = showTLSMtls.value ? form.tlsKeyPath.trim() || undefined : undefined

  const sshParam = form.sshEnabled
    ? {
        enabled: true,
        host: form.sshHost.trim(),
        port: Number(form.sshPort) || 22,
        username: form.sshUsername.trim(),
        authMethod: form.sshAuthMethod,
        privateKeyPath: form.sshAuthMethod === 'key' ? form.sshPrivateKeyPath.trim() : undefined,
        saveCredential: form.sshAuthMethod === 'agent' ? false : form.sshSaveCredential,
      }
    : undefined

  const effectiveSshPw =
    runtimeOverride?.sshSecret !== undefined && form.sshAuthMethod === 'password'
      ? runtimeOverride.sshSecret
      : form.sshPassword
  const effectiveSshPassphrase =
    runtimeOverride?.sshSecret !== undefined && form.sshAuthMethod === 'key'
      ? runtimeOverride.sshSecret
      : form.sshPassphrase

  return {
    id: form.id || undefined,
    name: form.name.trim() || `${engineLabel(form.driver)} ${form.host}:${form.port}`,
    driver: form.driver,
    envColor: form.envColor,
    readOnly: form.readOnly,
    params: {
      host: form.host.trim() || '127.0.0.1',
      port: Number(form.port) || defaultPortForEngine(form.driver),
      username: form.username.trim(),
      defaultDatabase: form.defaultDatabase.trim(),
      filePath: '',
      tls: {
        mode: form.tlsMode,
        ...(tlsCa ? { caCertPath: tlsCa } : {}),
        ...(tlsCert ? { certPath: tlsCert } : {}),
        ...(tlsKey ? { keyPath: tlsKey } : {}),
      },
      ...(sshParam ? { ssh: sshParam } : {}),
      connectTimeoutS: Number(form.connectTimeoutS) || 10,
      errorCharset: form.driver === EngineType.EnginePostgres ? form.errorCharset.trim() || undefined : undefined,
    },
    password: form.password,
    updatePassword: form.id ? form.password.length > 0 || form.updatePassword : true,
    sshPassword: form.sshEnabled && form.sshAuthMethod === 'password' ? effectiveSshPw : undefined,
    updateSshPassword:
      form.sshEnabled && form.sshAuthMethod === 'password'
        ? form.id
          ? effectiveSshPw.length > 0 || form.updateSshPassword
          : true
        : false,
    sshPassphrase: form.sshEnabled && form.sshAuthMethod === 'key' ? effectiveSshPassphrase : undefined,
    updateSshPassphrase:
      form.sshEnabled && form.sshAuthMethod === 'key'
        ? form.id
          ? effectiveSshPassphrase.length > 0 || form.updateSshPassphrase
          : true
        : false,
  }
}

async function runTest() {
  testing.value = true
  testResult.value = null
  formError.value = ''
  let runtimeSecret: string | undefined
  try {
    for (let round = 0; round <= 4; round++) {
      const res = await api.testConnection(
        buildRequest(runtimeSecret !== undefined ? { sshSecret: runtimeSecret } : undefined),
      )
      testResult.value = res
      if (res.success || !res.error) {
        return
      }
      if (round < 4 && res.error.code === ErrorCode.ErrTunnelFailed && res.error.hostKeyChallenge) {
        const ch = res.error.hostKeyChallenge
        const confirmed = await uiStore.requestConfirm(buildHostKeyConfirmOptions(ch))
        if (!confirmed) return
        await api.trustSSHHostKey(ch.hostPort, ch.algorithm, ch.fingerprint)
        continue
      }
      if (round < 4 && res.error.code === ErrorCode.ErrAuthFailed && res.error.sshAuthPrompt) {
        const sub = await uiStore.requestSSHSecret(res.error.sshAuthPrompt, { saveMode: 'form' })
        if (!sub) return
        runtimeSecret = sub.secret
        if (sub.saveSecret) {
          form.sshSaveCredential = true
          if (form.sshAuthMethod === 'key') {
            form.sshPassphrase = sub.secret
            form.updateSshPassphrase = true
          } else {
            form.sshPassword = sub.secret
            form.updateSshPassword = true
          }
        }
        continue
      }
      return
    }
  } catch (err) {
    formError.value = parseAppError(err).message
  } finally {
    testing.value = false
  }
}

async function onSubmit() {
  saving.value = true
  formError.value = ''
  try {
    const saved = await connStore.saveConnection(buildRequest())
    uiStore.closeConnectionModal()
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
  <BaseModal
    :open="uiStore.connectionModalOpen"
    :title="form.id ? '编辑数据库连接' : '新建数据库连接'"
    max-width-class="max-w-xl"
    max-height-class="max-h-[92vh]"
    test-id="connection-modal"
    @close="uiStore.closeConnectionModal()"
  >
    <form class="p-5 space-y-4 overflow-y-auto" @submit.prevent="onSubmit">
      <!-- 4-Engine Driver Selector -->
      <div class="grid grid-cols-4 gap-2">
        <button
          v-for="opt in driverOptions"
          :key="opt.value"
          type="button"
          class="h-10 px-3 rounded-lg border font-medium text-xs cursor-pointer outline-none transition-colors"
          :class="
            form.driver === opt.value
              ? 'border-accent bg-accent-soft text-accent-text'
              : 'border-border bg-canvas text-ink hover:bg-subtle'
          "
          @click="onDriverChange(opt.value)"
        >
          {{ opt.label }}
        </button>
      </div>

      <!-- Name -->
      <label class="flex flex-col gap-1.5 text-xs">
        <span class="text-ink-secondary">连接名称</span>
        <input
          v-model="form.name"
          type="text"
          class="input-field"
          :placeholder="isSQLite ? '例如：本地业务库 (app.db)' : '例如：生产订单库 (只读)'"
        />
      </label>

      <!-- Environment Color -->
      <div class="flex flex-col gap-1.5 text-xs">
        <span class="text-ink-secondary">环境色标识</span>
        <div class="grid grid-cols-4 gap-2">
          <button
            v-for="c in envColors"
            :key="c"
            type="button"
            class="h-9 px-3 rounded-lg border text-xs font-medium text-ink flex items-center justify-center gap-2 cursor-pointer outline-none transition-colors"
            :class="form.envColor === c ? 'bg-subtle' : 'bg-canvas border-border hover:bg-subtle'"
            :style="form.envColor === c ? { borderColor: envColorHex(c) } : undefined"
            @click="form.envColor = c"
          >
            <span class="w-2.5 h-2.5 rounded-full shrink-0" :style="{ background: envColorHex(c) }" />
            <span>{{ envColorLabel(c) }}</span>
          </button>
        </div>
        <div v-if="form.envColor === 'red'" class="text-[11px] text-danger">
          生产环境连接在执行 DROP、TRUNCATE 或无 WHERE 的 UPDATE / DELETE 前将弹出二次确认。
        </div>
      </div>

      <!-- SQLite Local File Mode -->
      <template v-if="isSQLite">
        <div class="flex flex-col gap-1.5 text-xs">
          <span class="text-ink-secondary">SQLite 数据库文件路径</span>
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
              class="btn h-[36px] shrink-0"
              :disabled="pickingFile === 'sqlite'"
              @click="pickSQLiteFile"
            >
              <Loader2 v-if="pickingFile === 'sqlite'" class="w-3.5 h-3.5 animate-spin" />
              <FolderOpen v-else class="w-3.5 h-3.5" />
              选择文件...
            </button>
          </div>
          <div class="text-[11px] text-ink-muted">
            直接读取已存在的本地 SQLite 文件；若路径不存在将直接报错，不会静默创建空库文件。
          </div>
        </div>
      </template>

      <!-- Network Engine Mode (MySQL / PostgreSQL / MariaDB) -->
      <template v-else>
        <div class="grid grid-cols-12 gap-3">
          <label class="col-span-8 flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">主机地址 (Host)</span>
            <input v-model="form.host" type="text" class="input-field font-mono-tabular" required />
          </label>
          <label class="col-span-4 flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">端口 (Port)</span>
            <input v-model.number="form.port" type="number" class="input-field font-mono-tabular" required />
          </label>
        </div>

        <div class="grid grid-cols-2 gap-3">
          <label class="flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">用户名</span>
            <input v-model="form.username" type="text" class="input-field font-mono-tabular" required />
          </label>
          <label class="flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">
              密码
              <span v-if="form.id" class="text-[11px] text-ink-faint">(留空则保持原密码)</span>
            </span>
            <input
              v-model="form.password"
              type="password"
              class="input-field font-mono-tabular"
              :placeholder="form.id ? '••••••••（未修改）' : '输入密码'"
            />
          </label>
        </div>

        <div class="grid grid-cols-2 gap-3">
          <label class="flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">默认数据库（可选）</span>
            <input
              v-model="form.defaultDatabase"
              type="text"
              class="input-field font-mono-tabular"
              :placeholder="form.driver === EngineType.EnginePostgres ? 'postgres' : '例如 app_db'"
            />
          </label>
          <label class="flex flex-col gap-1 text-xs">
            <span class="text-ink-secondary">TLS 加密模式</span>
            <select v-model="form.tlsMode" data-testid="tls-mode-select" class="input-field">
              <option value="disable">关闭 (disable)</option>
              <option value="prefer">优先 (prefer)</option>
              <option value="require">强制加密 (require)</option>
              <option value="verify-ca">校验 CA 证书链 (verify-ca)</option>
              <option value="verify-full">严格校验 CA 与主机名 (verify-full)</option>
            </select>
          </label>
        </div>

        <!-- TLS Certificate Paths (CA / mTLS) -->
        <div
          v-if="showTLSCa || showTLSMtls"
          data-testid="tls-cert-section"
          class="rounded-lg border border-border bg-surface p-3 space-y-2.5 text-xs"
        >
          <div class="font-medium text-ink flex items-center gap-1.5">
            <Lock class="w-3.5 h-3.5 text-accent" />
            <span>TLS 证书配置（{{ showTLSCa ? 'CA 校验 / 可选双向 mTLS' : '可选双向 mTLS 客户端证书' }}）</span>
          </div>

          <div v-if="showTLSCa" class="flex flex-col gap-1">
            <span class="text-ink-secondary">CA 根证书路径（留空则使用系统根证书）</span>
            <div class="flex items-center gap-2">
              <input
                v-model="form.tlsCaCertPath"
                type="text"
                data-testid="tls-ca-input"
                class="input-field font-mono-tabular flex-1"
                placeholder="/path/to/ca.pem"
              />
              <button
                type="button"
                class="btn h-[36px] shrink-0"
                :disabled="pickingFile === 'tls_ca'"
                @click="pickLocalFile('tls_ca')"
              >
                <FolderOpen class="w-3.5 h-3.5" />
                浏览
              </button>
            </div>
          </div>

          <div v-if="showTLSMtls" class="grid grid-cols-1 gap-2">
            <div class="flex flex-col gap-1">
              <span class="text-ink-secondary">客户端证书路径（mTLS，可选）</span>
              <div class="flex items-center gap-2">
                <input
                  v-model="form.tlsCertPath"
                  type="text"
                  data-testid="tls-cert-input"
                  class="input-field font-mono-tabular flex-1"
                  placeholder="/path/to/client-cert.pem"
                />
                <button
                  type="button"
                  class="btn h-[36px] shrink-0"
                  :disabled="pickingFile === 'tls_cert'"
                  @click="pickLocalFile('tls_cert')"
                >
                  <FolderOpen class="w-3.5 h-3.5" />
                  浏览
                </button>
              </div>
            </div>
            <div class="flex flex-col gap-1">
              <span class="text-ink-secondary">客户端私钥路径（mTLS，与客户端证书成对填写）</span>
              <div class="flex items-center gap-2">
                <input
                  v-model="form.tlsKeyPath"
                  type="text"
                  data-testid="tls-key-input"
                  class="input-field font-mono-tabular flex-1"
                  placeholder="/path/to/client-key.pem"
                />
                <button
                  type="button"
                  class="btn h-[36px] shrink-0"
                  :disabled="pickingFile === 'tls_key'"
                  @click="pickLocalFile('tls_key')"
                >
                  <FolderOpen class="w-3.5 h-3.5" />
                  浏览
                </button>
              </div>
            </div>
          </div>
        </div>

        <!-- SSH Bastion Tunnel Section -->
        <div data-testid="ssh-tunnel-section" class="rounded-lg border border-border bg-surface overflow-hidden text-xs">
          <div class="px-3 py-2.5 flex items-center justify-between gap-2">
            <button
              type="button"
              class="flex items-center gap-1.5 font-medium text-ink bg-transparent border-0 p-0 cursor-pointer"
              @click="sshExpanded = !sshExpanded"
            >
              <ChevronDown v-if="sshExpanded" class="w-3.5 h-3.5 text-ink-muted" />
              <ChevronRight v-else class="w-3.5 h-3.5 text-ink-muted" />
              <KeyRound class="w-3.5 h-3.5 text-accent" />
              <span>SSH 跳板机隧道（零本地端口占用）</span>
            </button>
            <label class="flex items-center gap-1.5 cursor-pointer select-none">
              <input
                v-model="form.sshEnabled"
                type="checkbox"
                data-testid="ssh-enabled-checkbox"
                class="accent-accent"
                @change="form.sshEnabled && (sshExpanded = true)"
              />
              <span class="text-ink-secondary">启用 SSH 隧道</span>
            </label>
          </div>

          <div v-if="sshExpanded && form.sshEnabled" class="px-3 pb-3 pt-1 border-t border-border space-y-3">
            <div class="grid grid-cols-12 gap-2.5">
              <label class="col-span-8 flex flex-col gap-1">
                <span class="text-ink-secondary">SSH 跳板机主机</span>
                <input
                  v-model="form.sshHost"
                  type="text"
                  data-testid="ssh-host-input"
                  class="input-field font-mono-tabular"
                  placeholder="bastion.example.com"
                  required
                />
              </label>
              <label class="col-span-4 flex flex-col gap-1">
                <span class="text-ink-secondary">SSH 端口</span>
                <input
                  v-model.number="form.sshPort"
                  type="number"
                  data-testid="ssh-port-input"
                  class="input-field font-mono-tabular"
                  min="1"
                  max="65535"
                  required
                />
              </label>
            </div>

            <div class="grid grid-cols-12 gap-2.5">
              <label class="col-span-5 flex flex-col gap-1">
                <span class="text-ink-secondary">SSH 用户名</span>
                <input
                  v-model="form.sshUsername"
                  type="text"
                  data-testid="ssh-username-input"
                  class="input-field font-mono-tabular"
                  placeholder="ubuntu"
                  required
                />
              </label>
              <div class="col-span-7 flex flex-col gap-1">
                <span class="text-ink-secondary">认证方式</span>
                <div class="grid grid-cols-3 gap-1.5">
                  <button
                    v-for="opt in sshAuthOptions"
                    :key="opt.value"
                    type="button"
                    :data-testid="`ssh-auth-${opt.value}`"
                    class="h-[36px] px-2 rounded-lg border text-xs font-medium cursor-pointer transition-colors"
                    :class="
                      form.sshAuthMethod === opt.value
                        ? 'border-accent bg-accent-soft text-accent-text'
                        : 'border-border bg-canvas text-ink hover:bg-subtle'
                    "
                    @click="form.sshAuthMethod = opt.value"
                  >
                    {{ opt.label }}
                  </button>
                </div>
              </div>
            </div>

            <!-- Password Auth -->
            <template v-if="form.sshAuthMethod === 'password'">
              <label class="flex items-center gap-2 cursor-pointer select-none">
                <input
                  v-model="form.sshSaveCredential"
                  type="checkbox"
                  data-testid="ssh-save-credential-checkbox"
                  class="accent-accent"
                />
                <span class="text-ink font-medium">记住 SSH 密码（保存到系统钥匙串 / 加密库）</span>
              </label>

              <label class="flex flex-col gap-1">
                <span class="text-ink-secondary">
                  SSH 密码
                  <span v-if="!form.sshSaveCredential" class="text-[11px] text-ink-faint">
                    （未勾选记住，保存时不落盘；仅用于当前测试，每次新建连接弹窗输入）
                  </span>
                  <span v-else-if="form.id && form.hasSavedSshPassword" class="text-[11px] text-ink-faint">
                    （留空则保持已保存密码）
                  </span>
                </span>
                <input
                  v-model="form.sshPassword"
                  type="password"
                  data-testid="ssh-password-input"
                  class="input-field font-mono-tabular"
                  :placeholder="
                    form.sshSaveCredential && form.id && form.hasSavedSshPassword
                      ? '••••••••（未修改）'
                      : form.sshSaveCredential
                        ? '输入 SSH 登录密码'
                        : '测试连接可临时填写，保存时不存储'
                  "
                />
              </label>
            </template>

            <!-- Private Key Auth -->
            <template v-else-if="form.sshAuthMethod === 'key'">
              <div class="flex flex-col gap-1">
                <span class="text-ink-secondary">SSH 私钥文件路径</span>
                <div class="flex items-center gap-2">
                  <input
                    v-model="form.sshPrivateKeyPath"
                    type="text"
                    data-testid="ssh-key-path-input"
                    class="input-field font-mono-tabular flex-1"
                    placeholder="~/.ssh/id_ed25519"
                    required
                  />
                  <button
                    type="button"
                    class="btn h-[36px] shrink-0"
                    :disabled="pickingFile === 'ssh_key'"
                    @click="pickLocalFile('ssh_key')"
                  >
                    <FolderOpen class="w-3.5 h-3.5" />
                    浏览
                  </button>
                </div>
              </div>

              <label class="flex items-center gap-2 cursor-pointer select-none">
                <input
                  v-model="form.sshSaveCredential"
                  type="checkbox"
                  data-testid="ssh-save-credential-checkbox"
                  class="accent-accent"
                />
                <span class="text-ink font-medium">记住私钥口令（未加密私钥可留空）</span>
              </label>

              <label class="flex flex-col gap-1">
                <span class="text-ink-secondary">
                  私钥口令 (Passphrase，可选)
                  <span v-if="!form.sshSaveCredential" class="text-[11px] text-ink-faint">
                    （未勾选记住，保存时不落盘，需要时弹窗输入）
                  </span>
                  <span v-else-if="form.id && form.hasSavedSshPassphrase" class="text-[11px] text-ink-faint">
                    （留空则保持已保存口令）
                  </span>
                </span>
                <input
                  v-model="form.sshPassphrase"
                  type="password"
                  data-testid="ssh-passphrase-input"
                  class="input-field font-mono-tabular"
                  :placeholder="
                    form.sshSaveCredential && form.id && form.hasSavedSshPassphrase
                      ? '••••••••（未修改）'
                      : '若私钥无口令可留空'
                  "
                />
              </label>
            </template>

            <!-- SSH Agent Auth -->
            <div v-else class="rounded-md border border-border bg-canvas px-3 py-2 text-[11px] text-ink-muted leading-relaxed">
              将自动连接本机 SSH Agent（Linux / macOS 使用 <code class="font-mono-tabular">SSH_AUTH_SOCK</code>，Windows 使用 OpenSSH Authentication Agent 命名管道）。
            </div>
          </div>
        </div>

        <label v-if="form.driver === EngineType.EnginePostgres" class="flex flex-col gap-1.5 text-xs">
          <span class="text-ink-secondary">连接报错编码（可选）</span>
          <input v-model="form.errorCharset" type="text" class="input-field font-mono-tabular" placeholder="例如 GBK 或 Windows-1252" />
        </label>
      </template>

      <!-- Read-Only Protection Toggle -->
      <label class="flex items-start gap-2.5 rounded-lg border border-border bg-surface p-3 cursor-pointer">
        <input v-model="form.readOnly" type="checkbox" class="mt-0.5 accent-accent" />
        <div class="space-y-0.5 text-xs">
          <div class="font-medium text-ink flex items-center gap-1.5">
            <Shield class="w-3.5 h-3.5 text-warning" />
            <template v-if="isSQLite">
              以只读模式打开文件（引擎级 mode=ro + query_only=ON）
            </template>
            <template v-else>
              启用客户端只读保护（防误写提示与只读会话）
            </template>
          </div>
          <div class="text-[11px] leading-relaxed text-ink-muted">
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
        :class="
          testResult.success
            ? 'bg-success-soft border-success text-success'
            : 'bg-danger-soft border-danger text-danger'
        "
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

      <div v-if="formError" class="rounded-lg border border-danger bg-danger-soft text-danger px-3 py-2 text-xs">
        {{ formError }}
      </div>

      <!-- Footer Buttons -->
      <div class="pt-2 flex items-center justify-between gap-2">
        <button type="button" data-testid="test-connection-btn" class="btn" :disabled="testing" @click="runTest">
          <Loader2 v-if="testing" class="w-3.5 h-3.5 animate-spin" />
          测试连接
        </button>

        <div class="flex items-center gap-2">
          <button type="button" class="btn" @click="uiStore.closeConnectionModal()">取消</button>
          <button type="submit" data-testid="save-connection-btn" class="btn btn-primary" :disabled="saving">
            <Loader2 v-if="saving" class="w-3.5 h-3.5 animate-spin" />
            {{ form.id ? '保存修改' : '保存并连接' }}
          </button>
        </div>
      </div>
    </form>
  </BaseModal>
</template>
