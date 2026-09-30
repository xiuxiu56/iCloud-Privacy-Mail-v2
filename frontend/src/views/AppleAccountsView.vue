<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Apple, CheckCircle2, CloudDownload, Eye, EyeOff, KeyRound, LoaderCircle, Mail, MailPlus, Plus, Power, RefreshCw, Server, ShieldCheck, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import CardSelect from '../components/CardSelect.vue'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

const loading = ref(true)
const busyActions = ref([])
const checking = computed(() => busyActions.value.some((action) => action === 'check' || action.startsWith('check:')))
const showLogin = ref(false)
const showIMAP = ref(false)
const showCreate = ref(false)
const showPassword = ref(true)
const showIMAPPassword = ref(true)
const data = ref({ items: [], module_ready: true })
const selected = ref(null)
const pending = reactive({ id: '', code: '', phoneNumber: '' })
const login = reactive({ flow: 'apple_account', apple_id: '', password: '', two_factor_method: 'trusted_device' })
const imap = reactive({ email: '', app_password: '' })
const create = reactive({ label: '', note: '', channel: 'auto' })
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}
const loginFlowOptions = [
  { value: 'apple_account', label: 'Apple Account 新接口', dot: 'bg-violet-500' },
  { value: 'icloud_web', label: 'iCloud Web 旧接口', dot: 'bg-sky-500' },
]
const twoFactorOptions = [
  { value: 'trusted_device', label: '受信任设备', dot: 'bg-emerald-500' },
  { value: 'sms', label: '短信', dot: 'bg-amber-500' },
]
const createChannelOptions = [
  { value: 'auto', label: '自动接口：新接口优先，失败用旧接口', dot: 'bg-slate-400' },
  ...loginFlowOptions,
]

function flash(text, isError = false) {
  if (isError) showError(text)
  else success(text)
}

function isBusy(action) {
  return busyActions.value.includes(action)
}

function startBusy(action) {
  if (!action || isBusy(action)) return false
  busyActions.value = [...busyActions.value, action]
  return true
}

function finishBusy(action) {
  if (!isBusy(action)) return
  busyActions.value = busyActions.value.filter((item) => item !== action)
}

function statusLabel(value) {
  return ({ active: '正常', disabled: '已停用', partial: '部分正常', need_login: '需要登录', need_2fa: '等待 2FA', no_icloud_plus: '无 iCloud+', rate_limited: '访问受限', failed: '失败' })[value] || value || '未知'
}

function stateLabel(kind) {
  return ({ apple_account: 'Apple Account 新接口', icloud_web: 'iCloud Web 旧接口', icloud_imap: 'iCloud IMAP' })[kind] || kind
}

function stateMeta(kind) {
  return ({
    apple_account: { description: '创建隐私邮箱', icon: Apple, tone: 'text-violet-600 bg-violet-50 dark:bg-violet-950/30 dark:text-violet-300' },
    icloud_web: { description: '同步与远端管理', icon: CloudDownload, tone: 'text-sky-600 bg-sky-50 dark:bg-sky-950/30 dark:text-sky-300' },
    icloud_imap: { description: '邮件与验证码', icon: Mail, tone: 'text-amber-600 bg-amber-50 dark:bg-amber-950/30 dark:text-amber-300' },
  })[kind] || { description: '登录态', icon: Server, tone: 'text-slate-500 bg-slate-100 dark:bg-slate-700 dark:text-slate-300' }
}

function stateStatusLabel(state) {
  if (!state.saved) return '未配置'
  if (!state.last_checked_at || String(state.last_checked_at).startsWith('0001-')) return '已保存'
  return state.last_check_ok ? '正常' : '需检查'
}

function stateStatusClass(state) {
  if (!state.saved) return 'bg-slate-100 text-slate-400 dark:bg-slate-700 dark:text-slate-400'
  if (!state.last_checked_at || String(state.last_checked_at).startsWith('0001-')) return 'bg-amber-50 text-amber-600 dark:bg-amber-950/30 dark:text-amber-300'
  return state.last_check_ok ? 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300' : 'bg-rose-50 text-rose-600 dark:bg-rose-950/30 dark:text-rose-300'
}

