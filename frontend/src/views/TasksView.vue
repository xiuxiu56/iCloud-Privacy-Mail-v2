<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { CheckCircle2, CircleAlert, Clock3, LoaderCircle, Play, Save, Settings, Square, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import CardSelect from '../components/CardSelect.vue'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

const loading = ref(true)
const busy = ref('')
const initialized = ref(false)
const showDefaults = ref(false)
const scheduler = ref({ running: false, status: 'idle', events: [] })
const accounts = ref([])
const form = reactive({ mode: 'once', account_id: '', account_ids: [], label: '', note: '', create_channel: 'auto', interval_min_minutes: 60, interval_max_minutes: 60, account_interval_min_seconds: 5, account_interval_max_seconds: 5 })
const defaultForm = reactive({ mode: 'once', label: '', note: '', account_ids: [], create_channel: 'auto', scheduler_create_channel: 'auto', apple_account_two_factor_method: 'trusted_device', icloud_web_two_factor_method: 'trusted_device', scheduler_interval_min_minutes: 60, scheduler_interval_max_minutes: 60, scheduler_account_interval_min_seconds: 5, scheduler_account_interval_max_seconds: 5 })
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()
let schedulerRefreshTimer = 0
let elapsedTimer = 0
const elapsedNow = ref(Date.now())
let realtimeRefreshTimer = 0
let realtimeUnsubscribe = () => {}
let autoSaveTimer = 0
let autoSaveVersion = 0
let autoSavePending = false
const pendingRealtimeResources = new Set()
let schedulerRefreshPending = false
let schedulerRefreshVersion = 0
const logViewport = ref(null)
const logViewportHeight = ref(251)
const logEmptyHeight = ref(215)
let logResizeTimer = 0
let logLayoutObserver

const accountOptions = computed(() => accounts.value.map((account) => ({ value: account.id, label: `${account.label || account.apple_id}（${account.apple_id}）` })))
const accountByID = computed(() => new Map(accounts.value.map((account) => [account.id, account])))
const schedulerEvents = computed(() => [...(scheduler.value.events || [])].reverse())
const displayedStatus = computed(() => {
  if (scheduler.value.running) return scheduler.value.status
  if (busy.value === 'create-one') return 'creating'
  return 'idle'
})
const selectedAccountCount = computed(() => {
  if (!scheduler.value.running && form.mode === 'once') return form.account_id ? 1 : 0
  const ids = scheduler.value.running ? scheduler.value.account_ids : form.account_ids
  return Array.isArray(ids) ? ids.length : 0
})
const intervalSummary = computed(() => {
  const minimum = scheduler.value.running
    ? Number(scheduler.value.interval_min_seconds || 0)
    : Number(form.interval_min_minutes || 0) * 60
  const maximum = scheduler.value.running
    ? Number(scheduler.value.interval_max_seconds || minimum)
    : Number(form.interval_max_minutes || 0) * 60
  if (!minimum) return '-'
  const formatDuration = (seconds) => {
    if (seconds % 3600 === 0) return `${seconds / 3600} 小时`
    if (seconds % 60 === 0) return `${seconds / 60} 分钟`
    return `${seconds} 秒`
  }
  return maximum > minimum ? `${formatDuration(minimum)}～${formatDuration(maximum)}` : formatDuration(minimum)
})
const nextRunSummary = computed(() => {
  if (!scheduler.value.running && form.mode === 'once') return '点击后立即创建'
  if (!scheduler.value.running) return '未安排'
  if (scheduler.value.next_run_at) return formatTime(scheduler.value.next_run_at)
  if (scheduler.value.status === 'creating') return '本轮执行中'
  return '准备执行'
})
const totalRunTime = computed(() => {
  const started = new Date(scheduler.value.started_at || '').getTime()
  if (!Number.isFinite(started) || new Date(started).getFullYear() <= 1) return '—'
  const stopped = new Date(scheduler.value.stopped_at || '').getTime()
  const ended = scheduler.value.running || !Number.isFinite(stopped) || new Date(stopped).getFullYear() <= 1
    ? elapsedNow.value
    : stopped
  const seconds = Math.max(0, Math.floor((ended - started) / 1000))
  const days = Math.floor(seconds / 86400)
  const hours = String(Math.floor((seconds % 86400) / 3600)).padStart(2, '0')
  const minutes = String(Math.floor((seconds % 3600) / 60)).padStart(2, '0')
  const remainder = String(seconds % 60).padStart(2, '0')
  return `${days ? `${days} 天 ` : ''}${hours}:${minutes}:${remainder}`
})

const createChannelOptions = [
  { value: 'auto', label: '自动接口：新接口优先，失败用旧接口', dot: 'bg-slate-400' },
  { value: 'apple_account', label: 'Apple Account 新接口', dot: 'bg-violet-500' },
  { value: 'icloud_web', label: 'iCloud Web 旧接口', dot: 'bg-sky-500' },
]
const modeOptions = [
  { value: 'once', label: '创建一个', description: '立即为一个账号创建邮箱', dot: 'bg-sky-500' },
  { value: 'scheduled', label: '自动创建', description: '按设定间隔持续创建', dot: 'bg-emerald-500' },
]

function statusText(status) {
  return ({ ready: '准备创建', running: '运行中', creating: '创建中', waiting: '等待下一轮', stopped: '已停止', idle: '未启动' })[status] || status
}

function statusClass(status) {
  if (status === 'running' || status === 'creating') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200'
  if (status === 'waiting' || status === 'ready') return 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-200'
  return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
}

function modeText(mode) {
  return mode === 'scheduled' ? '自动创建' : '创建一个'
}

function syncModeDefaults(mode) {
  if (mode === 'once' && !form.account_id) form.account_id = form.account_ids[0] || ''
  if (mode === 'scheduled' && !form.account_ids.length && form.account_id) form.account_ids = [form.account_id]
  form.create_channel = mode === 'scheduled'
    ? (defaultForm.scheduler_create_channel || 'auto')
    : (defaultForm.create_channel || 'auto')
}

function applyDefaults() {
  form.mode = defaultForm.mode === 'scheduled' ? 'scheduled' : 'once'
  const validAccountIDs = (defaultForm.account_ids || []).filter((id) => accounts.value.some((account) => account.id === id))
  const firstAccountID = validAccountIDs[0] || accounts.value[0]?.id || ''
  form.account_id = firstAccountID
  form.account_ids = validAccountIDs.length ? [...validAccountIDs] : (firstAccountID ? [firstAccountID] : [])
  form.label = defaultForm.label || ''
  form.note = defaultForm.note || ''
  form.create_channel = form.mode === 'scheduled'
    ? (defaultForm.scheduler_create_channel || 'auto')
    : (defaultForm.create_channel || 'auto')
  form.interval_min_minutes = defaultForm.scheduler_interval_min_minutes || 60
  form.interval_max_minutes = defaultForm.scheduler_interval_max_minutes || form.interval_min_minutes
  form.account_interval_min_seconds = defaultForm.scheduler_account_interval_min_seconds || 5
  form.account_interval_max_seconds = defaultForm.scheduler_account_interval_max_seconds || form.account_interval_min_seconds
}

function assignDefaultSettings(settings = {}) {
  const displaySettings = { ...settings }
  if (String(displaySettings.label || '').trim().toLowerCase() === 'x') displaySettings.label = ''
  Object.assign(defaultForm, {
    mode: 'once',
    label: '',
    note: '',
    account_ids: [],
    create_channel: 'auto',
    scheduler_create_channel: 'auto',
    apple_account_two_factor_method: 'trusted_device',
    icloud_web_two_factor_method: 'trusted_device',
    scheduler_interval_min_minutes: 60,
    scheduler_interval_max_minutes: 60,
    scheduler_account_interval_min_seconds: 5,
    scheduler_account_interval_max_seconds: 5,
  }, displaySettings, {
    scheduler_interval_min_minutes: Number(displaySettings.scheduler_interval_min_minutes || 60),
    scheduler_interval_max_minutes: Number(displaySettings.scheduler_interval_max_minutes || displaySettings.scheduler_interval_min_minutes || 60),
    scheduler_account_interval_min_seconds: Number(displaySettings.scheduler_account_interval_min_seconds || 5),
    scheduler_account_interval_max_seconds: Number(displaySettings.scheduler_account_interval_max_seconds || displaySettings.scheduler_account_interval_min_seconds || 5),
  })
}

function buildFormSettings() {
  const accountIDs = form.mode === 'scheduled'
    ? [...form.account_ids]
    : (form.account_id ? [form.account_id] : [])
  return {
    ...defaultForm,
    mode: form.mode,
    label: form.label,
    note: form.note,
    account_ids: accountIDs,
    ...(form.mode === 'scheduled'
      ? { scheduler_create_channel: form.create_channel }
      : { create_channel: form.create_channel }),
  }
}

async function saveFormSettings(version) {
  try {
    const data = await api('/api/create-settings', { method: 'PUT', body: JSON.stringify(buildFormSettings()) })
    if (version === autoSaveVersion && data.settings) assignDefaultSettings(data.settings)
  } catch (err) {
    if (version === autoSaveVersion) notify(`自动保存创建设置失败：${err.message}`, true)
  }
}

function scheduleFormAutoSave() {
  if (!initialized.value) return
  autoSaveVersion += 1
  const version = autoSaveVersion
  autoSavePending = true
  window.clearTimeout(autoSaveTimer)
  autoSaveTimer = window.setTimeout(() => {
    autoSavePending = false
    void saveFormSettings(version)
  }, 500)
}

function eventTypeText(type) {
  return ({ started: '启动', stopped: '停止', round_started: '新一轮', created: '已创建', failed: '失败', waiting: '等待' })[type] || type
}

function eventTone(type) {
  if (type === 'failed') return 'bg-rose-100 text-rose-600 dark:bg-rose-950/60 dark:text-rose-300'
  if (type === 'created') return 'bg-emerald-100 text-emerald-600 dark:bg-emerald-950/60 dark:text-emerald-300'
  return 'bg-sky-100 text-sky-600 dark:bg-sky-950/60 dark:text-sky-300'
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() <= 1) return '-'
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date)
}

