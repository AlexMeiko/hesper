import { describe, expect, it, vi, beforeEach } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { mount } from '@vue/test-utils'
import { computed, defineComponent, nextTick } from 'vue'
import { api, parseAppError } from '@/api/client'
import { EngineType, ErrorCode, SessionState, type ConnectResult, type TabSessionInfo } from '@/api/types'
import ConnectionModal from '@/components/ConnectionModal.vue'
import SessionBanner from '@/components/SessionBanner.vue'
import SSHSecretModal from '@/components/SSHSecretModal.vue'
import { useConnectionsStore } from '@/stores/connections'
import { useUiStore } from '@/stores/ui'
import { useWorkspaceStore, type QueryTab } from '@/stores/workspace'

function makeConnectResult(connId = 'conn-1'): ConnectResult {
  return {
    connectionId: connId,
    detectedType: EngineType.EngineMySQL,
    serverVersion: '8.4.0',
    currentDb: 'app_db',
    capabilities: {
      hasDatabases: true,
      hasSchemas: false,
      switchDbReconnects: false,
      supportsCancel: true,
      supportsViews: true,
      supportsMatViews: false,
      supportsRoutines: false,
      supportsTriggers: false,
      supportsSequences: false,
      supportsSavepoint: true,
      supportsExplain: true,
      ddlTransactional: false,
      supportsSingleStmtAlterAll: false,
    },
  }
}

