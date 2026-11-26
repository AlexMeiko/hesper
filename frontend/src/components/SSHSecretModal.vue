<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { KeyRound } from 'lucide-vue-next'
import { useUiStore } from '@/stores/ui'
import BaseModal from './BaseModal.vue'

const uiStore = useUiStore()
const prompt = computed(() => uiStore.sshSecretPrompt)

const secretInput = ref('')
const saveSecret = ref(false)

watch(prompt, (next) => {
  if (!next) return
  secretInput.value = ''
  saveSecret.value = false // Always default unchecked so users never overwrite stored credentials unintentionally
})

const isAgent = computed(() => prompt.value?.authMethod === 'agent')
const isPassphrase = computed(() => !isAgent.value && (prompt.value?.authMethod === 'key' || Boolean(prompt.value?.needPassphrase)))
const isFormSaveMode = computed(() => uiStore.sshSecretSaveMode === 'form')

function submit() {
  if (isAgent.value) {
    uiStore.resolveSSHSecret({
      secret: '',
      saveSecret: false,
    })
    return
  }
  if (!secretInput.value) return
  uiStore.resolveSSHSecret({
    secret: secretInput.value,
    saveSecret: saveSecret.value,
  })
}

function cancel() {
  uiStore.resolveSSHSecret(null)
}
</script>

<template>
  <BaseModal
    :open="Boolean(prompt)"
    max-width-class="max-w-md"
    max-height-class="max-h-[82vh]"
    z-index-class="z-60"
    test-id="ssh-secret-modal"
    @close="cancel"
  >
    <template #header>
      <div class="flex items-center gap-2">
        <KeyRound class="w-4 h-4 shrink-0 text-accent" />
        <span class="font-semibold text-sm text-ink truncate">
          {{ isAgent ? 'SSH Agent 认证未通过' : isPassphrase ? '输入 SSH 私钥口令' : '输入 SSH 登录密码' }}
        </span>
      </div>
    </template>

    <form v-if="prompt" class="p-5 space-y-4 text-xs" @submit.prevent="submit">
      <div class="space-y-1">
        <p class="m-0 text-ink leading-relaxed">
          {{
            isAgent
              ? prompt.reason || 'SSH Agent 认证失败，请检查已加载的密钥后重试'
              : prompt.reason || (isPassphrase ? '连接该 SSH 跳板机需要私钥口令' : '连接该 SSH 跳板机需要登录密码')
          }}
        </p>
        <div class="font-mono-tabular text-[11px] text-ink-muted bg-surface border border-border rounded-md px-2.5 py-1.5">
          {{ prompt.username }}@{{ prompt.hostPort }}
        </div>
        <p v-if="isAgent" class="m-0 pt-1 text-[11px] leading-relaxed text-ink-muted">
          请确认本机 ssh-agent 正在运行且已通过 <code class="font-mono-tabular">ssh-add</code> 加载对应私钥，然后点击下方重试。
        </p>
      </div>

      <template v-if="!isAgent">
        <label class="flex flex-col gap-1.5">
          <span class="text-ink-secondary">{{ isPassphrase ? '私钥口令 (Passphrase)' : 'SSH 密码' }}</span>
          <input
            v-model="secretInput"
            type="password"
            data-testid="ssh-secret-input"
            data-modal-initial-focus
            class="input-field font-mono-tabular"
            :placeholder="isPassphrase ? '输入私钥解密口令' : '输入 SSH 密码'"
            required
          />
        </label>

        <label class="flex items-start gap-2 rounded-lg border border-border bg-surface px-3 py-2.5 cursor-pointer">
          <input
            v-model="saveSecret"
            type="checkbox"
            data-testid="ssh-secret-save-checkbox"
            class="mt-0.5 accent-accent"
          />
          <div class="space-y-0.5">
            <div class="font-medium text-ink">
              <template v-if="isFormSaveMode">
                {{ isPassphrase ? '随连接配置保存私钥口令' : '随连接配置保存 SSH 密码' }}
              </template>
              <template v-else>
                {{ isPassphrase ? '保存并覆盖已存储的私钥口令' : '保存并覆盖已存储的 SSH 密码' }}
              </template>
            </div>
            <div class="text-[11px] leading-relaxed text-ink-muted">
              <template v-if="isFormSaveMode">
                不勾选则仅本次测试有效；勾选后将回填到当前表单并开启“记住凭据”，点击保存连接后写入安全存储。
              </template>
              <template v-else>
                不勾选则仅本次连接有效，原存储凭据与设置保持不变；勾选后会立即覆盖写入安全存储。
              </template>
            </div>
          </div>
        </label>
      </template>

      <div class="flex items-center justify-end gap-2 pt-1">
        <button type="button" data-testid="ssh-secret-cancel-btn" class="btn" @click="cancel">取消</button>
        <button
          type="submit"
          data-testid="ssh-secret-submit-btn"
          :data-modal-initial-focus="isAgent ? '' : undefined"
          class="btn btn-primary"
          :disabled="!isAgent && !secretInput"
        >
          {{ isAgent ? '检查密钥后重试' : '确认并继续' }}
        </button>
      </div>
    </form>
  </BaseModal>
</template>