function calculateLogViewportHeight() {
  const element = logViewport.value
  if (!element) return

  const scrollContainer = element.closest('.page-scroll')
  const scrollTop = scrollContainer?.scrollTop || 0
  const viewportTop = element.getBoundingClientRect().top + scrollTop
  const scrollBottom = scrollContainer?.getBoundingClientRect().bottom || window.innerHeight
  const scrollStyle = scrollContainer ? window.getComputedStyle(scrollContainer) : null
  const bottomPadding = Number.parseFloat(scrollStyle?.paddingBottom || '0') || 0
  const measuredHeaderHeight = element.querySelector('thead')?.getBoundingClientRect().height || 36
  const dataRow = element.querySelector('tbody tr:not(.task-log-empty-row)')
  const measuredRowHeight = dataRow?.getBoundingClientRect().height || 43
  const tableWidth = element.querySelector('table')?.scrollWidth || 0
  const hasHorizontalScrollbar = tableWidth > element.clientWidth + 1
  const reservedHeight = (hasHorizontalScrollbar ? 8 : 0) + 1
  const minimumRows = window.matchMedia('(max-width: 620px)').matches ? 3 : 5
  const availableHeight = scrollBottom - viewportTop - bottomPadding - 2
  const visibleRows = Math.max(minimumRows, Math.floor((availableHeight - measuredHeaderHeight - reservedHeight) / measuredRowHeight))

  logEmptyHeight.value = Math.floor(visibleRows * measuredRowHeight)
  logViewportHeight.value = Math.floor(measuredHeaderHeight + logEmptyHeight.value + reservedHeight)
}

