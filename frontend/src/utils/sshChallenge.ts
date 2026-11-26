import { parseAppError } from '@/api/client'
import { ErrorCode, type AppError, type SSHHostKeyChallenge } from '@/api/types'
import { useUiStore, type ConfirmOptions, type SSHSecretSubmission } from '@/stores/ui'

export interface SaveFeedbackPayload {
  credentialSaved?: boolean
  saveWarning?: string
}

export type SSHChallengeOutcome<T> =
  | { ok: true; value: T }
  | { ok: false; aborted?: boolean; canceled?: boolean; error?: AppError }

export function buildHostKeyConfirmOptions(ch: SSHHostKeyChallenge): ConfirmOptions {
  if (ch.mismatch) {
    return {
      title: 'SSH 主机指纹已变更（高危警示）',
      message: `跳板机 ${ch.hostPort} 返回的主机密钥与历史记录不一致！这可能意味着存在中间人攻击，或跳板机重装了 SSH 服务。请先向运维确认后再决定是否信任。`,
      tone: 'danger',
      bullets: [
        `主机地址：${ch.hostPort}`,
        `密钥算法：${ch.algorithm}`,
        `历史指纹：${ch.previousFingerprint || '未知'}`,
        `当前指纹：${ch.fingerprint}`,
      ],
      codePreview: `Host: ${ch.hostPort}\nAlgorithm: ${ch.algorithm}\nPrevious: ${ch.previousFingerprint || '(none)'}\nCurrent:  ${ch.fingerprint}`,
      confirmLabel: '确认信任新指纹并重试',
      cancelLabel: '取消连接',
    }
  }
  return {
    title: '首次连接 SSH 跳板机 · 核对主机指纹',
    message: `首次连接 SSH 跳板机 ${ch.hostPort}，无法自动验证主机身份。请核对以下 SHA-256 指纹后确认信任：`,
    tone: 'warning',
    bullets: [`主机地址：${ch.hostPort}`, `密钥算法：${ch.algorithm}`],
    codePreview: `${ch.hostPort} (${ch.algorithm})\n${ch.fingerprint}`,
    confirmLabel: '信任此主机并继续',
    cancelLabel: '取消',
  }
}

export function handleSaveFeedback(
  payload: SaveFeedbackPayload | null | undefined,
  onCredentialSaved?: () => void,
) {
  if (!payload) return
  if (payload.credentialSaved) {
    onCredentialSaved?.()
  }
  if (payload.saveWarning) {
    useUiStore().notify('warning', '保存 SSH 凭据失败', payload.saveWarning, 5500)
  }
}

/**
 * Shared interactive SSH TOFU host-key confirmation + runtime password/passphrase retry loop.
 * Interactive modals always await OUTSIDE backend lifecycle serialization chains and re-verify
 * isStillCurrent() before and after every prompt so concurrent disconnect/delete never deadlocks.
 */
export async function runWithSSHChallenges<T extends SaveFeedbackPayload>(opts: {
  run: (secret?: SSHSecretSubmission) => Promise<T | null>
  isStillCurrent: () => boolean
  onTrustHostKey: (hostPort: string, algorithm: string, fingerprint: string) => Promise<void>
  onCredentialSaved?: () => void
  maxRounds?: number
}): Promise<SSHChallengeOutcome<T>> {
  const uiStore = useUiStore()
  const maxRounds = opts.maxRounds ?? 4
  let pendingSecret: SSHSecretSubmission | undefined

  for (let round = 0; round <= maxRounds; round++) {
    if (!opts.isStillCurrent()) {
      return { ok: false, aborted: true }
    }
    try {
      const value = await opts.run(pendingSecret)
      if (!value || !opts.isStillCurrent()) {
        return { ok: false, aborted: true }
      }
      handleSaveFeedback(value, opts.onCredentialSaved)
      return { ok: true, value }
    } catch (err) {
      const appErr = parseAppError(err)
      // Always surface independent credential save success/warning even when auth/handshake fails!
      handleSaveFeedback(appErr, opts.onCredentialSaved)
      if (pendingSecret && appErr.credentialSaved) {
        // Already persisted on the pre-auth save entry; do not redundantly save again on retry.
        pendingSecret = { secret: pendingSecret.secret, saveSecret: false }
      }

      if (!opts.isStillCurrent()) {
        return { ok: false, aborted: true }
      }

      if (round < maxRounds && appErr.code === ErrorCode.ErrTunnelFailed && appErr.hostKeyChallenge) {
        const ch = appErr.hostKeyChallenge
        const confirmed = await uiStore.requestConfirm(buildHostKeyConfirmOptions(ch))
        if (!opts.isStillCurrent()) {
          return { ok: false, aborted: true }
        }
        if (!confirmed) {
          return { ok: false, canceled: true }
        }
        try {
          await opts.onTrustHostKey(ch.hostPort, ch.algorithm, ch.fingerprint)
        } catch (trustErr) {
          return { ok: false, error: parseAppError(trustErr) }
        }
        if (!opts.isStillCurrent()) {
          return { ok: false, aborted: true }
        }
        continue
      }

      if (round < maxRounds && appErr.code === ErrorCode.ErrAuthFailed && appErr.sshAuthPrompt) {
        const submission = await uiStore.requestSSHSecret(appErr.sshAuthPrompt)
        if (!opts.isStillCurrent()) {
          return { ok: false, aborted: true }
        }
        if (!submission) {
          return { ok: false, canceled: true }
        }
        pendingSecret = submission.secret === '' && !submission.saveSecret ? undefined : submission
        continue
      }

      return { ok: false, error: appErr }
    }
  }

  return {
    ok: false,
    error: {
      code: ErrorCode.ErrTunnelFailed,
      message: 'SSH 验证重试次数超过限制，请检查跳板机配置后重试',
      retrySafe: false,
    },
  }
}