function accountStatusClass(account) {
  if (account.status === 'disabled') return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
  const status = account.icloud_status || account.status
  if (status === 'active') return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300'
  if (status === 'partial' || status === 'need_2fa' || status === 'need_login') return 'bg-amber-50 text-amber-600 dark:bg-amber-950/30 dark:text-amber-300'
  return 'bg-rose-50 text-rose-600 dark:bg-rose-950/30 dark:text-rose-300'
}

function openLoginDialog() {
  showIMAP.value = false
  showCreate.value = false
  login.apple_id = selected.value?.apple_id || ''
  login.password = selected.value?.password || ''
  showPassword.value = true
  showLogin.value = true
}

function closeLoginDialog(force = false) {
  if (!force && (isBusy('login') || isBusy('2fa'))) return
  showLogin.value = false
  showPassword.value = true
  login.password = ''
  pending.id = ''
  pending.code = ''
  pending.phoneNumber = ''
}

function openIMAPDialog() {
  if (!selected.value) {
    flash('请先选择一个 Apple 账号', true)
    return
  }
  showLogin.value = false
  showCreate.value = false
  showIMAPPassword.value = true
  imap.email = selected.value.imap_email || selected.value.apple_id || ''
  imap.app_password = selected.value.imap_app_password || ''
  showIMAP.value = true
}

function openCreateDialog() {
  if (!selected.value) {
    flash('请先选择一个 Apple 账号', true)
    return
  }
  showLogin.value = false
  showIMAP.value = false
  showCreate.value = true
}

async function load(options = {}) {
  const silent = Boolean(options.silent)
  if (!silent) loading.value = true
  try {
    data.value = await api('/api/apple-accounts')
    if (selected.value) {
      const current = data.value.items.find((item) => item.id === selected.value.id)
      if (current) {
        selected.value = {
          ...current,
          password: selected.value.password || '',
          imap_email: selected.value.imap_email || '',
          imap_app_password: selected.value.imap_app_password || '',
        }
      }
      else selected.value = null
    }
  } catch (err) {
    flash(err.message, true)
  } finally {
    if (!silent) loading.value = false
  }
}

function scheduleRealtimeRefresh(change) {
	if (change.resource === 'apple-account' && change.operation === 'updated' && change.payload?.data?.id) {
		let applied = false
		data.value.items = (data.value.items || []).map((item) => {
			if (item.id !== change.payload.data.id) return item
			applied = true
			return { ...item, ...change.payload.data }
		})
		if (selected.value?.id === change.payload.data.id) {
			selected.value = { ...selected.value, ...change.payload.data }
			applied = true
		}
		if (applied) return
	}
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => load({ silent: true }), 120)
}