function scheduleLogViewportHeight() {
  window.clearTimeout(logResizeTimer)
  logResizeTimer = window.setTimeout(calculateLogViewportHeight, 80)
}

function notify(text, isError = false) {
  if (isError) showError(text)
  else success(text)
}

async function copyEmail(value) {
  const email = String(value || '').trim()
  if (!email) return
  try {
    await navigator.clipboard.writeText(email)
    success('邮箱已复制')
  } catch {
    showError('邮箱复制失败，请手动复制')
  }
}

function eventAppleAccount(event) {
  const account = accountByID.value.get(event?.account_id)
  return account?.apple_id || account?.label || event?.account_id || '—'
}

async function load(options = {}) {
  const silent = Boolean(options.silent)
  if (!silent) loading.value = true
  try {
    const [schedulerData, accountData, createData] = await Promise.all([api('/api/scheduler/status'), api('/api/apple-accounts'), api('/api/create-settings')])
    scheduler.value = schedulerData.scheduler || scheduler.value
    accounts.value = accountData.items || []
    assignDefaultSettings(createData.settings)
    if (!defaultForm.account_ids.length && accounts.value.length) defaultForm.account_ids = [accounts.value[0].id]
    if (!initialized.value) {
      applyDefaults()
      initialized.value = true
    }
  } catch (err) {
    notify(err.message, true)
  } finally {
    if (!silent) loading.value = false
  }
}