describe('SSH/TLS Error Passthrough, SessionEpoch Gating & Interactive Prompts', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.restoreAllMocks()
  })

  it('parseAppError preserves hostKeyChallenge, sshAuthPrompt, sessionEpoch, credentialSaved, and saveWarning', () => {
    const rawJson = JSON.stringify({
      code: ErrorCode.ErrTunnelFailed,
      message: '首次连接 SSH 主机',
      retrySafe: false,
      sessionState: SessionState.SessionLost,
      sessionEpoch: 7,
      hostKeyChallenge: {
        hostPort: 'bastion.internal:22',
        algorithm: 'ssh-ed25519',
        fingerprint: 'SHA256:abcdef',
        mismatch: true,
        previousFingerprint: 'SHA256:old',
      },
      sshAuthPrompt: {
        connectionId: 'conn-1',
        hostPort: 'bastion.internal:22',
        username: 'ops',
        authMethod: 'password',
        reason: 'SSH 认证失败',
      },
      credentialSaved: true,
      saveWarning: '钥匙串写入失败',
    })

    const parsedFromError = parseAppError(new Error(`rpc failed: ${rawJson}`))
    expect(parsedFromError.code).toBe(ErrorCode.ErrTunnelFailed)
    expect(parsedFromError.sessionEpoch).toBe(7)
    expect(parsedFromError.hostKeyChallenge?.fingerprint).toBe('SHA256:abcdef')
    expect(parsedFromError.hostKeyChallenge?.mismatch).toBe(true)
    expect(parsedFromError.hostKeyChallenge?.previousFingerprint).toBe('SHA256:old')
    expect(parsedFromError.sshAuthPrompt?.username).toBe('ops')
    expect(parsedFromError.credentialSaved).toBe(true)
    expect(parsedFromError.saveWarning).toBe('钥匙串写入失败')
  })

  it('strictly gates both background events and RPC responses by sessionEpoch (rejects older and epoch=0)', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()

    connStore.activeMap['conn-1'] = makeConnectResult('conn-1')
    vi.spyOn(api, 'openTabSession').mockResolvedValue({
      tabId: 'q-1',
      connectionId: 'conn-1',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 4,
      serverConnId: '101',
      readOnly: false,
    })
    vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue(null)

    let resolveExec!: (v: any) => void
    vi.spyOn(api, 'executeSQL').mockImplementation(
      () =>
        new Promise((r) => {
          resolveExec = r
        }),
    )

    const tab = (await wsStore.openQueryTab('conn-1', 'app_db', '', 'SELECT 1')) as QueryTab
    expect(tab).not.toBeNull()
    expect(tab.sessionEpoch).toBe(4)

    // Start query (will finish with stale epoch=4).
    const execPromise = wsStore.executeQuery(tab.id, { runMode: 'all', cursorOffset: 0 })

    // Background SSH disconnect event arrives FIRST with epoch=5!
    wsStore.applySessionStateEvent({
      tabId: tab.id,
      connectionId: 'conn-1',
      sessionState: SessionState.SessionLost,
      sessionEpoch: 5,
    })
    expect(tab.sessionState).toBe(SessionState.SessionLost)
    expect(tab.sessionEpoch).toBe(5)
    expect(tab.bannerError?.code).toBe(ErrorCode.ErrSessionLost)

    // Late query response with epoch=4 arrives AFTER disconnect -> MUST NOT overwrite SessionLost!
    resolveExec({
      tabId: tab.id,
      execId: 'exec-1',
      currentDb: 'app_db',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 4,
      statements: [],
      totalMs: 12,
    })
    await execPromise

    expect(tab.sessionState).toBe(SessionState.SessionLost)
    expect(tab.sessionEpoch).toBe(5)
    expect(tab.bannerError?.code).toBe(ErrorCode.ErrSessionLost)

    // Unversioned / epoch=0 late switch response MUST ALSO be rejected once tab has sessionEpoch > 0!
    vi.spyOn(api, 'switchTabDatabase').mockResolvedValue({
      tabId: tab.id,
      connectionId: 'conn-1',
      currentDb: 'other_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 0,
      serverConnId: '101',
      readOnly: false,
    })
    await wsStore.switchQueryDatabase(tab.id, 'other_db')
    expect(tab.sessionState).toBe(SessionState.SessionLost)
    expect(tab.sessionEpoch).toBe(5)

    // ForceAbort returning SessionCommitUnknown with epoch=6 MUST preserve CommitOutcomeUnknown banner!
    vi.spyOn(api, 'forceAbortSession').mockResolvedValue({
      tabId: tab.id,
      connectionId: 'conn-1',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionCommitUnknown,
      sessionEpoch: 6,
      serverConnId: '',
      readOnly: false,
    })
    await wsStore.forceAbortQuery(tab.id)
    expect(tab.sessionState).toBe(SessionState.SessionCommitUnknown)
    expect(tab.sessionEpoch).toBe(6)
    expect(tab.bannerError?.code).toBe(ErrorCode.ErrCommitOutcomeUnknown)
  })

  it('handles openQueryTab SSH password retry even when activeMap already exists after tunnel drop', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()
    const uiStore = useUiStore()

    // Connection appears active in sidebar, but SSH tunnel dropped in background.
    connStore.activeMap['conn-ssh'] = makeConnectResult('conn-ssh')
    vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue(null)
    vi.spyOn(api, 'listConnections').mockResolvedValue([])

    vi.spyOn(api, 'openTabSession').mockRejectedValue({
      code: ErrorCode.ErrAuthFailed,
      message: '请输入 SSH 密码',
      retrySafe: false,
      sshAuthPrompt: {
        connectionId: 'conn-ssh',
        hostPort: 'bastion:22',
        username: 'ops',
        authMethod: 'password',
        reason: '请输入 SSH 密码',
      },
    })

    const withSecretSpy = vi.spyOn(api, 'openTabSessionWithSSHSecret').mockResolvedValue({
      tabId: 'tab-x',
      connectionId: 'conn-ssh',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 10,
      serverConnId: '777',
      readOnly: false,
      credentialSaved: true,
    } as TabSessionInfo)

    const openPromise = wsStore.openQueryTab('conn-ssh', 'app_db')
    await nextTick()
    await nextTick()
    expect(uiStore.sshSecretPrompt?.hostPort).toBe('bastion:22')
    uiStore.resolveSSHSecret({ secret: 'temp-pw', saveSecret: true })

    const tab = (await openPromise) as QueryTab
    expect(tab).not.toBeNull()
    expect(withSecretSpy).toHaveBeenCalledWith(tab.id, 'conn-ssh', 'app_db', '', 'temp-pw', true)
    expect(tab.sessionState).toBe(SessionState.SessionAliveIdle)
    expect(tab.sessionEpoch).toBe(10)
  })

  it('connects through consecutive hostKeyChallenge + sshAuthPrompt, refreshes on pre-auth save even when auth fails, and never deadlocks on concurrent disconnect', async () => {
    const connStore = useConnectionsStore()
    const uiStore = useUiStore()

    const listSpy = vi.spyOn(api, 'listConnections').mockResolvedValue([])
    vi.spyOn(api, 'listDatabases').mockResolvedValue([{ name: 'app_db', isSystem: false }])
    const trustSpy = vi.spyOn(api, 'trustSSHHostKey').mockResolvedValue()

    // Round 0: unknown host key
    vi.spyOn(api, 'connect')
      .mockRejectedValueOnce({
        code: ErrorCode.ErrTunnelFailed,
        message: '首次连接 SSH 主机',
        retrySafe: false,
        hostKeyChallenge: {
          hostPort: 'bastion:22',
          algorithm: 'ssh-ed25519',
          fingerprint: 'SHA256:first',
          mismatch: false,
        },
      })
      // Round 1: needs password
      .mockRejectedValueOnce({
        code: ErrorCode.ErrAuthFailed,
        message: '请输入 SSH 密码',
        retrySafe: false,
        sshAuthPrompt: {
          connectionId: 'conn-1',
          hostPort: 'bastion:22',
          username: 'ops',
          authMethod: 'password',
        },
      })

    // Round 2 (wrong password, saveSecret=true): backend saved BEFORE auth -> credentialSaved=true AND auth error!
    // Round 3 (right password, saveSecret=false): succeeds!
    const withSecretSpy = vi
      .spyOn(api, 'connectWithSSHSecret')
      .mockRejectedValueOnce({
        code: ErrorCode.ErrAuthFailed,
        message: 'SSH 认证失败',
        retrySafe: false,
        credentialSaved: true,
        saveWarning: '同步标志告警测试',
        sshAuthPrompt: {
          connectionId: 'conn-1',
          hostPort: 'bastion:22',
          username: 'ops',
          authMethod: 'password',
          reason: '密码错误，请重试',
        },
      })
      .mockResolvedValueOnce(makeConnectResult('conn-1'))

    const connectPromise = connStore.connect('conn-1')
    await vi.waitFor(() => expect(uiStore.confirmDialog).not.toBeNull())
    uiStore.resolveConfirm(true)

    await vi.waitFor(() => expect(uiStore.sshSecretPrompt).not.toBeNull())
    uiStore.resolveSSHSecret({ secret: 'wrong-pw', saveSecret: true })

    // Even though auth failed on wrong-pw, credentialSaved=true MUST trigger loadConnections()
    // AND saveWarning MUST show an independent warning toast!
    await vi.waitFor(() => expect(listSpy).toHaveBeenCalledTimes(1))
    expect(uiStore.toasts.some((t) => t.kind === 'warning' && t.detail?.includes('同步标志告警测试'))).toBe(true)

    await vi.waitFor(() => expect(uiStore.sshSecretPrompt?.reason).toContain('密码错误'))
    uiStore.resolveSSHSecret({ secret: 'right-pw', saveSecret: false })

    const res = await connectPromise
    expect(res?.connectionId).toBe('conn-1')
    expect(trustSpy).toHaveBeenCalledWith('bastion:22', 'ssh-ed25519', 'SHA256:first')
    expect(withSecretSpy).toHaveBeenCalledTimes(2)

    // Concurrent disconnect while SSH password dialog is open MUST NOT deadlock lifecycleChains!
    const disconnectSpy = vi.spyOn(api, 'disconnect').mockResolvedValue()
    vi.spyOn(api, 'connect').mockRejectedValueOnce({
      code: ErrorCode.ErrAuthFailed,
      message: '请输入密码',
      retrySafe: false,
      sshAuthPrompt: { connectionId: 'conn-2', hostPort: 'b2:22', username: 'ops', authMethod: 'password' },
    })

    const stalledConnect = connStore.connect('conn-2')
    await vi.waitFor(() => expect(uiStore.sshSecretPrompt?.hostPort).toBe('b2:22'))

    // Disconnect completes immediately without waiting for the dialog to close!
    await connStore.disconnect('conn-2')
    expect(disconnectSpy).toHaveBeenCalledWith('conn-2')

    // Even if user later clicks submit on the stale dialog, it aborts cleanly without reviving conn-2!
    uiStore.resolveSSHSecret({ secret: 'late-pw', saveSecret: false })
    expect(await stalledConnect).toBeNull()
    expect(connStore.activeMap['conn-2']).toBeUndefined()
  })

  it('ConnectionModal toggles TLS 5 modes, hides SSH on SQLite, strips hidden TLS cert paths, and retries TestConnection on hostKeyChallenge', async () => {
    const uiStore = useUiStore()
    uiStore.openNewConnectionModal()

    const wrapper = mount(ConnectionModal, {
      global: {
        stubs: {
          teleport: true,
        },
      },
    })
    await nextTick()

    // TLS verify-full shows CA + mTLS; switching back to disable strips them from submitted payload.
    const tlsSelect = wrapper.get('[data-testid="tls-mode-select"]')
    await tlsSelect.setValue('verify-full')
    await wrapper.get('[data-testid="tls-ca-input"]').setValue('/tmp/ca.pem')
    await wrapper.get('[data-testid="tls-cert-input"]').setValue('/tmp/client.pem')
    await wrapper.get('[data-testid="tls-key-input"]').setValue('/tmp/client.key')

    await tlsSelect.setValue('require')
    expect(wrapper.find('[data-testid="tls-ca-input"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="tls-cert-input"]').exists()).toBe(true)

    // Enable SSH & trigger TestConnection -> hits hostKeyChallenge -> confirm -> retries -> success!
    await wrapper.get('[data-testid="ssh-enabled-checkbox"]').setValue(true)
    await wrapper.get('[data-testid="ssh-host-input"]').setValue('bastion.internal')
    await wrapper.get('[data-testid="ssh-username-input"]').setValue('ops')
    await wrapper.get('[data-testid="ssh-password-input"]').setValue('ssh-pw')

    const trustSpy = vi.spyOn(api, 'trustSSHHostKey').mockResolvedValue()
    const testSpy = vi
      .spyOn(api, 'testConnection')
      .mockResolvedValueOnce({
        success: false,
        detectedType: EngineType.EngineMySQL,
        serverVersion: '',
        latencyMs: 12,
        capabilities: makeConnectResult().capabilities,
        error: {
          code: ErrorCode.ErrTunnelFailed,
          message: '首次连接 SSH 主机',
          retrySafe: false,
          hostKeyChallenge: {
            hostPort: 'bastion.internal:22',
            algorithm: 'ssh-ed25519',
            fingerprint: 'SHA256:modal-fp',
            mismatch: false,
          },
        },
      })
      .mockResolvedValueOnce({
        success: true,
        detectedType: EngineType.EngineMySQL,
        serverVersion: '8.4.0',
        latencyMs: 20,
        capabilities: makeConnectResult().capabilities,
      })

    const clickPromise = wrapper.get('[data-testid="test-connection-btn"]').trigger('click')
    await vi.waitFor(() => expect(uiStore.confirmDialog).not.toBeNull())
    uiStore.resolveConfirm(true)
    await clickPromise
    await vi.waitFor(() => expect(testSpy).toHaveBeenCalledTimes(2))

    expect(trustSpy).toHaveBeenCalledWith('bastion.internal:22', 'ssh-ed25519', 'SHA256:modal-fp')
    // Hidden caCertPath MUST have been stripped because mode is "require"!
    const firstReq = testSpy.mock.calls[0][0]
    expect(firstReq.params.tls.mode).toBe('require')
    expect(firstReq.params.tls.caCertPath).toBeUndefined()
    expect(firstReq.params.tls.certPath).toBe('/tmp/client.pem')
    expect(firstReq.params.ssh?.enabled).toBe(true)
    expect(firstReq.params.ssh?.saveCredential).toBe(true)

    // Switching driver to SQLite hides SSH panel completely.
    const sqliteBtn = wrapper.findAll('button').find((b) => b.text().trim() === 'SQLite')!
    await sqliteBtn.trigger('click')
    expect(wrapper.find('[data-testid="ssh-tunnel-section"]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('reactively renders the reconnect banner button when initial SSH prompt is canceled, then clicks it to OpenTabSession (same tabID) and uses ReconnectTabSession afterward', async () => {
    const connStore = useConnectionsStore()
    const wsStore = useWorkspaceStore()
    const uiStore = useUiStore()

    connStore.activeMap['conn-1'] = makeConnectResult('conn-1')
    vi.spyOn(api, 'fetchCompletionCatalog').mockResolvedValue(null)

    const sshPromptErr = {
      code: ErrorCode.ErrAuthFailed,
      message: 'SSH 认证需要密码',
      retrySafe: false,
      sshAuthPrompt: {
        connectionId: 'conn-1',
        hostPort: 'bastion.internal:22',
        username: 'ops',
        authMethod: 'password',
        reason: '请输入 SSH 密码',
      },
    }

    const openSpy = vi.spyOn(api, 'openTabSession').mockRejectedValue(sshPromptErr)
    const openSecretSpy = vi.spyOn(api, 'openTabSessionWithSSHSecret').mockResolvedValue({
      tabId: '',
      connectionId: 'conn-1',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 20,
      serverConnId: '501',
      readOnly: false,
    })
    const reconnectSpy = vi.spyOn(api, 'reconnectTabSession').mockResolvedValue({
      tabId: '',
      connectionId: 'conn-1',
      currentDb: 'app_db',
      currentSchema: '',
      sessionState: SessionState.SessionAliveIdle,
      sessionEpoch: 25,
      serverConnId: '502',
      readOnly: false,
    })

    // Mount a reactive host observing wsStore.activeTab just like QueryTabView.vue does!
    const Host = defineComponent({
      components: { SessionBanner },
      setup() {
        const activeQueryTab = computed(() =>
          wsStore.activeTab?.kind === 'query' ? wsStore.activeTab : null,
        )
        return { activeQueryTab, wsStore }
      },
      template: `<SessionBanner
        v-if="activeQueryTab"
        :error="activeQueryTab.bannerError"
        :session-state="activeQueryTab.sessionState"
        @reconnect="wsStore.reconnectQueryTab(activeQueryTab.id)"
      />`,
    })
    const hostWrapper = mount(Host)

    // 1. Open query tab -> SSH password prompt appears -> user cancels.
    const openPromise = wsStore.openQueryTab('conn-1', 'app_db', '', 'SELECT 42;')
    await vi.waitFor(() => expect(uiStore.sshSecretPrompt).not.toBeNull())
    uiStore.resolveSSHSecret(null)
    const tab = await openPromise
    await nextTick()

    expect(tab).not.toBeNull()
    expect(tab!.sql).toBe('SELECT 42;')
    expect(tab!.sessionState).toBe(SessionState.SessionLost)
    expect(tab!.sessionEstablished).toBe(false)

    // Reactivity proof: the SessionLost banner and visible reconnect button MUST actually render in the DOM!
    const lostBanner = hostWrapper.find('[data-testid="session-lost-banner"]')
    expect(lostBanner.exists()).toBe(true)
    const reconnectBtn = lostBanner.find('button')
    expect(reconnectBtn.exists()).toBe(true)
    expect(reconnectBtn.text()).toContain('在新会话中重连')

    // User continues editing SQL before clicking the rendered reconnect button.
    wsStore.setQuerySql(tab!.id, 'SELECT 42, now();')

    // 2. Click the actual DOM reconnect button -> MUST call OpenTabSession with SAME tab.id, NOT ReconnectTabSession!
    const clickPromise = reconnectBtn.trigger('click')
    await vi.waitFor(() => expect(uiStore.sshSecretPrompt).not.toBeNull())
    uiStore.resolveSSHSecret({ secret: 'ssh-pw', saveSecret: false })
    await clickPromise
    await vi.waitFor(() => expect(tab!.sessionEstablished).toBe(true))
    await nextTick()

    expect(openSpy).toHaveBeenCalledTimes(2)
    expect(openSecretSpy).toHaveBeenCalledWith(tab!.id, 'conn-1', 'app_db', '', 'ssh-pw', false)
    expect(reconnectSpy).not.toHaveBeenCalled()
    expect(tab!.sessionState).toBe(SessionState.SessionAliveIdle)
    expect(tab!.sessionEpoch).toBe(20)
    expect(tab!.sql).toBe('SELECT 42, now();')
    expect(hostWrapper.find('[data-testid="session-lost-banner"]').exists()).toBe(false)

    // 3. Subsequent background disconnect -> banner reappears -> clicking it now calls ReconnectTabSession!
    wsStore.applySessionStateEvent({
      tabId: tab!.id,
      connectionId: 'conn-1',
      sessionState: SessionState.SessionLost,
      sessionEpoch: 22,
    })
    await nextTick()
    expect(hostWrapper.find('[data-testid="session-lost-banner"]').exists()).toBe(true)

    await hostWrapper.get('[data-testid="session-lost-banner"] button').trigger('click')
    await vi.waitFor(() => expect(reconnectSpy).toHaveBeenCalledTimes(1))
    expect(reconnectSpy).toHaveBeenCalledWith(tab!.id, 'app_db', '')
    expect(tab!.sessionState).toBe(SessionState.SessionAliveIdle)
    expect(tab!.sessionEpoch).toBe(25)

    hostWrapper.unmount()
  })

  it('SSHSecretModal shows key-check retry guidance in agent mode and aligns TestConnection form-save vs Connect immediate-save wording', async () => {
    const uiStore = useUiStore()
    const wrapper = mount(SSHSecretModal, {
      global: { stubs: { teleport: true } },
    })

    // 1. Agent mode: no password input or save checkbox, retry enabled immediately.
    const agentPromise = uiStore.requestSSHSecret({
      connectionId: 'conn-agent',
      hostPort: 'bastion.internal:22',
      username: 'ops',
      authMethod: 'agent',
      reason: 'SSH Agent 认证失败，请检查已加载的密钥后重试',
    })
    await nextTick()

    expect(wrapper.find('[data-testid="ssh-secret-input"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="ssh-secret-save-checkbox"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('检查已加载的密钥')
    const submitBtn = wrapper.get('[data-testid="ssh-secret-submit-btn"]')
    expect((submitBtn.element as HTMLButtonElement).disabled).toBe(false)

    await wrapper.get('form').trigger('submit')
    expect(await agentPromise).toEqual({ secret: '', saveSecret: false })

    // 2. TestConnection form-save mode: wording clearly states "随连接配置保存", not immediate write.
    const formSavePromise = uiStore.requestSSHSecret(
      {
        connectionId: '',
        hostPort: 'bastion.internal:22',
        username: 'ops',
        authMethod: 'password',
        reason: '请输入 SSH 密码',
      },
      { saveMode: 'form' },
    )
    await nextTick()
    expect(wrapper.text()).toContain('随连接配置保存 SSH 密码')
    expect(wrapper.text()).not.toContain('立即覆盖写入安全存储')
    uiStore.resolveSSHSecret(null)
    await formSavePromise

    // 3. Normal Connect immediate-save mode (default): keeps immediate pre-auth overwrite wording.
    const immSavePromise = uiStore.requestSSHSecret({
      connectionId: 'conn-1',
      hostPort: 'bastion.internal:22',
      username: 'ops',
      authMethod: 'password',
      reason: '请输入 SSH 密码',
    })
    await nextTick()
    expect(wrapper.text()).toContain('立即覆盖写入安全存储')
    uiStore.resolveSSHSecret(null)
    await immSavePromise

    wrapper.unmount()
  })
})