async function deleteAccount(account) {
  const name = account.label || account.apple_id || account.id
  const confirmed = await confirmAction({
    title: '删除 Apple 账号',
    message: `确定删除“${name}”吗？\n\n本地登录态、关联隐私邮箱和本地邮件会一并删除；Apple 服务器上的隐私邮箱不会删除。`,
    confirmText: '删除账号',
    tone: 'danger',
  })
  if (!confirmed) return
  const busyKey = `delete:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/apple-accounts/${account.id}`, { method: 'DELETE' })
    if (selected.value?.id === account.id) selected.value = null
    await load()
    const deleted = result.deleted || {}
    flash(`已删除 Apple 账号：${name}；清理邮箱 ${deleted.mailboxes || 0} 个，邮件 ${deleted.messages || 0} 封`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

async function selectAccount(account) {
  const busyKey = `detail:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/apple-accounts/${account.id}`)
    selected.value = result.account
    imap.email = result.account.imap_email || result.account.apple_id || ''
    imap.app_password = result.account.imap_app_password || ''
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

async function startLogin() {
  if (!startBusy('login')) return
  flash('正在与 Apple 建立登录态，请稍候…')
  try {
    const result = await api('/api/apple-accounts/login/start', { method: 'POST', body: JSON.stringify(login) })
    if (result.needs_2fa) {
      pending.id = result.pending_id
      flash(result.message || '请输入 Apple 两步验证码')
    } else {
      closeLoginDialog(true)
      flash(result.message || 'Apple 登录已完成')
      await load()
    }
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('login')
  }
}

async function submit2FA() {
  if (!startBusy('2fa')) return
  try {
    const body = { pending_id: pending.id, code: pending.code }
    if (pending.phoneNumber.trim()) {
      try { body.phone_number = JSON.parse(pending.phoneNumber) } catch { body.phone_number = pending.phoneNumber.trim() }
    }
    const result = await api('/api/apple-accounts/login/2fa', { method: 'POST', body: JSON.stringify(body) })
    closeLoginDialog(true)
    flash(result.message || 'Apple 登录和 2FA 已完成')
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('2fa')
  }
}

async function checkAccount() {
  if (!selected.value) return
  if (checking.value) return
  if (!startBusy('check')) return
  try {
    const result = await api(`/api/apple-accounts/${selected.value.id}/check`, { method: 'POST' })
    selected.value = result.account
    flash('登录态检测完成')
    await load()
  } catch (err) {
    flash(err.message, true)
    await load({ silent: true })
  } finally {
    finishBusy('check')
  }
}

async function checkState(kind) {
  if (!selected.value || !kind) return
  if (checking.value) return
  const busyKey = `check:${kind}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/apple-accounts/${selected.value.id}/check/${encodeURIComponent(kind)}`, { method: 'POST' })
    selected.value = result.account
    flash(`${stateLabel(kind)}检测完成`)
    await load({ silent: true })
  } catch (err) {
    flash(err.message, true)
    await load({ silent: true })
  } finally {
    finishBusy(busyKey)
  }
}