function scheduleRealtimeRefresh(change) {
	if (change.resource === 'scheduler' && change.payload?.data) {
		scheduler.value = change.payload.data
		return
	}
  pendingRealtimeResources.add(change.resource)
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => {
    const resources = new Set(pendingRealtimeResources)
    pendingRealtimeResources.clear()
    if (resources.has('scheduler')) void refreshScheduler()
    if (resources.has('apple-account') || resources.has('create-settings')) void load({ silent: true })
  }, 120)
}

async function refreshScheduler() {
  if (schedulerRefreshPending || loading.value || busy.value) return
  schedulerRefreshPending = true
  const refreshVersion = schedulerRefreshVersion
  try {
    const data = await api('/api/scheduler/status')
    if (refreshVersion === schedulerRefreshVersion) scheduler.value = data.scheduler || scheduler.value
  } catch {
    // 后台刷新失败时保留当前数据，下一轮会自动重试。
  } finally {
    schedulerRefreshPending = false
  }
}

async function saveDefaults() {
  busy.value = 'save-defaults'
  autoSaveVersion += 1
  autoSavePending = false
  window.clearTimeout(autoSaveTimer)
  try {
    // 设置弹窗没有执行方式控件，保存时沿用当前页面选择，避免覆盖用户刚切换的模式。
    const data = await api('/api/create-settings', { method: 'PUT', body: JSON.stringify({ ...defaultForm, mode: form.mode }) })
    assignDefaultSettings(data.settings)
    applyDefaults()
    showDefaults.value = false
    notify('创建与调度默认设置已保存')
  } catch (err) {
    notify(err.message, true)
  } finally {
    busy.value = ''
  }
}

async function start() {
  busy.value = 'start'
  schedulerRefreshVersion++
  try {
    const data = await api('/api/scheduler/start', { method: 'POST', body: JSON.stringify({ account_ids: form.account_ids, label: form.label, note: form.note, create_channel: form.create_channel, interval_min_minutes: form.interval_min_minutes, interval_max_minutes: form.interval_max_minutes, account_interval_min_seconds: form.account_interval_min_seconds, account_interval_max_seconds: form.account_interval_max_seconds }) })
    scheduler.value = data.scheduler || data.data?.scheduler || scheduler.value
    notify('定时创建已启动')
    await load()
  } catch (err) {
    notify(err.message, true)
  } finally {
    busy.value = ''
  }
}

async function createOne() {
  if (!form.account_id) return
  busy.value = 'create-one'
  schedulerRefreshVersion++
  try {
    const result = await api(`/api/apple-accounts/${form.account_id}/mailboxes`, { method: 'POST', body: JSON.stringify({ label: form.label, note: form.note, channel: form.create_channel }) })
    scheduler.value = result.scheduler || scheduler.value
    form.label = defaultForm.label || ''
    form.note = defaultForm.note || ''
    notify(`已创建隐私邮箱：${result.mailbox.email}`)
  } catch (err) {
    notify(err.message, true)
    try {
      const status = await api('/api/scheduler/status')
      scheduler.value = status.scheduler || scheduler.value
    } catch {
      // 保留原始创建错误提示，状态刷新失败时不重复提示。
    }
  } finally {
    busy.value = ''
  }
}

async function stop() {
  busy.value = 'stop'
  schedulerRefreshVersion++
  try {
    const data = await api('/api/scheduler/stop', { method: 'POST', body: '{}' })
    scheduler.value = data.scheduler || data.data?.scheduler || scheduler.value
    notify('定时创建已停止')
    await load()
  } catch (err) {
    notify(err.message, true)
  } finally {
    busy.value = ''
  }
}

async function clearLogs() {
  const confirmed = await confirmAction({
    title: '清除调度日志',
    message: '确定清除当前调度任务的所有运行日志吗？',
    confirmText: '确认清除',
    tone: 'danger',
  })
  if (!confirmed) return
  busy.value = 'clear'
  schedulerRefreshVersion++
  try {
    await api('/api/scheduler/logs/clear', { method: 'POST', body: '{}' })
    scheduler.value.events = []
    notify('调度日志已清除')
  } catch (err) {
    notify(err.message, true)
  } finally {
    busy.value = ''
  }
}

onMounted(() => {
  load()
  realtimeUnsubscribe = subscribeRealtime(['scheduler', 'apple-account', 'create-settings'], scheduleRealtimeRefresh)
  schedulerRefreshTimer = window.setInterval(refreshScheduler, 30000)
  elapsedTimer = window.setInterval(() => { elapsedNow.value = Date.now() }, 1000)
  logLayoutObserver = new ResizeObserver(scheduleLogViewportHeight)
  logLayoutObserver.observe(document.querySelector('.page-scroll'))
  window.addEventListener('resize', scheduleLogViewportHeight)
})

watch(
  () => [form.mode, form.account_id, form.account_ids.join(','), form.create_channel, form.label, form.note],
  scheduleFormAutoSave,
)

watch([loading, () => scheduler.value.last_error], async () => {
  await nextTick()
  scheduleLogViewportHeight()
})

onBeforeUnmount(() => {
  schedulerRefreshVersion++
  window.clearInterval(schedulerRefreshTimer)
  window.clearInterval(elapsedTimer)
  window.clearTimeout(realtimeRefreshTimer)
  if (autoSavePending) {
    window.clearTimeout(autoSaveTimer)
    autoSavePending = false
    void saveFormSettings(autoSaveVersion)
  }
  pendingRealtimeResources.clear()
  realtimeUnsubscribe()
  window.clearTimeout(logResizeTimer)
  window.removeEventListener('resize', scheduleLogViewportHeight)
  logLayoutObserver?.disconnect()
})
</script>