async function toggleAccount(account) {
  const enabled = account.status === 'disabled'
  const busyKey = `status:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/apple-accounts/${account.id}/status`, { method: 'POST', body: JSON.stringify({ enabled }) })
    if (selected.value?.id === account.id) selected.value = { ...selected.value, ...result.account }
    await load({ silent: true })
    flash(`${account.label || account.apple_id || 'Apple 账号'}已${enabled ? '开启' : '停用'}`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

async function saveIMAP() {
  if (!selected.value) return
  if (!startBusy('imap')) return
  try {
    const result = await api(`/api/apple-accounts/${selected.value.id}/imap`, { method: 'POST', body: JSON.stringify(imap) })
    selected.value = result.account
    imap.app_password = ''
    showIMAPPassword.value = true
    showIMAP.value = false
    flash('IMAP App 专用密码已验证并保存')
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('imap')
  }
}

async function createMailbox() {
  if (!selected.value) return
  if (!startBusy('create')) return
  try {
    const result = await api(`/api/apple-accounts/${selected.value.id}/mailboxes`, { method: 'POST', body: JSON.stringify(create) })
    create.label = ''
    create.note = ''
    showCreate.value = false
    flash(`已创建隐私邮箱：${result.mailbox.email}`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('create')
  }
}

async function syncMailboxes() {
  if (!selected.value) return
  if (!startBusy('sync')) return
  try {
    const result = await api(`/api/apple-accounts/${selected.value.id}/mailboxes/sync`, { method: 'POST' })
    showCreate.value = false
    flash(`已从 Apple 同步 ${result.count} 个隐私邮箱`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('sync')
  }
}

onMounted(() => {
  load()
  realtimeUnsubscribe = subscribeRealtime(['apple-account', 'apple-session'], scheduleRealtimeRefresh)
})

onBeforeUnmount(() => {
  window.clearTimeout(realtimeRefreshTimer)
  realtimeUnsubscribe()
})
</script>

<template>
  <div class="apple-account-page">
    <section class="panel apple-account-workbench">
      <header class="apple-command-bar">
        <div class="apple-command-title"><span><Apple :size="16" /></span><div><h2>Apple 账号</h2><p>管理登录态、IMAP 与隐私邮箱通道</p></div></div>
        <div class="apple-command-actions">
          <button class="primary-button apple-command-button" @click="openLoginDialog"><Plus :size="14" />添加 Apple 账号</button>
          <button class="secondary-button apple-command-button" :disabled="!selected" :title="selected ? `为 ${selected.apple_id} 配置 IMAP` : '请先选择一个 Apple 账号'" @click="openIMAPDialog"><KeyRound :size="14" />IMAP 取码</button>
          <button class="secondary-button apple-command-button" :disabled="!selected" :title="selected ? `使用 ${selected.apple_id} 创建隐私邮箱` : '请先选择一个 Apple 账号'" @click="openCreateDialog"><MailPlus :size="14" />创建隐私邮箱</button>
        </div>
      </header>

      <div v-if="loading" class="apple-loading"><LoaderCircle :size="16" class="animate-spin" />正在加载 Apple 账号</div>
      <div v-else class="apple-account-body">
        <section class="apple-account-list">
          <header class="apple-section-heading"><div><h3>账号与登录态</h3><p>选择账号后可在右侧查看各通道状态</p></div><span>{{ data.items?.length || 0 }} 个账号</span></header>
          <div v-if="!data.items?.length" class="apple-empty"><span><Server :size="20" /></span><strong>还没有 Apple 账号</strong><small>点击“添加 Apple 账号”完成首次协议登录。</small></div>
          <div v-else class="apple-account-table-viewport">
            <table class="apple-account-table">
              <colgroup><col class="apple-col-account" /><col class="apple-col-status" /><col class="apple-col-channel" /><col class="apple-col-action" /></colgroup>
              <thead><tr><th>Apple 账号</th><th>状态</th><th>登录通道</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="account in data.items" :key="account.id" class="apple-account-row" :class="{ 'apple-account-row-selected': selected?.id === account.id }" tabindex="0" :aria-label="`查看 ${account.label || account.apple_id || 'Apple 账号'} 的登录态详情`" @click="selectAccount(account)" @keydown.enter="selectAccount(account)" @keydown.space.prevent="selectAccount(account)">
                  <td><button type="button" class="apple-account-select" :disabled="isBusy(`delete:${account.id}`) || isBusy(`detail:${account.id}`)" @click.stop="selectAccount(account)"><span class="apple-account-icon"><LoaderCircle v-if="isBusy(`detail:${account.id}`)" :size="14" class="animate-spin" /><Apple v-else :size="14" /></span><span><strong>{{ account.label || account.apple_id || 'Apple 账号' }}</strong><small>{{ account.apple_id || account.id }}</small></span></button></td>
                  <td><span :class="accountStatusClass(account)" class="apple-status-badge">{{ statusLabel(account.status === 'disabled' ? 'disabled' : (account.icloud_status || account.status)) }}</span></td>
                  <td><div class="apple-channel-list"><span v-for="state in account.login_states" :key="state.kind" class="apple-channel-pill"><component :is="stateMeta(state.kind).icon" :size="11" /><span>{{ stateLabel(state.kind) }}</span><em :class="stateStatusClass(state)">{{ stateStatusLabel(state) }}</em></span><span v-if="!account.login_states?.length" class="apple-channel-empty">暂无登录态</span></div></td>
                  <td><div class="apple-row-actions"><button type="button" class="apple-toggle-button" :class="{ 'apple-toggle-button-disabled': account.status === 'disabled' }" :title="account.status === 'disabled' ? '开启账号' : '停用账号'" :aria-label="account.status === 'disabled' ? '开启账号' : '停用账号'" :disabled="isBusy(`status:${account.id}`)" @click.stop="toggleAccount(account)"><LoaderCircle v-if="isBusy(`status:${account.id}`)" :size="13" class="animate-spin" /><Power v-else :size="13" /></button><button type="button" class="apple-delete-button" :title="`删除 ${account.label || account.apple_id || 'Apple 账号'}`" :aria-label="`删除 ${account.label || account.apple_id || 'Apple 账号'}`" :disabled="isBusy(`delete:${account.id}`)" @click.stop="deleteAccount(account)"><LoaderCircle v-if="isBusy(`delete:${account.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button></div></td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <aside class="apple-detail-panel">
          <div v-if="!selected" class="apple-detail-empty"><span><Apple :size="20" /></span><strong>选择一个 Apple 账号</strong><small>登录态详情和检测结果会显示在这里。</small></div>
          <template v-else>
            <header class="apple-detail-heading"><div><span>所选 Apple 账号</span><h3>{{ selected.label || selected.apple_id }}</h3><p>{{ selected.apple_id }}</p></div><button class="secondary-button apple-check-button" :disabled="checking" @click="checkAccount"><LoaderCircle v-if="checking" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />{{ checking ? '检查中' : '检测登录态' }}</button></header>
            <div class="apple-state-list">
              <article v-for="state in selected.login_states" :key="state.kind" class="apple-state-row"><span :class="stateMeta(state.kind).tone" class="apple-state-icon"><component :is="stateMeta(state.kind).icon" :size="14" /></span><span class="apple-state-copy"><strong>{{ stateLabel(state.kind) }}</strong><small>{{ isBusy('check') || isBusy(`check:${state.kind}`) ? `正在检查 ${stateLabel(state.kind)} 登录态…` : (state.last_status_message || stateMeta(state.kind).description) }}</small></span><span v-if="isBusy('check') || isBusy(`check:${state.kind}`)" class="apple-state-checking"><LoaderCircle :size="10" class="animate-spin" />检查中</span><template v-else><span :class="stateStatusClass(state)" class="apple-state-status">{{ stateStatusLabel(state) }}</span><button type="button" class="apple-state-check-button" :title="`检测${stateLabel(state.kind)}登录态`" :aria-label="`检测${stateLabel(state.kind)}登录态`" :disabled="checking || !state.saved" @click="checkState(state.kind)"><RefreshCw :size="12" /></button></template></article>
              <div v-if="!selected.login_states?.length" class="apple-state-empty">该账号还没有已保存的登录态</div>
            </div>
          </template>
        </aside>
      </div>
    </section>

    <div v-if="showLogin" class="mailbox-dialog-backdrop" role="presentation" @click.self="closeLoginDialog()">
      <section role="dialog" aria-modal="true" aria-labelledby="apple-login-title" class="panel mailbox-operation-dialog apple-login-dialog">
        <header class="mailbox-dialog-heading"><div><h2 id="apple-login-title"><Apple :size="17" />登录 Apple 账号</h2><p>登录凭据仅用于本次 Apple 协议请求；保存的是本地登录态。</p></div><button class="icon-button" title="关闭" :disabled="isBusy('login') || isBusy('2fa')" @click="closeLoginDialog()"><X :size="16" /></button></header>
        <form v-if="!pending.id" @submit.prevent="startLogin">
          <div class="apple-dialog-grid"><div class="form-group"><span class="form-label">登录通道</span><CardSelect v-model="login.flow" :options="loginFlowOptions" aria-label="登录通道" /><span class="form-help">新接口用于创建；旧接口支持同步、删除和 Web 收信。</span></div><div class="form-group"><span class="form-label">两步验证方式</span><CardSelect v-model="login.two_factor_method" :options="twoFactorOptions" aria-label="两步验证方式" /><span class="form-help">优先使用受信任设备弹出的验证码。</span></div><label class="form-group"><span class="form-label">Apple ID</span><span class="field-wrap apple-auth-field"><Mail :size="15" class="field-icon" /><input v-model.trim="login.apple_id" class="field field-leading" autocomplete="username" placeholder="name@example.com" required /></span></label><label class="form-group"><span class="form-label">Apple ID 密码</span><span class="field-wrap apple-auth-field"><KeyRound :size="15" class="field-icon" /><input v-model="login.password" class="field field-leading field-trailing" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" placeholder="输入 Apple ID 密码" required /><button type="button" class="apple-field-toggle" :title="showPassword ? '隐藏密码' : '显示密码'" @click="showPassword = !showPassword"><EyeOff v-if="showPassword" :size="15" /><Eye v-else :size="15" /></button></span></label></div>
          <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('login')" @click="closeLoginDialog()">取消</button><button class="primary-button" :disabled="isBusy('login')"><LoaderCircle v-if="isBusy('login')" :size="14" class="animate-spin" /><ShieldCheck v-else :size="14" />开始登录</button></footer>
        </form>
        <form v-else @submit.prevent="submit2FA"><div class="apple-dialog-grid"><label class="form-group"><span class="form-label">Apple 验证码</span><input v-model.trim="pending.code" class="field font-mono tracking-[0.3em]" inputmode="numeric" maxlength="8" placeholder="000000" required /><span class="form-help">输入受信任设备或短信收到的验证码。</span></label><label class="form-group"><span class="form-label">短信号码参数（可选）</span><input v-model.trim="pending.phoneNumber" class="field" placeholder='例如 {"id":1}' /><span class="form-help">只有短信流程要求选择号码时才填写。</span></label></div><footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('2fa')" @click="closeLoginDialog()">取消</button><button class="primary-button" :disabled="isBusy('2fa')"><LoaderCircle v-if="isBusy('2fa')" :size="14" class="animate-spin" /><CheckCircle2 v-else :size="14" />提交验证码</button></footer></form>
      </section>
    </div>

    <div v-if="showIMAP" class="mailbox-dialog-backdrop" role="presentation" @click.self="showIMAP = false">
      <form role="dialog" aria-modal="true" aria-labelledby="imap-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="saveIMAP">
        <header class="mailbox-dialog-heading"><div><h2 id="imap-title"><KeyRound :size="17" />IMAP 取码</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('imap')" @click="showIMAP = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields"><label class="form-group"><span class="form-label">iCloud 邮箱</span><input v-model.trim="imap.email" class="field" type="email" placeholder="name@icloud.com" required /></label><label class="form-group"><span class="form-label">App 专用密码</span><span class="field-wrap apple-auth-field"><KeyRound :size="15" class="field-icon" /><input v-model="imap.app_password" class="field field-leading field-trailing" :type="showIMAPPassword ? 'text' : 'password'" placeholder="xxxx-xxxx-xxxx-xxxx" required /><button type="button" class="apple-field-toggle" :title="showIMAPPassword ? '隐藏 App 专用密码' : '显示 App 专用密码'" @click="showIMAPPassword = !showIMAPPassword"><EyeOff v-if="showIMAPPassword" :size="15" /><Eye v-else :size="15" /></button></span><span class="form-help">保存前会连接 imap.mail.me.com 验证。</span></label></div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('imap')" @click="showIMAP = false">取消</button><button class="primary-button" :disabled="isBusy('imap')"><LoaderCircle v-if="isBusy('imap')" :size="14" class="animate-spin" /><ShieldCheck v-else :size="14" />验证并保存</button></footer>
      </form>
    </div>

    <div v-if="showCreate" class="mailbox-dialog-backdrop" role="presentation" @click.self="showCreate = false">
      <form role="dialog" aria-modal="true" aria-labelledby="create-mailbox-title" class="panel mailbox-operation-dialog apple-create-dialog" @submit.prevent="createMailbox">
        <header class="mailbox-dialog-heading"><div><h2 id="create-mailbox-title"><MailPlus :size="17" />创建隐私邮箱</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('create') || isBusy('sync')" @click="showCreate = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields"><div class="form-group"><span class="form-label">创建通道</span><CardSelect v-model="create.channel" :options="createChannelOptions" aria-label="创建通道" /></div><div class="apple-create-meta"><label class="form-group"><span class="form-label">标签前缀</span><input v-model.trim="create.label" class="field" placeholder="可选，默认 x" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="create.note" class="field" placeholder="可选备注" /></label></div><span class="form-help">标签留空时默认使用 x，并自动生成连续编号。</span></div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('sync')" @click="syncMailboxes"><LoaderCircle v-if="isBusy('sync')" :size="14" class="animate-spin" /><CloudDownload v-else :size="14" />{{ isBusy('sync') ? '同步中' : '同步已有' }}</button><button class="primary-button" :disabled="isBusy('create')"><LoaderCircle v-if="isBusy('create')" :size="14" class="animate-spin" /><Plus v-else :size="14" />{{ isBusy('create') ? '创建中' : '创建邮箱' }}</button></footer>
      </form>
    </div>
  </div>
</template>