<template>
  <div class="task-page">
    <div v-if="loading" class="task-loading"><LoaderCircle :size="22" class="animate-spin" /><span>正在加载创建任务</span></div>
    <template v-else>
      <section class="panel task-workbench">
        <div class="task-command-bar">
          <div class="task-command-controls">
            <div class="task-control task-mode-control">
              <span class="task-control-label">执行方式</span>
              <CardSelect v-model="form.mode" :options="modeOptions" aria-label="执行方式" compact @change="syncModeDefaults" />
            </div>
            <div class="task-control task-account-control">
              <span class="task-control-label">参与 Apple 账号</span>
              <CardSelect v-if="form.mode === 'once'" v-model="form.account_id" :options="accountOptions" placeholder="请选择一个账号" aria-label="参与 Apple 账号" compact />
              <CardSelect v-else v-model="form.account_ids" :options="accountOptions" placeholder="请选择参与账号" aria-label="参与 Apple 账号" compact multiple />
            </div>
            <div class="task-control task-channel-control">
              <span class="task-control-label">创建通道</span>
              <CardSelect v-model="form.create_channel" :options="createChannelOptions" aria-label="创建通道" compact />
            </div>
            <label class="task-control task-label-control"><span class="task-control-label">标签前缀</span><input v-model.trim="form.label" class="field task-command-input" placeholder="可选，默认 x" /></label>
            <label class="task-control task-note-control"><span class="task-control-label">备注</span><input v-model.trim="form.note" class="field task-command-input" placeholder="可选备注" /></label>
          </div>
          <div class="task-command-actions">
            <span :class="statusClass(displayedStatus)" class="task-status-badge">{{ statusText(displayedStatus) }}</span>
            <button type="button" class="icon-button task-settings-button" title="创建与调度设置" aria-label="打开创建与调度设置" @click="showDefaults = true"><Settings :size="16" /></button>
            <button v-if="scheduler.running" class="secondary-button task-run-button task-stop-button" :disabled="busy === 'stop'" @click="stop"><LoaderCircle v-if="busy === 'stop'" :size="15" class="animate-spin" /><Square v-else :size="15" /><span>停止任务</span></button>
            <button v-else-if="form.mode === 'once'" class="primary-button task-run-button" :disabled="busy === 'create-one' || !form.account_id" @click="createOne"><LoaderCircle v-if="busy === 'create-one'" :size="15" class="animate-spin" /><Play v-else :size="15" /><span>创建一个</span></button>
            <button v-else class="primary-button task-run-button" :disabled="busy === 'start' || !form.account_ids.length" @click="start"><LoaderCircle v-if="busy === 'start'" :size="15" class="animate-spin" /><Play v-else :size="15" /><span>启动任务</span></button>
          </div>
        </div>

        <div class="task-summary" aria-label="任务概览">
          <div class="task-summary-title"><span class="task-summary-icon"><Clock3 :size="16" /></span><span><strong>任务概览</strong><small>实时调度状态</small></span></div>
          <dl><dt>参与账号</dt><dd>{{ selectedAccountCount }}</dd></dl>
          <dl><dt>执行方式</dt><dd>{{ scheduler.running ? '自动创建' : modeText(form.mode) }}</dd></dl>
          <dl><dt>创建成功</dt><dd class="task-success-value">{{ scheduler.success || 0 }}</dd></dl>
          <dl><dt>创建失败</dt><dd class="task-failed-value">{{ scheduler.failed || 0 }}</dd></dl>
          <dl><dt>轮次间隔</dt><dd>{{ scheduler.running || form.mode === 'scheduled' ? intervalSummary : '—' }}</dd></dl>
          <dl class="task-summary-wide"><dt>下次执行</dt><dd>{{ nextRunSummary }}</dd></dl>
          <dl class="task-summary-wide"><dt>最近执行</dt><dd>{{ formatTime(scheduler.last_run_at) }}</dd></dl>
          <dl class="task-summary-wide"><dt>总运行时间</dt><dd>{{ totalRunTime }}</dd></dl>
        </div>

        <div v-if="scheduler.last_error" class="task-notice-row" aria-live="polite">
          <div class="task-notice task-notice-error"><CircleAlert :size="15" /><span>最近错误</span><strong>{{ scheduler.last_error }}</strong></div>
        </div>

        <div class="task-log-heading">
          <div><h2>调度日志</h2><p>记录启动、轮次、创建结果和等待状态</p></div>
          <div class="task-log-actions"><span>{{ schedulerEvents.length }} 条记录</span><button class="icon-button task-clear-button" title="清除调度日志" aria-label="清除调度日志" :disabled="busy === 'clear' || !schedulerEvents.length" @click="clearLogs"><LoaderCircle v-if="busy === 'clear'" :size="15" class="animate-spin" /><Trash2 v-else :size="15" /></button></div>
        </div>
        <div ref="logViewport" class="task-log-viewport" :style="{ '--task-log-height': `${logViewportHeight}px`, '--task-log-empty-height': `${logEmptyHeight}px` }">
          <table class="task-log-table" :class="{ 'task-log-table-empty': !schedulerEvents.length }">
            <colgroup><col class="task-log-column" /><col class="task-log-column" /><col class="task-log-column" /><col class="task-log-column" /><col class="task-log-column" /><col class="task-log-column" /></colgroup>
            <thead><tr><th>Apple 账号</th><th>事件</th><th>邮箱</th><th>标签</th><th>详情</th><th>时间</th></tr></thead>
            <tbody>
              <tr v-for="event in schedulerEvents" :key="event.id">
                <td><span class="task-event-account" :title="eventAppleAccount(event)">{{ eventAppleAccount(event) }}</span></td>
                <td><span :class="eventTone(event.type)" class="task-event-badge"><CircleAlert v-if="event.type === 'failed'" :size="13" /><CheckCircle2 v-else :size="13" />{{ eventTypeText(event.type) }}</span></td>
                <td><button v-if="event.email" type="button" class="task-event-email" title="点击复制邮箱" :aria-label="`复制邮箱 ${event.email}`" @click="copyEmail(event.email)">{{ event.email }}</button><span v-else class="task-event-email-empty">—</span></td>
                <td><span v-if="event.label" class="task-event-label" :title="event.label">{{ event.label }}</span><span v-else class="task-event-label-empty">—</span></td>
                <td><span class="task-event-message" :title="event.message">{{ event.message }}</span></td>
                <td><time>{{ formatTime(event.at) }}</time></td>
              </tr>
              <tr v-if="!schedulerEvents.length" class="task-log-empty-row"><td colspan="6" class="task-log-empty"><span class="task-empty-icon"><Clock3 :size="20" /></span><strong>暂无调度记录</strong><small>启动创建任务后，运行过程会显示在这里。</small></td></tr>
            </tbody>
          </table>
        </div>
      </section>

      <Teleport to="body">
        <div v-if="showDefaults" class="task-dialog-backdrop" role="presentation" @click.self="showDefaults = false">
          <form class="panel task-settings-dialog" role="dialog" aria-modal="true" aria-labelledby="create-defaults-title" @submit.prevent="saveDefaults">
            <div class="task-dialog-heading">
              <div><h2 id="create-defaults-title"><Settings :size="18" />创建与调度设置</h2><p>设置单次创建和自动创建的默认参数。</p></div>
              <button type="button" class="icon-button" title="关闭设置" aria-label="关闭创建与调度设置" :disabled="busy === 'save-defaults'" @click="showDefaults = false"><X :size="16" /></button>
            </div>

            <div class="task-settings-grid">
              <label class="form-group"><span class="form-label">默认标签前缀</span><input v-model.trim="defaultForm.label" class="field" placeholder="可选" /><span class="form-help">留空默认使用 x，并从现有最大编号继续创建。</span></label>
              <label class="form-group"><span class="form-label">默认备注</span><input v-model.trim="defaultForm.note" class="field" placeholder="可选" /></label>
              <div class="form-group"><span class="form-label">创建一个通道</span><CardSelect v-model="defaultForm.create_channel" :options="createChannelOptions" aria-label="创建一个通道" /></div>
              <div class="form-group"><span class="form-label">自动创建通道</span><CardSelect v-model="defaultForm.scheduler_create_channel" :options="createChannelOptions" aria-label="自动创建通道" /></div>
              <div class="form-group"><span class="form-label">下一轮间隔（随机分钟范围）</span><div class="task-interval-fields"><input v-model.number="defaultForm.scheduler_interval_min_minutes" class="field" type="text" inputmode="numeric" pattern="[0-9]*" placeholder="最小" autocomplete="off" aria-label="下一轮最小间隔分钟" /><span class="task-interval-separator">到</span><input v-model.number="defaultForm.scheduler_interval_max_minutes" class="field" type="text" inputmode="numeric" pattern="[0-9]*" placeholder="最大" autocomplete="off" aria-label="下一轮最大间隔分钟" /></div></div>
              <div class="form-group"><span class="form-label">账号间隔（随机秒数范围）</span><div class="task-interval-fields"><input v-model.number="defaultForm.scheduler_account_interval_min_seconds" class="field" type="text" inputmode="numeric" pattern="[0-9]*" placeholder="最小" autocomplete="off" aria-label="账号最小间隔秒数" /><span class="task-interval-separator">到</span><input v-model.number="defaultForm.scheduler_account_interval_max_seconds" class="field" type="text" inputmode="numeric" pattern="[0-9]*" placeholder="最大" autocomplete="off" aria-label="账号最大间隔秒数" /></div></div>
            </div>
            <div class="task-default-accounts form-group"><span class="form-label">默认参与账号</span><CardSelect v-model="defaultForm.account_ids" :options="accountOptions" placeholder="请选择默认参与账号" aria-label="默认参与账号" multiple /><span class="form-help">可以保存多个默认账号；进入页面时先选择第一个，也可以在自动创建中继续多选。</span></div>

            <div class="task-dialog-actions">
              <button type="button" class="secondary-button" :disabled="busy === 'save-defaults'" @click="showDefaults = false">取消</button>
              <button type="submit" class="primary-button" :disabled="busy === 'save-defaults'"><LoaderCircle v-if="busy === 'save-defaults'" :size="16" class="animate-spin" /><Save v-else :size="16" />保存设置</button>
            </div>
          </form>
        </div>
      </Teleport>
    </template>
  </div>
</template>
