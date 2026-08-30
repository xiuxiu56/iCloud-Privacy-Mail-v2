<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Boxes, ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight, Clipboard, CloudOff, KeyRound, LoaderCircle, MailOpen, RefreshCw, Save, Search, ShieldCheck, Sparkles, Trash2, X } from '@lucide/vue'
import CardSelect from '../components/CardSelect.vue'
import FormDialog from '../components/FormDialog.vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'
import { emailDomain, loadDomainMailPreview, normalizeDomainMailPreview, normalizeDomainRoutes } from '../domainMailPreview'

const config = reactive(loadDomainMailPreview())
const loading = ref(true)
const search = ref('')
const domainFilter = ref('')
const statusFilter = ref('')
const page = ref(1)
const pageSize = ref(7)
const previewMailboxes = ref([])
const selectedMailboxes = ref([])
const selected = ref(null)
const selectedMessage = ref(null)
const selectedMessageLoading = ref(false)
const messageViewMode = ref('auto')
const messages = ref([])
const codeDialogOpen = ref(false)
const codeMailbox = ref(null)
const codeResult = ref(null)
const codeError = ref('')
const codeBusyVisible = ref('')
const rowBusyActions = ref({})
const busyActions = ref([])
const deletingMailboxIDs = ref([])
const deleteConfirmID = ref('')
const showBulkDelete = ref(false)
const showGenerate = ref(false)
const quickEditOpen = ref(false)
const quickEditField = ref('note')
const quickEditMailbox = ref(null)
const bulkDeleteEmails = ref('')
const bulkDeleteError = ref('')
const generateForm = reactive({ prefix: '', domain: '', label: '', mode: 'sequence', count: 1, start: '', random_length: 12 })
const quickEdit = reactive({ status: 'available', note: '' })
const detailEdit = reactive({ status: 'available', note: '', api_active: true, icloud_active: true })
const generatedRandomTokens = ref([])
const mailboxCommandBar = ref(null)
const mailboxRouteStrip = ref(null)
const mailboxTableViewport = ref(null)
const mailboxPagination = ref(null)
const mailboxTableHeight = ref(372)
const mailboxEmptyHeight = ref(336)
const { success, info, error: showError, update: updateToast, dismiss: dismissToast } = useToast()
const { confirm: confirmAction } = useConfirm()
let tableResizeTimer
let mailboxLayoutObserver
let codeBusyTimer
let messageLoadRequestID = 0
let realtimeRefreshTimer
let autoRefreshTimer
let realtimeUnsubscribe = () => {}
let domainSyncQueue = []
let domainSyncBatch = null
let domainSyncBatchSequence = 0
let domainSyncIntroTimer
let domainSyncNoticeID = null
const activeDomainSyncKeys = new Set()
const maxConcurrentDomainSyncGroups = 3
let domainDeleteQueue = []
let domainDeleteBatch = null
let domainDeleteBatchSequence = 0
let domainDeleteIntroTimer
let domainDeleteNoticeID = null
let activeDomainDeleteJobs = 0
const activeDomainDeleteKeys = new Set()
const maxConcurrentDomainDeleteJobs = 3

const configuredRoutes = computed(() => normalizeDomainRoutes(config.routes, config.domains, config))
const configuredDomains = computed(() => configuredRoutes.value.map((item) => item.domain))
const receiverReady = computed(() => configuredRoutes.value.every((item) => item.receiver_type === 'custom_imap'
  ? Boolean(String(item.imap_host || '').trim() && String(item.imap_username || '').trim())
  : Boolean(String(item.account_id || '').trim())))
const routeReady = computed(() => Boolean(config.enabled && configuredRoutes.value.length && configuredRoutes.value.every((item) => String(item.forward_to_email || '').trim()) && receiverReady.value))
const domainFilterOptions = computed(() => [
  { value: '', label: '全部接收域名', description: '显示所有自动发现的域名邮箱', dot: 'bg-slate-400' },
  ...configuredDomains.value.map((domain) => ({ value: domain, label: `@${domain}`, description: '只显示该域名下的邮箱', dot: 'bg-sky-500' })),
])
const statusFilterOptions = [
  { value: '', label: '全部状态', dot: 'bg-slate-400' },
  { value: 'available', label: '可用', dot: 'bg-emerald-500' },
  { value: 'reserved', label: '已预留', dot: 'bg-violet-500' },
  { value: 'used', label: '已使用', dot: 'bg-amber-500' },
  { value: 'failed', label: '失败', dot: 'bg-rose-500' },
  { value: 'disabled', label: '已停用', dot: 'bg-slate-500' },
]
const mailboxDetailStatusOptions = statusFilterOptions.filter((item) => item.value)
const generateDomainOptions = computed(() => configuredDomains.value.map((domain) => ({ value: domain, label: `@${domain}`, description: '生成该域名下的登记地址', dot: 'bg-sky-500' })))
const generateModeOptions = [
  { value: 'sequence', label: '指定前缀 / 递增编号', description: '编号留空创建单个指定前缀', dot: 'bg-sky-500' },
  { value: 'random', label: '随机字符', description: '固定前缀后追加随机字符', dot: 'bg-violet-500' },
]
const singlePrefixMode = computed(() => generateForm.mode === 'sequence' && String(generateForm.start ?? '').trim() === '')
const generatedEmails = computed(() => {
  const prefix = normalizePrefix(generateForm.prefix)
  if (!generateForm.domain || (generateForm.mode === 'sequence' && !prefix)) return []
  if (singlePrefixMode.value) return [`${prefix}@${generateForm.domain}`]
  const count = Math.min(500, Math.max(0, Number(generateForm.count) || 0))
  if (!count) return []
  if (generateForm.mode === 'random') {
    return Array.from({ length: count }, (_, index) => `${prefix}${generatedRandomTokens.value[index] || ''}@${generateForm.domain}`)
  }
  const start = Number(generateForm.start)
  if (!Number.isInteger(start) || start < 0) return []
  return Array.from({ length: count }, (_, index) => `${prefix}${start + index}@${generateForm.domain}`)
})
const receiverText = computed(() => {
  const labels = [...new Set(configuredRoutes.value.map(receiverChannelLabel))]
  if (!labels.length) return '尚未配置收件通道'
  return labels.length === 1 ? labels[0] : `${labels.length} 个收件通道`
})
const routeStatusText = computed(() => {
  if (!config.enabled) return '接收未开启'
  if (!routeReady.value) return '等待完整配置'
  return '收件准备就绪'
})
const allMailboxes = computed(() => previewMailboxes.value
  .filter((item) => item.kind !== 'receiver_mailbox' && !item.preview)
  .map((item) => ({
    ...item,
    id: item.id || `domain:${item.email}`,
    domain: emailDomain(item.email),
    status: item.status || 'available',
    api_active: item.api_active !== false,
    receive_count: Number(item.receive_count || item.messages?.length || 0),
    messages: Array.isArray(item.messages) ? item.messages : [],
  }))
  .sort(compareMailboxCreatedAt))
const filteredMailboxes = computed(() => {
  const keyword = search.value.trim().toLowerCase()
  return allMailboxes.value.filter((item) => {
    if (domainFilter.value && item.domain !== domainFilter.value) return false
    if (statusFilter.value && item.status !== statusFilter.value) return false
    if (!keyword) return true
    return `${item.email} ${item.label || ''} ${item.note || ''} ${latestSubject(item)}`.toLowerCase().includes(keyword)
  })
})
const totalPages = computed(() => Math.max(1, Math.ceil(filteredMailboxes.value.length / pageSize.value)))
const pagedMailboxes = computed(() => filteredMailboxes.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
const totalMessages = computed(() => allMailboxes.value.reduce((total, item) => total + item.receive_count, 0))
const selectedMailboxIDs = computed(() => selectedMailboxes.value.map((item) => item.id))
const selectedDeletableCount = computed(() => selectedMailboxes.value.filter((item) => !isDomainDeleteBusy(item.id)).length)
const selectablePageMailboxes = computed(() => pagedMailboxes.value.filter((item) => !isDomainDeleteBusy(item.id)))
const selectedPageCount = computed(() => selectablePageMailboxes.value.filter((item) => selectedMailboxIDs.value.includes(item.id)).length)
const allPageMailboxesSelected = computed(() => Boolean(selectablePageMailboxes.value.length) && selectedPageCount.value === selectablePageMailboxes.value.length)
const somePageMailboxesSelected = computed(() => selectedPageCount.value > 0 && !allPageMailboxesSelected.value)
const codeDialogBusy = computed(() => Boolean(codeMailbox.value?.id && rowBusyAction(codeMailbox.value.id) === 'code'))
const bulkDeleteEmailCount = computed(() => parseBulkDeleteEmails(bulkDeleteEmails.value).length)
const quickEditTitle = computed(() => quickEditField.value === 'note' ? '修改备注' : '修改邮箱状态')
const selectedMessageHasHTML = computed(() => Boolean(
  String(selectedMessage.value?.html_body || '').trim()
  || (String(selectedMessage.value?.content_type || '').toLowerCase().includes('text/html') && looksLikeHTML(selectedMessage.value?.body)),
))
const selectedMessageHTML = computed(() => {
  if (!selectedMessage.value) return ''
  return String(selectedMessage.value.html_body || (looksLikeHTML(selectedMessage.value.body) ? selectedMessage.value.body : '')).trim()
})
const selectedMessageHTMLDocument = computed(() => buildEmailHTMLDocument(selectedMessageHTML.value))
const showSelectedMessageHTML = computed(() => selectedMessageHasHTML.value && messageViewMode.value !== 'text')

watch([search, domainFilter, statusFilter], () => {
  page.value = 1
  nextTick(scheduleMailboxTableSize)
})
watch([() => generateForm.mode, () => generateForm.count, () => generateForm.random_length], () => {
  if (generateForm.mode === 'random') regenerateRandomEmails()
})
watch(totalPages, (value) => { if (page.value > value) page.value = value })
watch(allMailboxes, (items) => {
  const validIDs = new Set(items.map((item) => item.id))
  selectedMailboxes.value = selectedMailboxes.value.filter((item) => validIDs.has(item.id))
}, { deep: true })
watch([selected, codeDialogOpen, selectedMessage, showBulkDelete, showGenerate, quickEditOpen], ([mailbox, codeOpen, message, bulkDeleteOpen, generateOpen, editOpen]) => {
  document.body.style.overflow = mailbox || codeOpen || message || bulkDeleteOpen || generateOpen || editOpen ? 'hidden' : ''
})

async function loadDomainData(options = {}) {
	if (!options.silent) loading.value = true
	try {
		const [configData, mailboxData] = await Promise.all([
			api('/api/domain-mail/settings'),
			api('/api/domain-mailboxes'),
		])
		const routes = Array.isArray(configData.routes) ? configData.routes : []
		Object.assign(config, normalizeDomainMailPreview({
			...(configData.settings || {}),
			domains: routes.map((item) => item.domain),
			routes,
		}))
		previewMailboxes.value = Array.isArray(mailboxData.items) ? mailboxData.items : []
	} catch (err) {
		showError(err.message)
	} finally {
		if (!options.silent) loading.value = false
	}
}

async function refreshDomainMailboxPage() {
  if (document.hidden) return
  await loadDomainData({ silent: true })
  const mailboxID = selected.value?.id
  if (!mailboxID || quickEditOpen.value || isDomainDeleteBusy(mailboxID)) return
  try {
	const [detail, messageData] = await Promise.all([
	  api(`/api/mailboxes/${encodeURIComponent(mailboxID)}`),
	  api(`/api/mailboxes/${encodeURIComponent(mailboxID)}/messages`),
	])
	if (selected.value?.id !== mailboxID) return
	selected.value = { ...selected.value, ...(detail.mailbox || {}) }
	messages.value = messageData.items || []
  } catch {
	return
  }
}

function scheduleRealtimeDomainRefresh() {
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => { void refreshDomainMailboxPage() }, 100)
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
  busyActions.value = busyActions.value.filter((item) => item !== action)
}

function rowBusyAction(mailboxID) {
  return rowBusyActions.value[mailboxID] || ''
}

function startRowBusy(mailboxID, action) {
  if (!mailboxID || rowBusyAction(mailboxID)) return false
  rowBusyActions.value = { ...rowBusyActions.value, [mailboxID]: action }
  return true
}

function finishRowBusy(mailboxID, action) {
  if (rowBusyAction(mailboxID) !== action) return
  const next = { ...rowBusyActions.value }
  delete next[mailboxID]
  rowBusyActions.value = next
}

function openQuickEdit(mailbox, field) {
  quickEditMailbox.value = mailbox
  quickEditField.value = field
  quickEdit.status = mailbox.status || 'available'
  quickEdit.note = mailbox.note || ''
  quickEditOpen.value = true
}

function closeQuickEdit() {
  if (isBusy('quick-edit')) return
  quickEditOpen.value = false
  quickEditMailbox.value = null
}

async function saveQuickEdit() {
  if (!quickEditMailbox.value || !startBusy('quick-edit')) return
  const field = quickEditField.value
  const payload = field === 'note'
    ? { note: String(quickEdit.note || '').trim() }
    : { status: quickEdit.status || 'available' }
  const mailboxID = quickEditMailbox.value.id
  try {
	const data = await api(`/api/mailboxes/${encodeURIComponent(mailboxID)}/status`, { method: 'POST', body: JSON.stringify(payload) })
	previewMailboxes.value = previewMailboxes.value.map((item) => item.id === mailboxID ? { ...item, ...(data.mailbox || payload) } : item)
	if (selected.value?.id === mailboxID) selected.value = { ...selected.value, ...(data.mailbox || payload) }
	quickEditOpen.value = false
	quickEditMailbox.value = null
	success(field === 'note' ? '邮箱备注已保存' : '邮箱状态已保存')
  } catch (err) {
	showError(err.message)
  } finally {
	finishBusy('quick-edit')
  }
}

async function saveDetailEdit() {
  if (!selected.value || !startBusy('detail-save')) return
  const mailboxID = selected.value.id
  try {
	const data = await api(`/api/mailboxes/${encodeURIComponent(mailboxID)}/status`, {
	  method: 'POST',
	  body: JSON.stringify({
		status: detailEdit.status,
		note: String(detailEdit.note || '').trim(),
		api_active: Boolean(detailEdit.api_active),
		icloud_active: Boolean(detailEdit.icloud_active),
	  }),
	})
	const updated = data.mailbox || {}
	selected.value = { ...selected.value, ...updated }
	previewMailboxes.value = previewMailboxes.value.map((item) => item.id === mailboxID ? { ...item, ...updated } : item)
	success('域名邮箱设置已保存')
  } catch (err) {
	showError(err.message)
  } finally {
	finishBusy('detail-save')
  }
}

function isMailboxSelected(mailboxID) {
  return selectedMailboxIDs.value.includes(mailboxID)
}

function toggleMailboxSelection(mailbox) {
  if (isDomainDeleteBusy(mailbox.id)) return
  selectedMailboxes.value = isMailboxSelected(mailbox.id)
    ? selectedMailboxes.value.filter((item) => item.id !== mailbox.id)
    : [...selectedMailboxes.value, { id: mailbox.id, email: mailbox.email }]
}

function toggleAllMailboxSelection() {
  if (!selectablePageMailboxes.value.length) return
  const pageIDs = new Set(selectablePageMailboxes.value.map((item) => item.id))
  if (allPageMailboxesSelected.value) {
    selectedMailboxes.value = selectedMailboxes.value.filter((item) => !pageIDs.has(item.id))
    return
  }
  const selectedIDs = new Set(selectedMailboxIDs.value)
  selectedMailboxes.value = [
    ...selectedMailboxes.value,
    ...selectablePageMailboxes.value.filter((item) => !selectedIDs.has(item.id)).map((item) => ({ id: item.id, email: item.email })),
  ]
}

function domainSyncGroupKey(mailbox) {
  const route = configuredRoutes.value.find((item) => item.domain === mailbox.domain)
  if (route?.receiver_type === 'custom_imap') return `imap:${route.id || route.domain}`
  return `apple:${route?.account_id || mailbox.account_id || route?.forward_to_email || mailbox.domain}`
}

function domainSyncSummary(batch) {
  return `IMAP ${batch.imap}｜Web API ${batch.web}｜回退 ${batch.fallbacks}｜扫描 ${batch.scanned}｜匹配 ${batch.matched}｜新增 ${batch.synced}`
}

function domainSyncErrorText(err) {
  const text = String(err?.message || '同步失败').replace(/\s+/g, ' ').trim()
  if (/HTTP 400/i.test(text) && /Validation failed/i.test(text)) return 'iCloud Web 补查请求参数被拒绝（HTTP 400）'
  return text.length > 180 ? `${text.slice(0, 180)}……` : text
}

function renderDomainSyncNotice(batch = domainSyncBatch) {
  if (!batch || batch !== domainSyncBatch || batch.introVisible) return
  const completed = batch.success + batch.failed
  if (batch.running || batch.queued) {
    domainSyncNoticeID = updateToast(
      domainSyncNoticeID,
      `域名邮件同步：执行中 ${batch.running}｜排队中 ${batch.queued}｜已完成 ${completed}/${batch.total}（成功 ${batch.success}，失败 ${batch.failed}）｜${domainSyncSummary(batch)}`,
      'info',
      0,
    )
    return
  }
  const type = batch.failed ? (batch.success ? 'warning' : 'error') : 'success'
  const errorText = batch.lastError ? `；最近错误：${batch.lastError}` : ''
  domainSyncNoticeID = updateToast(
    domainSyncNoticeID,
    `域名邮件同步完成：任务成功 ${batch.success}/${batch.total}｜账号 ${batch.accounts}｜${domainSyncSummary(batch)}｜失败 ${batch.failed}${errorText}`,
    type,
    batch.failed ? 9000 : 7000,
  )
  domainSyncBatch = null
}

function beginDomainSyncBatch() {
  if (domainSyncBatch) return domainSyncBatch
  const batch = {
    id: ++domainSyncBatchSequence,
    total: 0,
    queued: 0,
    running: 0,
    success: 0,
    failed: 0,
    accounts: 0,
    imap: 0,
    web: 0,
    fallbacks: 0,
    scanned: 0,
    matched: 0,
    synced: 0,
    lastError: '',
    introID: null,
    introVisible: true,
  }
  domainSyncBatch = batch
  if (domainSyncNoticeID !== null) dismissToast(domainSyncNoticeID)
  domainSyncNoticeID = null
  batch.introID = updateToast(null, '正在同步域名邮箱邮件：IMAP 与 Web API 按设置并发读取并自动合并……', 'info', 1350)
  window.clearTimeout(domainSyncIntroTimer)
  domainSyncIntroTimer = window.setTimeout(() => {
    if (domainSyncBatch !== batch) return
    batch.introVisible = false
    batch.introID = null
    renderDomainSyncNotice(batch)
  }, 1400)
  return batch
}

function mergeDomainSyncResult(batch, data) {
  batch.accounts += Array.isArray(data?.accounts) ? data.accounts.length : 0
  batch.imap += Number(data?.imap_accounts || 0)
  batch.web += Number(data?.web_api_accounts || 0)
  batch.fallbacks += Number(data?.fallbacks || 0)
  batch.scanned += Number(data?.scanned || 0)
  batch.matched += Number(data?.matched || 0)
  batch.synced += Number(data?.synced_messages || data?.synced || 0)
}

function pumpDomainSyncQueue() {
  while (activeDomainSyncKeys.size < maxConcurrentDomainSyncGroups) {
    const index = domainSyncQueue.findIndex((job) => !activeDomainSyncKeys.has(job.key)
      && !activeDomainDeleteKeys.has(job.key)
      && !activeDomainDeleteKeys.has('domain-delete:global'))
    if (index < 0) break
    const [job] = domainSyncQueue.splice(index, 1)
    const batch = job.batch
    batch.queued = Math.max(0, batch.queued - 1)
    batch.running += 1
    activeDomainSyncKeys.add(job.key)
    Promise.resolve()
      .then(job.run)
      .then((data) => {
        batch.success += 1
        mergeDomainSyncResult(batch, data)
        job.resolve(data)
      })
      .catch((err) => {
        batch.failed += 1
        batch.lastError = domainSyncErrorText(err)
        job.reject(err)
      })
      .finally(() => {
        batch.running = Math.max(0, batch.running - 1)
        activeDomainSyncKeys.delete(job.key)
        pumpDomainSyncQueue()
        pumpDomainDeleteQueue()
        renderDomainSyncNotice(batch)
      })
  }
  renderDomainSyncNotice()
}

function enqueueDomainSync(key, run) {
  const batch = beginDomainSyncBatch()
  batch.total += 1
  batch.queued += 1
  const promise = new Promise((resolve, reject) => {
    domainSyncQueue.push({ key, run, resolve, reject, batch })
  })
  pumpDomainSyncQueue()
  return promise
}

function domainDeleteSummary(batch) {
  const cloud = batch.destroyed || batch.moved
  return `云端邮件 ${cloud}｜本地清理 ${batch.localRemoved}`
}

function domainDeleteErrorText(err) {
  const text = String(err?.message || '删除失败').replace(/\s+/g, ' ').trim()
  return text.length > 180 ? `${text.slice(0, 180)}……` : text
}

function renderDomainDeleteNotice(batch = domainDeleteBatch) {
  if (!batch || batch !== domainDeleteBatch || batch.introVisible) return
  const completed = batch.success + batch.failed
  if (batch.running || batch.queued) {
    domainDeleteNoticeID = updateToast(
      domainDeleteNoticeID,
      `域名邮箱删除：执行中 ${batch.running}｜排队中 ${batch.queued}｜已完成 ${completed}/${batch.total}`,
      'info',
      0,
    )
    return
  }
  const type = batch.failed ? (batch.success ? 'warning' : 'error') : 'success'
  const errorText = batch.lastError ? `；最近错误：${batch.lastError}` : ''
  domainDeleteNoticeID = updateToast(
    domainDeleteNoticeID,
    `域名邮箱删除完成：成功 ${batch.success}｜失败 ${batch.failed}｜${domainDeleteSummary(batch)}${errorText}`,
    type,
    batch.failed ? 9000 : 7000,
  )
  domainDeleteBatch = null
}

function beginDomainDeleteBatch() {
  if (domainDeleteBatch) return domainDeleteBatch
  const batch = {
    id: ++domainDeleteBatchSequence,
    total: 0,
    queued: 0,
    running: 0,
    success: 0,
    failed: 0,
    accounts: 0,
    scanned: 0,
    found: 0,
    moved: 0,
    destroyed: 0,
    localRemoved: 0,
    verified: 0,
    fallbacks: 0,
    lastError: '',
    introID: null,
    introVisible: true,
  }
  domainDeleteBatch = batch
  if (domainDeleteNoticeID !== null) dismissToast(domainDeleteNoticeID)
  domainDeleteNoticeID = null
  batch.introID = updateToast(null, '正在确认邮件同步状态并执行删除……', 'info', 1100)
  window.clearTimeout(domainDeleteIntroTimer)
  domainDeleteIntroTimer = window.setTimeout(() => {
    if (domainDeleteBatch !== batch) return
    batch.introVisible = false
    batch.introID = null
    renderDomainDeleteNotice(batch)
  }, 1150)
  return batch
}

function mergeDomainDeleteResult(batch, data) {
  batch.accounts += Number(data?.accounts || 0)
  batch.scanned += Number(data?.threads_scanned || 0)
  batch.found += Number(data?.cloud_messages_found || 0)
  batch.moved += Number(data?.moved_to_trash || 0)
  batch.destroyed += Number(data?.destroyed || 0)
  batch.localRemoved += Number(data?.local_removed ?? data?.deleted ?? 0)
  batch.verified += Number(data?.verified_mailboxes || 0)
  batch.fallbacks += Number(data?.fallback_accounts || 0)
}

function canRunDomainDeleteJob(job) {
  if (job.keys.includes('domain-delete:global')) return activeDomainDeleteJobs === 0 && activeDomainSyncKeys.size === 0
  if (activeDomainDeleteKeys.has('domain-delete:global')) return false
  return job.keys.every((key) => !activeDomainDeleteKeys.has(key) && !activeDomainSyncKeys.has(key))
}

function pumpDomainDeleteQueue() {
  while (activeDomainDeleteJobs < maxConcurrentDomainDeleteJobs) {
    const index = domainDeleteQueue.findIndex(canRunDomainDeleteJob)
    if (index < 0) break
    const [job] = domainDeleteQueue.splice(index, 1)
    const batch = job.batch
    batch.queued = Math.max(0, batch.queued - job.units)
    batch.running += job.units
    activeDomainDeleteJobs += 1
    job.keys.forEach((key) => activeDomainDeleteKeys.add(key))
    deletingMailboxIDs.value = [...new Set([...deletingMailboxIDs.value, ...job.mailboxIDs])]
    let jobResult
    let jobError
    Promise.resolve()
      .then(job.run)
      .then((data) => {
        batch.success += job.units
        mergeDomainDeleteResult(batch, data)
        jobResult = data
      })
      .catch((err) => {
        batch.failed += job.units
        batch.lastError = domainDeleteErrorText(err)
        jobError = err
      })
      .finally(() => {
        batch.running = Math.max(0, batch.running - job.units)
        activeDomainDeleteJobs = Math.max(0, activeDomainDeleteJobs - 1)
        job.keys.forEach((key) => activeDomainDeleteKeys.delete(key))
        deletingMailboxIDs.value = deletingMailboxIDs.value.filter((id) => !job.mailboxIDs.includes(id))
        pumpDomainDeleteQueue()
        pumpDomainSyncQueue()
        renderDomainDeleteNotice(batch)
        if (jobError) job.reject(jobError)
        else job.resolve(jobResult)
      })
  }
  renderDomainDeleteNotice()
}

function enqueueDomainDelete(keys, run, options = {}) {
  const normalizedKeys = [...new Set((keys || []).map((key) => String(key || '').trim()).filter(Boolean))]
  const mailboxIDs = [...new Set((options.mailboxIDs || []).map((id) => String(id || '').trim()).filter(Boolean))]
  const units = Math.max(1, Number(options.units) || mailboxIDs.length || 1)
  const batch = beginDomainDeleteBatch()
  batch.total += units
  batch.queued += units
  const promise = new Promise((resolve, reject) => {
    domainDeleteQueue.push({ keys: normalizedKeys.length ? normalizedKeys : ['domain-delete:global'], run, resolve, reject, batch, mailboxIDs, units })
  })
  pumpDomainDeleteQueue()
  return promise
}

function isDomainDeleteQueued(mailboxID) {
  return domainDeleteQueue.some((job) => job.mailboxIDs.includes(mailboxID))
}

function isDomainDeleting(mailboxID) {
  return deletingMailboxIDs.value.includes(mailboxID)
}

function isDomainDeleteBusy(mailboxID) {
  return deleteConfirmID.value === mailboxID || isDomainDeleting(mailboxID) || isDomainDeleteQueued(mailboxID)
}

function domainDeleteKeysForMailboxes(mailboxes) {
  return [...new Set((mailboxes || []).map(domainSyncGroupKey))]
}

async function syncMailboxes() {
  if (!routeReady.value) {
    showError('请先在系统设置中完成域名收件配置')
    return
  }
  const action = 'sync-all'
  if (!startBusy(action)) return
  try {
	const groups = new Map()
	for (const mailbox of allMailboxes.value.filter((item) => item.status !== 'disabled' && item.icloud_active !== false)) {
	  const key = domainSyncGroupKey(mailbox)
	  if (!groups.has(key)) groups.set(key, [])
	  groups.get(key).push(mailbox.id)
	}
	if (!groups.size) throw new Error('没有可同步的域名邮箱')
	await Promise.allSettled([...groups.entries()].map(([key, mailboxIDs]) => enqueueDomainSync(
	  key,
	  () => api('/api/domain-mailboxes/sync', { method: 'POST', body: JSON.stringify({ mailbox_ids: mailboxIDs }) }),
	)))
	await refreshDomainMailboxPage()
  } catch (err) {
	showError(err.message)
  } finally {
	finishBusy(action)
  }
}

async function quickSyncMailbox(mailbox) {
  if (!routeReady.value) {
    showError('请先完成域名收件配置')
    return
  }
  if (!startRowBusy(mailbox.id, 'sync')) return
  try {
	await enqueueDomainSync(domainSyncGroupKey(mailbox), () => api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}/sync`, { method: 'POST' }))
	await refreshDomainMailboxPage()
  } catch {
	// 队列顶部提示已统一展示该任务的失败信息。
  } finally {
	finishRowBusy(mailbox.id, 'sync')
  }
}

async function deleteMailbox(mailbox) {
  if (!mailbox || rowBusyAction(mailbox.id) || isDomainDeleteBusy(mailbox.id)) return
  deleteConfirmID.value = mailbox.id
  try {
	const confirmed = await confirmAction({
	  title: '删除域名邮箱',
	  message: `删除前会先把 ${mailbox.email} 的 IMAP 索引补齐到当前 UID；同步覆盖完整后按远端标识精确删除，覆盖不完整时再使用 iCloud Web 深度定位。云端完成后删除本地邮件和地址登记，继续吗？`,
	  confirmText: '确认删除',
	  tone: 'danger',
	})
	if (!confirmed) return
	deleteConfirmID.value = ''
	await enqueueDomainDelete(
	  [domainSyncGroupKey(mailbox)],
	  () => api(`/api/domain-mailboxes/${encodeURIComponent(mailbox.id)}`, { method: 'DELETE' }),
	  { mailboxIDs: [mailbox.id] },
	)
	previewMailboxes.value = previewMailboxes.value.filter((item) => item.id !== mailbox.id)
	selectedMailboxes.value = selectedMailboxes.value.filter((item) => item.id !== mailbox.id)
	if (selected.value?.id === mailbox.id) selected.value = null
	nextTick(scheduleMailboxTableSize)
  } catch {
	// 删除队列的顶部提示已统一展示失败信息。
  } finally {
	deleteConfirmID.value = ''
  }
}

function normalizePrefix(value) {
  return String(value || '').trim().toLowerCase().replace(/[^a-z0-9._+-]/g, '').replace(/^\.+|\.+$/g, '')
}

function randomToken(length) {
  const alphabet = 'abcdefghjkmnpqrstuvwxyz23456789'
  const bytes = new Uint8Array(length)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (value) => alphabet[value % alphabet.length]).join('')
}

function regenerateRandomEmails() {
  const count = Math.min(500, Math.max(1, Number(generateForm.count) || 1))
  const length = Math.min(32, Math.max(4, Number(generateForm.random_length) || 12))
  generatedRandomTokens.value = Array.from({ length: count }, () => randomToken(length))
}

function openGenerateDialog() {
  if (!configuredDomains.value.length) {
    showError('请先在系统设置中添加接收域名')
    return
  }
  generateForm.domain = configuredDomains.value[0]
  generateForm.prefix = ''
  generateForm.label = ''
  generateForm.mode = 'sequence'
  generateForm.count = 1
  generateForm.start = ''
  generateForm.random_length = 12
  regenerateRandomEmails()
  showGenerate.value = true
}

async function saveGeneratedMailbox() {
  const prefix = normalizePrefix(generateForm.prefix)
  if ((generateForm.mode === 'sequence' && !prefix) || prefix.length > 55) {
    showError('递增编号模式需要 1–55 个字符的邮箱前缀')
    return
  }
  if (!configuredDomains.value.includes(generateForm.domain)) {
    showError('请选择已配置的接收域名')
    return
  }
  const count = Number(generateForm.count)
  const startText = String(generateForm.start ?? '').trim()
  const start = startText === '' ? null : Number(startText)
  if (!singlePrefixMode.value && (!Number.isInteger(count) || count < 1 || count > 500)) {
    showError('生成数量需要在 1–500 之间')
    return
  }
  if (generateForm.mode === 'sequence' && start !== null && (!Number.isInteger(start) || start < 0 || start > 999999999)) {
    showError('开始编号需要在 0–999999999 之间')
    return
  }
  const randomLength = Number(generateForm.random_length)
  if (generateForm.mode === 'random' && (!Number.isInteger(randomLength) || randomLength < 4 || randomLength > 32)) {
    showError('随机字符长度需要在 4–32 之间')
    return
  }
  if (generatedEmails.value.some((email) => email.slice(0, email.lastIndexOf('@')).length > 64)) {
    showError('邮箱前缀与编号或随机字符的总长度不能超过 64')
    return
  }
  const existing = new Set(allMailboxes.value.map((item) => item.email))
  const duplicates = generatedEmails.value.filter((email) => existing.has(email))
  if (duplicates.length) {
    showError(`已存在 ${duplicates.length} 个邮箱，请调整前缀或开始编号`)
    return
  }
  const label = String(generateForm.label || '').trim() || '域名邮箱'
  if (!startBusy('generate')) return
  try {
	const data = await api('/api/domain-mailboxes/generate', {
		method: 'POST',
		body: JSON.stringify({ domain: generateForm.domain, emails: generatedEmails.value, label }),
	})
	previewMailboxes.value = [...(data.items || []), ...previewMailboxes.value]
	showGenerate.value = false
	page.value = 1
	success(`已生成并登记 ${data.count || generatedEmails.value.length} 个域名邮箱`)
	if (config.match_mode !== 'registered_only') info('要限制其他地址，请在系统设置将接收模式选为“只允许已生成的邮箱前缀”')
	nextTick(scheduleMailboxTableSize)
  } catch (err) {
	showError(err.message)
  } finally {
	finishBusy('generate')
  }
}

async function deleteSelectedMessages() {
  if (!selectedMailboxes.value.length || isBusy('delete-selected')) return
  const targets = selectedMailboxes.value.filter((mailbox) => !isDomainDeleteBusy(mailbox.id))
  const count = targets.length
  if (!count) return
  const confirmed = await confirmAction({
    title: '删除选中域名邮箱',
	message: `将先补齐已选 ${count} 个邮箱所属主号的 IMAP 索引，再精确删除对应云端邮件；同步覆盖不完整的账号会自动使用 iCloud Web 深度定位。云端完成后删除本地邮件和地址登记，继续吗？`,
    confirmText: '确认删除邮箱',
    tone: 'danger',
  })
  if (!confirmed || !startBusy('delete-selected')) return
  try {
	const mailboxIDs = targets.map((mailbox) => mailbox.id)
	const selectedItems = allMailboxes.value.filter((mailbox) => mailboxIDs.includes(mailbox.id))
	await enqueueDomainDelete(
	  domainDeleteKeysForMailboxes(selectedItems),
	  () => api('/api/domain-mailboxes/delete', { method: 'POST', body: JSON.stringify({ mailbox_ids: mailboxIDs }) }),
	  { mailboxIDs },
	)
	await loadDomainData({ silent: true })
	selectedMailboxes.value = []
  } catch {
	// 删除队列的顶部提示已统一展示失败信息。
  } finally {
	finishBusy('delete-selected')
  }
}

async function deleteAllMessages() {
  if (isBusy('delete-all')) return
  const confirmed = await confirmAction({
    title: '删除全部域名邮件',
	message: '将先把各收件主号的 IMAP 索引补齐到当前 UID，再精确删除全部已登记域名邮箱的云端邮件；覆盖不完整时自动使用 iCloud Web 深度定位。完成后清空本地邮件，域名邮箱地址和收件配置会保留，继续吗？',
    confirmText: '确认全部删除',
    tone: 'danger',
  })
  if (!confirmed || !startBusy('delete-all')) return
  try {
	await enqueueDomainDelete(
	  domainDeleteKeysForMailboxes(allMailboxes.value),
	  () => api('/api/domain-mailboxes/messages/delete', { method: 'POST', body: JSON.stringify({ all: true }) }),
	  { mailboxIDs: allMailboxes.value.map((mailbox) => mailbox.id), units: allMailboxes.value.length },
	)
	await loadDomainData({ silent: true })
  } catch {
	// 删除队列的顶部提示已统一展示失败信息。
  } finally {
	finishBusy('delete-all')
  }
}

function parseBulkDeleteEmails(value) {
  return [...new Set(String(value || '').split(/[\s,;，；]+/).map((item) => item.trim().toLowerCase()).filter(Boolean))]
}

function openBulkDeleteDialog() {
  bulkDeleteEmails.value = ''
  bulkDeleteError.value = ''
  showBulkDelete.value = true
}

function closeBulkDeleteDialog() {
  if (isBusy('bulk-delete')) return
  showBulkDelete.value = false
  bulkDeleteError.value = ''
}

async function submitBulkDeleteMailboxes() {
  const emails = parseBulkDeleteEmails(bulkDeleteEmails.value)
  if (!emails.length) {
    bulkDeleteError.value = '请至少输入一个邮箱地址。'
    return
  }
  const invalid = emails.filter((email) => !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email))
  if (invalid.length) {
    bulkDeleteError.value = `邮箱格式不正确：${invalid.slice(0, 3).join('、')}${invalid.length > 3 ? '等' : ''}`
    return
  }
  const mailboxByEmail = new Map(allMailboxes.value.map((mailbox) => [String(mailbox.email || '').toLowerCase(), mailbox]))
  const resolved = emails.map((email) => mailboxByEmail.get(email)).filter(Boolean)
  const targets = resolved.filter((mailbox) => !isDomainDeleteBusy(mailbox.id))
  const missing = emails.filter((email) => !mailboxByEmail.has(email))
  const queued = resolved.filter((mailbox) => isDomainDeleteBusy(mailbox.id))
  if (!targets.length) {
    if (missing.length) bulkDeleteError.value = `域名邮箱中未找到：${missing.slice(0, 5).join('、')}${missing.length > 5 ? ` 等 ${missing.length} 个邮箱` : ''}`
    else bulkDeleteError.value = '这些域名邮箱已经在删除队列中。'
    return
  }
  if (!startBusy('bulk-delete')) return
  const mailboxIDs = targets.map((mailbox) => mailbox.id)
  const task = enqueueDomainDelete(
    domainDeleteKeysForMailboxes(targets),
    () => api('/api/domain-mailboxes/delete', { method: 'POST', body: JSON.stringify({ mailbox_ids: mailboxIDs }) }),
    { mailboxIDs },
  )
  // 与邮箱池保持一致：加入删除队列后立即关闭弹窗，后续状态由顶部队列提示展示。
  showBulkDelete.value = false
  bulkDeleteEmails.value = ''
  bulkDeleteError.value = ''
  finishBusy('bulk-delete')
  info(`已将 ${targets.length} 个指定域名邮箱加入删除队列`)
  if (missing.length) showError(`域名邮箱中未找到：${missing.slice(0, 5).join('、')}${missing.length > 5 ? ` 等 ${missing.length} 个邮箱` : ''}`)
  if (queued.length) showError(`${queued.length} 个域名邮箱已在删除队列中`)
  void task
    .then(() => loadDomainData({ silent: true }))
    .catch(() => {
      // 删除失败信息由顶部队列提示统一展示。
    })
}

async function openMailbox(mailbox) {
  if (!startRowBusy(mailbox.id, 'detail')) return
  closeSelectedMessage()
  try {
	const [detail, messageData] = await Promise.all([
		api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}`),
		api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}/messages`),
	])
	selected.value = { ...mailbox, ...(detail.mailbox || {}) }
	messages.value = messageData.items || []
	Object.assign(detailEdit, {
	  status: selected.value.status || 'available',
	  note: selected.value.note || '',
	  api_active: selected.value.api_active !== false,
	  icloud_active: selected.value.icloud_active !== false,
	})
  } catch (err) {
	showError(err.message)
  } finally {
	finishRowBusy(mailbox.id, 'detail')
  }
}

async function quickGetCode(mailbox) {
  if (!startRowBusy(mailbox.id, 'code')) return
  codeMailbox.value = mailbox
  codeResult.value = null
  codeError.value = ''
  codeDialogOpen.value = true
  codeBusyVisible.value = ''
  window.clearTimeout(codeBusyTimer)
  codeBusyTimer = window.setTimeout(() => {
    if (rowBusyAction(mailbox.id) === 'code') codeBusyVisible.value = `code-row:${mailbox.id}`
  }, 600)
  try {
	codeResult.value = await api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}/code?allow_stale=1`)
	const [detail, messageData] = await Promise.all([
	  api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}`),
	  api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}/messages`),
	])
	if (detail.mailbox) {
		codeMailbox.value = { ...mailbox, ...detail.mailbox }
		previewMailboxes.value = previewMailboxes.value.map((item) => item.id === mailbox.id ? { ...item, ...detail.mailbox } : item)
		if (selected.value?.id === mailbox.id) selected.value = { ...selected.value, ...detail.mailbox }
	}
	if (selected.value?.id === mailbox.id) messages.value = messageData.items || []
  } catch (err) {
	codeError.value = err.message || '当前邮箱尚未保存可提取的验证码。'
  } finally {
	window.clearTimeout(codeBusyTimer)
	finishRowBusy(mailbox.id, 'code')
	codeBusyVisible.value = ''
  }
}

async function cleanSelectedMailboxMessages() {
  const mailbox = selected.value
  if (!mailbox || isDomainDeleteBusy(mailbox.id)) return
  const confirmed = await confirmAction({
	title: '删除该邮箱的全部邮件',
	message: `将先补齐 ${mailbox.email} 的同步索引，再把对应 iCloud 云端邮件移入废纸篓并彻底删除，最后清空本地邮件；邮箱地址登记会保留。继续吗？`,
	confirmText: '确认删除邮件',
	tone: 'danger',
  })
  if (!confirmed) return
  try {
	await enqueueDomainDelete(
	  [domainSyncGroupKey(mailbox)],
	  () => api('/api/domain-mailboxes/messages/delete', { method: 'POST', body: JSON.stringify({ mailbox_ids: [mailbox.id] }) }),
	  { mailboxIDs: [mailbox.id] },
	)
	await refreshDomainMailboxPage()
  } catch {
	// 删除队列的顶部提示已统一展示失败信息。
  }
}

function closeCodeDialog() {
  if (codeDialogBusy.value) return
  codeDialogOpen.value = false
  codeMailbox.value = null
  codeResult.value = null
  codeError.value = ''
}

function latestSubject(item) {
  return item.latest_subject || item.messages?.[0]?.subject || item.note || '暂无邮件'
}

function looksLikeHTML(value) {
  return /<(?:!doctype|html|head|body|style|table|div|p|a|img|span)\b/i.test(String(value || ''))
}

function messageContentTypeLabel(message) {
  const contentType = String(message?.content_type || '').toLowerCase()
  if (String(message?.html_body || '').trim() || contentType.includes('text/html') || looksLikeHTML(message?.body)) return 'HTML 邮件'
  if (contentType.includes('text/plain')) return '纯文本邮件'
  return '邮件正文'
}

function shrinkEmailFontSizes(value) {
  return String(value || '').replace(/(font-size\s*:\s*)(\d+(?:\.\d+)?)px/gi, (match, prefix, rawSize) => {
    const size = Number(rawSize)
    if (!Number.isFinite(size) || size < 12) return match
    const reducedSize = Math.max(11, Math.round(size * 0.82))
    return `${prefix}${reducedSize}px`
  })
}

function buildEmailHTMLDocument(value) {
  if (!value || typeof DOMParser === 'undefined') return ''
  const documentNode = new DOMParser().parseFromString(value, 'text/html')
  documentNode.querySelectorAll('script, iframe, object, embed, form, input, button, textarea, select, base, meta[http-equiv="refresh"]').forEach((element) => element.remove())
  documentNode.querySelectorAll('*').forEach((element) => {
    for (const attribute of [...element.attributes]) {
      const name = attribute.name.toLowerCase()
      const attributeValue = attribute.value.trim().toLowerCase()
      if (name.startsWith('on') || ((name === 'href' || name === 'src' || name === 'action') && attributeValue.startsWith('javascript:'))) {
        element.removeAttribute(attribute.name)
      }
    }
  })
  documentNode.querySelectorAll('a[href]').forEach((link) => {
    link.setAttribute('target', '_blank')
    link.setAttribute('rel', 'noopener noreferrer')
  })
  // 只调整用于展示的副本，保留数据库中的原始邮件 HTML。
  documentNode.querySelectorAll('style').forEach((element) => {
    element.textContent = shrinkEmailFontSizes(element.textContent)
  })
  documentNode.querySelectorAll('[style]').forEach((element) => {
    element.setAttribute('style', shrinkEmailFontSizes(element.getAttribute('style')))
  })
  const policy = documentNode.createElement('meta')
  policy.setAttribute('http-equiv', 'Content-Security-Policy')
  policy.setAttribute('content', "default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline' https: http:; font-src https: http: data:; media-src https: http: data:; script-src 'none'; object-src 'none'; frame-src 'none'; form-action 'none'")
  const viewport = documentNode.createElement('meta')
  viewport.setAttribute('name', 'viewport')
  viewport.setAttribute('content', 'width=device-width, initial-scale=1')
  const readerStyle = documentNode.createElement('style')
  readerStyle.textContent = 'html{color-scheme:light;background:#fff;scrollbar-width:thin;scrollbar-color:#cbd5e1 transparent}body{box-sizing:border-box;min-height:100%;margin:0;padding:24px;color:#172033;background:#fff;font-size:13px;overflow-wrap:anywhere}::-webkit-scrollbar{width:8px;height:8px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{border:2px solid transparent;border-radius:9999px;background:#cbd5e1;background-clip:content-box}img{max-width:100%;height:auto}table{max-width:100%}pre{white-space:pre-wrap}a{color:#0a6cff}'
  documentNode.head.prepend(policy, viewport, readerStyle)
  return `<!doctype html>\n${documentNode.documentElement.outerHTML}`
}

async function openMessage(item) {
  if (!item || !selected.value?.id) return
  const requestID = ++messageLoadRequestID
  selectedMessage.value = item
  messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
  selectedMessageLoading.value = true
  try {
    const data = await api(`/api/mailboxes/${encodeURIComponent(selected.value.id)}/messages/${encodeURIComponent(item.id)}`)
    if (requestID !== messageLoadRequestID || selectedMessage.value?.id !== item.id) return
    const detail = data.message || item
    selectedMessage.value = detail
    messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
    messages.value = messages.value.map((message) => message.id === detail.id ? detail : message)
  } catch (err) {
    if (requestID === messageLoadRequestID && selectedMessage.value?.id === item.id) showError(`完整邮件加载失败：${err.message}`)
  } finally {
    if (requestID === messageLoadRequestID) selectedMessageLoading.value = false
  }
}

function closeSelectedMessage() {
  messageLoadRequestID++
  selectedMessageLoading.value = false
  selectedMessage.value = null
  messageViewMode.value = 'auto'
}

function statusLabel(value) {
  return ({
    available: '可用',
    reserved: '已预留',
    used: '已使用',
    failed: '失败',
    disabled: '已停用',
    active: '活跃',
  })[value] || value || '未知'
}

function statusClass(value) {
  if (value === 'available' || value === 'active') return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/40 dark:text-emerald-300'
  if (value === 'reserved') return 'bg-violet-50 text-violet-600 dark:bg-violet-950/40 dark:text-violet-300'
  if (value === 'failed') return 'bg-rose-50 text-rose-600 dark:bg-rose-950/40 dark:text-rose-300'
  if (value === 'disabled') return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
  return 'bg-amber-50 text-amber-600 dark:bg-amber-950/40 dark:text-amber-300'
}

function receiverChannelLabel(route) {
  return route?.receiver_type === 'custom_imap'
    ? (route.receiver_label || route.imap_username || '标准 IMAP 邮箱')
    : '复用现有 iCloud IMAP'
}

function supportsWebAPI(item) {
  const route = configuredRoutes.value.find((candidate) => candidate.domain === item?.domain)
  return item?.receiver_type === 'apple_account' || route?.receiver_type === 'apple_account'
}

function receiverForDomain(domain) {
  return configuredRoutes.value.find((item) => item.domain === domain)?.forward_to_email || '未配置'
}

function receiverChannelForDomain(domain) {
  return receiverChannelLabel(configuredRoutes.value.find((item) => item.domain === domain))
}

function formatTime(value) {
  if (!value) return '尚未收件'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '尚未收件' : date.toLocaleString('zh-CN')
}

function formatSyncTime(value) {
  if (!value) return '尚未同步'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) || date.getFullYear() < 2000 ? '尚未同步' : date.toLocaleString('zh-CN')
}

function compareMailboxCreatedAt(left, right) {
  const leftTime = Date.parse(left?.created_at || '') || 0
  const rightTime = Date.parse(right?.created_at || '') || 0
  if (leftTime !== rightTime) return rightTime - leftTime
  return String(right?.id || '').localeCompare(String(left?.id || ''), undefined, { numeric: true, sensitivity: 'base' })
}

function domainMailboxDisplayID(mailboxID) {
  const value = String(mailboxID || '').trim()
  const matched = /^dm_(\d+)$/i.exec(value)
  if (!matched) return value || '-'
  const numeric = Number.parseInt(matched[1], 10)
  return Number.isSafeInteger(numeric) ? String(numeric) : matched[1].replace(/^0+(?=\d)/, '')
}

function formatMessageTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '-'
  return date.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
}

async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value)
    return
  }
  const textarea = document.createElement('textarea')
  textarea.value = value
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  textarea.remove()
  if (!copied) throw new Error('复制失败')
}

async function copyMailboxEmail(item) {
  try {
    await copyText(item.email)
    success(`邮箱已复制：${item.email}`)
  } catch (err) {
    showError(err.message || '复制邮箱失败，请重试')
  }
}

async function copyCode() {
  if (!codeResult.value?.code) return
  try {
    await copyText(codeResult.value.code)
    success('验证码已复制')
  } catch (err) {
    showError(err.message || '复制验证码失败，请重试')
  }
}

function movePage(offset) {
  page.value = Math.min(totalPages.value, Math.max(1, page.value + offset))
}

function calculateMailboxTableSize() {
  const element = mailboxTableViewport.value
  if (!element) return pageSize.value
  const scrollContainer = element.closest('.page-scroll')
  const scrollTop = scrollContainer?.scrollTop || 0
  const viewportTop = element.getBoundingClientRect().top + scrollTop
  const scrollBottom = scrollContainer?.getBoundingClientRect().bottom || window.innerHeight
  const scrollStyle = scrollContainer ? window.getComputedStyle(scrollContainer) : null
  const bottomPadding = Number.parseFloat(scrollStyle?.paddingBottom || '0') || 0
  const footerHeight = mailboxPagination.value?.getBoundingClientRect().height || 53
  const headerHeight = element.querySelector('thead')?.getBoundingClientRect().height || 36
  const dataRow = element.querySelector('tbody tr:not(.mailbox-empty-row)')
  const rowHeight = dataRow?.getBoundingClientRect().height || 48
  const tableWidth = element.querySelector('table')?.scrollWidth || 0
  const hasHorizontalScrollbar = tableWidth > element.clientWidth + 1
  const reservedHeight = (hasHorizontalScrollbar ? 8 : 0) + 1
  const minimumRows = window.matchMedia('(max-width: 620px)').matches ? 3 : 5
  const availableHeight = scrollBottom - viewportTop - footerHeight - bottomPadding - 2
  const visibleRows = Math.max(minimumRows, Math.min(50, Math.floor((availableHeight - headerHeight - reservedHeight) / rowHeight)))
  mailboxEmptyHeight.value = Math.floor(visibleRows * rowHeight)
  mailboxTableHeight.value = Math.floor(headerHeight + mailboxEmptyHeight.value + reservedHeight)
  return visibleRows
}

function applyMailboxTableSize() {
  const nextPageSize = calculateMailboxTableSize()
  if (nextPageSize === pageSize.value) return
  const firstVisibleIndex = (page.value - 1) * pageSize.value
  pageSize.value = nextPageSize
  page.value = Math.floor(firstVisibleIndex / nextPageSize) + 1
}

function scheduleMailboxTableSize() {
  window.clearTimeout(tableResizeTimer)
  tableResizeTimer = window.setTimeout(applyMailboxTableSize, 100)
}

function handlePageKeydown(event) {
  if (event.key !== 'Escape') return
  if (selectedMessage.value) closeSelectedMessage()
  else if (codeDialogOpen.value) closeCodeDialog()
  else if (showBulkDelete.value) closeBulkDeleteDialog()
  else if (showGenerate.value) showGenerate.value = false
  else if (quickEditOpen.value) closeQuickEdit()
  else if (selected.value) selected.value = null
}

onMounted(async () => {
  document.addEventListener('keydown', handlePageKeydown)
	await loadDomainData()
  realtimeUnsubscribe = subscribeRealtime(['mailbox', 'message', 'mailbox-message-sync'], scheduleRealtimeDomainRefresh)
  autoRefreshTimer = window.setInterval(() => { void refreshDomainMailboxPage() }, 30000)
  await nextTick()
	scheduleMailboxTableSize()
  mailboxLayoutObserver = new ResizeObserver(scheduleMailboxTableSize)
  if (mailboxCommandBar.value) mailboxLayoutObserver.observe(mailboxCommandBar.value)
  if (mailboxRouteStrip.value) mailboxLayoutObserver.observe(mailboxRouteStrip.value)
  if (mailboxPagination.value) mailboxLayoutObserver.observe(mailboxPagination.value)
  const scrollContainer = mailboxTableViewport.value?.closest('.page-scroll')
  if (scrollContainer) mailboxLayoutObserver.observe(scrollContainer)
  window.addEventListener('resize', scheduleMailboxTableSize)
})

onBeforeUnmount(() => {
  window.clearTimeout(tableResizeTimer)
  window.clearTimeout(codeBusyTimer)
  window.clearTimeout(domainSyncIntroTimer)
  window.clearTimeout(domainDeleteIntroTimer)
  window.clearTimeout(realtimeRefreshTimer)
  window.clearInterval(autoRefreshTimer)
  realtimeUnsubscribe()
  window.removeEventListener('resize', scheduleMailboxTableSize)
  mailboxLayoutObserver?.disconnect()
  document.removeEventListener('keydown', handlePageKeydown)
  document.body.style.overflow = ''
})
</script>

<template>
  <div class="mailbox-page domain-mailbox-page">
    <section class="panel mailbox-workbench domain-mailbox-workbench">
      <div ref="mailboxCommandBar" class="mailbox-command-bar">
        <div class="mailbox-command-filters">
          <label class="field-wrap mailbox-search"><Search :size="15" class="field-icon" /><input v-model="search" class="field field-leading" type="search" placeholder="搜索邮箱或邮件主题" aria-label="搜索域名邮箱" /></label>
          <CardSelect v-model="domainFilter" class="mailbox-account-filter" :options="domainFilterOptions" aria-label="接收域名" compact />
          <CardSelect v-model="statusFilter" class="mailbox-status-filter" :options="statusFilterOptions" aria-label="邮箱状态" compact />
        </div>
        <div class="mailbox-command-actions">
          <button type="button" class="secondary-button mailbox-command-button domain-mailbox-generate-button" title="生成并登记新的域名邮箱前缀" @click="openGenerateDialog"><Sparkles :size="14" />生成邮箱前缀</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('sync-all')" title="通过已配置的 IMAP 收件通道同步全部域名邮件" @click="syncMailboxes"><LoaderCircle v-if="isBusy('sync-all')" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />{{ isBusy('sync-all') ? '正在同步' : '同步全部邮件' }}</button>
          <button type="button" class="secondary-button mailbox-command-button mailbox-command-button-danger" :disabled="!allMailboxes.length || isBusy('delete-all')" title="按收件人定位云端邮件，移入废纸篓后彻底删除，成功后再清理本地邮件" @click="deleteAllMessages"><LoaderCircle v-if="isBusy('delete-all')" :size="14" class="animate-spin" /><Trash2 v-else :size="14" />全部删除邮件</button>
          <button type="button" class="secondary-button mailbox-command-button mailbox-command-button-danger" :disabled="isBusy('bulk-delete')" title="按邮箱地址批量删除云端邮件、本地邮件和域名邮箱登记" @click="openBulkDeleteDialog"><Trash2 :size="14" />批量删除指定邮箱</button>
          <button type="button" class="secondary-button mailbox-command-button mailbox-command-button-danger" :disabled="!selectedDeletableCount || isBusy('delete-selected')" :title="selectedDeletableCount ? `同步校验后删除选中的 ${selectedDeletableCount} 个域名邮箱` : '请先选择未进入删除队列的域名邮箱'" @click="deleteSelectedMessages"><LoaderCircle v-if="isBusy('delete-selected')" :size="14" class="animate-spin" /><Trash2 v-else :size="14" />删除选中{{ selectedDeletableCount ? `（${selectedDeletableCount}）` : '' }}</button>
        </div>
      </div>

      <div ref="mailboxRouteStrip" class="domain-mailbox-route-strip">
        <div class="domain-mailbox-route-channel"><span><ShieldCheck :size="16" /></span><div><small>收件通道</small><strong>{{ receiverText }}</strong></div><em :class="routeReady ? 'is-ready' : ''">{{ routeStatusText }}</em></div>
        <dl><div><dt>接收域名</dt><dd>{{ configuredDomains.length }}</dd></div><div><dt>已发现邮箱</dt><dd>{{ allMailboxes.length }}</dd></div><div><dt>本地邮件</dt><dd>{{ totalMessages }}</dd></div></dl>
      </div>

      <div v-if="loading" class="mailbox-loading-mask"><div><LoaderCircle :size="16" class="animate-spin" />正在加载域名邮箱</div></div>
      <div ref="mailboxTableViewport" class="mailbox-table-viewport domain-mailbox-table-viewport" :style="{ '--mailbox-table-height': `${mailboxTableHeight}px`, '--mailbox-empty-height': `${mailboxEmptyHeight}px` }">
        <table class="mailbox-pool-table domain-mailbox-pool-table" :class="{ 'mailbox-pool-table-empty': !pagedMailboxes.length }">
          <colgroup v-if="pagedMailboxes.length"><col class="domain-mailbox-col-id" /><col class="domain-mailbox-col-domain" /><col class="domain-mailbox-col-email" /><col class="domain-mailbox-col-label" /><col class="domain-mailbox-col-kind" /><col class="domain-mailbox-col-channel" /><col class="domain-mailbox-col-count" /><col class="domain-mailbox-col-sync" /><col class="domain-mailbox-col-actions" /></colgroup>
          <thead><tr><th><label class="mailbox-select-all"><input type="checkbox" :checked="allPageMailboxesSelected" :indeterminate="somePageMailboxesSelected" :disabled="!pagedMailboxes.length" aria-label="全选当前页邮箱" @change="toggleAllMailboxSelection" /><span>ID</span></label></th><th>接收域名</th><th>域名邮箱</th><th>备注</th><th>状态</th><th>API / 收件</th><th>收件</th><th>最近同步</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="item in pagedMailboxes" :key="item.id" :class="{ 'mailbox-row-selected': isMailboxSelected(item.id) }">
              <td class="mailbox-id-cell"><label class="mailbox-id-wrap"><input type="checkbox" :checked="isMailboxSelected(item.id)" :disabled="isDomainDeleteBusy(item.id)" :aria-label="`选择邮箱 ${item.email}`" @change="toggleMailboxSelection(item)" /><span :title="`内部 ID：${item.id}`">{{ domainMailboxDisplayID(item.id) }}</span></label></td>
              <td><span class="domain-mailbox-domain" :title="item.domain">@{{ item.domain }}</span></td>
              <td class="mailbox-email-cell"><button type="button" class="mailbox-email-copy" :title="`点击复制邮箱：${item.email}`" @click.stop="copyMailboxEmail(item)">{{ item.email }}</button></td>
              <td class="mailbox-label-cell"><span :title="item.label || '—'">{{ item.label || '—' }}</span><button type="button" :title="item.note ? `点击修改备注：${item.note}` : '点击添加备注'" @click.stop="openQuickEdit(item, 'note')">{{ item.note || '添加备注' }}</button></td>
              <td><button type="button" class="mailbox-status-button" :class="statusClass(item.status)" title="点击修改邮箱状态" @click.stop="openQuickEdit(item, 'status')">{{ statusLabel(item.status) }}</button></td>
              <td><div class="mailbox-channel-badges domain-mailbox-channel-badges"><span :class="item.api_active ? 'text-emerald-600 bg-emerald-50 dark:bg-emerald-950/40' : 'text-slate-400 bg-slate-100 dark:bg-slate-700'">API</span><span class="text-sky-600 bg-sky-50 dark:bg-sky-950/40">IMAP</span><span :class="supportsWebAPI(item) ? 'text-violet-600 bg-violet-50 dark:bg-violet-950/40' : 'text-slate-400 bg-slate-100 dark:bg-slate-700'" :title="supportsWebAPI(item) ? 'Apple 收件账号支持 Web API' : '标准 IMAP 收件箱不支持 Web API'">Web API</span></div></td>
              <td class="mailbox-count">{{ item.receive_count }}</td>
              <td class="mailbox-sync-time">{{ formatSyncTime(item.last_sync_at) }}</td>
              <td><div class="mailbox-row-actions"><button class="mailbox-action-button mailbox-action-sync" :class="{ 'mailbox-action-sync-selected': rowBusyAction(item.id) === 'sync' }" :disabled="Boolean(rowBusyAction(item.id)) || isDomainDeleteBusy(item.id)" title="通过收件通道同步该邮箱邮件" @click.stop="quickSyncMailbox(item)"><LoaderCircle v-if="rowBusyAction(item.id) === 'sync'" :size="12" class="animate-spin" /><RefreshCw v-else :size="12" />同步</button><button class="mailbox-action-button mailbox-action-code" :class="{ 'mailbox-action-code-selected': rowBusyAction(item.id) === 'code' || (codeDialogOpen && codeMailbox?.id === item.id) }" :disabled="Boolean(rowBusyAction(item.id)) || isDomainDeleteBusy(item.id)" title="获取该邮箱的最新验证码" @click.stop="quickGetCode(item)"><LoaderCircle v-if="codeBusyVisible === `code-row:${item.id}`" :size="12" class="animate-spin" /><KeyRound v-else :size="12" />取码</button><button class="mailbox-action-button mailbox-action-detail" :class="{ 'mailbox-action-detail-selected': selected?.id === item.id }" :disabled="Boolean(rowBusyAction(item.id)) || isDomainDeleteBusy(item.id)" title="查看邮箱与本地邮件" @click.stop="openMailbox(item)"><LoaderCircle v-if="rowBusyAction(item.id) === 'detail'" :size="12" class="animate-spin" /><MailOpen v-else :size="12" />详情</button><button class="mailbox-action-button mailbox-action-delete" :class="{ 'mailbox-action-delete-selected': isDomainDeleteBusy(item.id) }" :disabled="Boolean(rowBusyAction(item.id)) || isDomainDeleteBusy(item.id)" :title="isDomainDeleting(item.id) ? '正在校验同步状态并删除域名邮箱' : isDomainDeleteQueued(item.id) ? '已加入域名邮箱删除队列' : '同步校验完成后删除云端邮件和地址登记'" @click.stop="deleteMailbox(item)"><LoaderCircle v-if="isDomainDeleteBusy(item.id)" :size="12" class="animate-spin" /><Trash2 v-else :size="12" />{{ isDomainDeleting(item.id) ? '删除中' : isDomainDeleteQueued(item.id) ? '排队中' : '删除' }}</button></div></td>
            </tr>
            <tr v-if="!loading && !pagedMailboxes.length" class="mailbox-empty-row"><td colspan="9" class="mailbox-empty-cell"><span><Boxes :size="20" /></span><strong>{{ search || domainFilter || statusFilter ? '没有符合条件的域名邮箱' : '尚未登记域名邮箱' }}</strong><small>{{ routeReady ? '点击“生成邮箱前缀”登记可接收和取码的地址。' : '请先在系统设置中完成域名收件配置。' }}</small></td></tr>
          </tbody>
        </table>
      </div>
      <div ref="mailboxPagination" class="mailbox-pagination"><span>第 {{ page }} / {{ totalPages }} 页　总 {{ filteredMailboxes.length }} 个邮箱<span v-if="selectedMailboxes.length">　已选 {{ selectedMailboxes.length }} 个</span></span><div><button type="button" class="secondary-button" :disabled="page <= 1" title="跳转到首页" aria-label="跳转到首页" @click="page = 1"><ChevronsLeft :size="15" /></button><button type="button" class="secondary-button" :disabled="page <= 1" title="上一页" aria-label="上一页" @click="movePage(-1)"><ChevronLeft :size="15" /></button><button type="button" class="secondary-button" :disabled="page >= totalPages" title="下一页" aria-label="下一页" @click="movePage(1)"><ChevronRight :size="15" /></button><button type="button" class="secondary-button" :disabled="page >= totalPages" title="跳转到末页" aria-label="跳转到末页" @click="page = totalPages"><ChevronsRight :size="15" /></button></div></div>
    </section>

    <FormDialog class="mailbox-quick-edit-dialog" :open="quickEditOpen" :title="quickEditTitle" :description="quickEditMailbox?.email || ''" :busy="isBusy('quick-edit')" @close="closeQuickEdit" @submit="saveQuickEdit">
      <label v-if="quickEditField === 'note'" class="form-group"><span class="form-label">备注</span><textarea v-model="quickEdit.note" class="field min-h-24 resize-none" maxlength="1000" placeholder="请输入邮箱备注，留空可清除备注" autofocus /></label>
      <div v-else class="form-group"><span class="form-label">邮箱状态</span><CardSelect v-model="quickEdit.status" :options="mailboxDetailStatusOptions" aria-label="邮箱状态" /></div>
    </FormDialog>

    <Teleport to="body">
      <div v-if="showGenerate" class="task-dialog-backdrop" role="presentation" @click.self="showGenerate = false">
        <form class="panel task-settings-dialog domain-mailbox-generate-dialog" role="dialog" aria-modal="true" aria-labelledby="domain-generate-title" @submit.prevent="saveGeneratedMailbox">
          <div class="task-dialog-heading"><div><h2 id="domain-generate-title"><Sparkles :size="18" />生成邮箱前缀</h2><p>生成或自定义前缀，并登记为允许收件和取码的域名邮箱。</p></div><button type="button" class="icon-button" title="关闭生成弹窗" aria-label="关闭生成弹窗" @click="showGenerate = false"><X :size="16" /></button></div>
          <div class="task-settings-grid domain-mailbox-generate-grid">
            <label class="form-group"><span class="form-label">邮箱前缀</span><input v-model="generateForm.prefix" class="field font-mono" type="text" maxlength="55" autocomplete="off" spellcheck="false" :placeholder="generateForm.mode === 'sequence' ? '例如 mrhuang' : '可选，例如 mrhuang'" :required="generateForm.mode === 'sequence'" /><span class="form-help">{{ generateForm.mode === 'sequence' ? '开始编号留空时，直接创建该指定前缀。' : '留空生成纯随机前缀；填写后作为固定开头。' }}</span></label>
            <div class="form-group"><span class="form-label">接收域名</span><CardSelect v-model="generateForm.domain" :options="generateDomainOptions" aria-label="选择接收域名" /></div>
            <div class="form-group"><span class="form-label">生成方式</span><CardSelect v-model="generateForm.mode" :options="generateModeOptions" aria-label="邮箱前缀生成方式" /></div>
            <label class="form-group"><span class="form-label">生成数量</span><input v-model.number="generateForm.count" class="field font-mono domain-number-field" type="number" min="1" max="500" inputmode="numeric" :disabled="singlePrefixMode" :required="!singlePrefixMode" /><span class="form-help">{{ singlePrefixMode ? '指定前缀模式固定创建 1 个邮箱。' : '一次可生成 1–500 个域名邮箱。' }}</span></label>
            <label v-if="generateForm.mode === 'sequence'" class="form-group"><span class="form-label">开始编号</span><input v-model="generateForm.start" class="field font-mono domain-number-field" type="number" min="0" max="999999999" inputmode="numeric" placeholder="留空则不添加编号" /><span class="form-help">留空创建 mrhuang@域名；填写 1 则从 mrhuang1 开始。</span></label>
            <label v-else class="form-group"><span class="form-label">随机字符长度</span><input v-model.number="generateForm.random_length" class="field font-mono" type="number" min="4" max="32" inputmode="numeric" required /><span class="form-help">每个地址生成 4–32 位小写字母和数字。</span></label>
            <label class="form-group"><span class="form-label">名称</span><input v-model.trim="generateForm.label" class="field" type="text" maxlength="80" placeholder="可选，例如 ChatGPT 注册" /></label>
          </div>
          <div class="task-dialog-actions"><button type="button" class="secondary-button" @click="showGenerate = false">取消</button><button type="submit" class="primary-button"><Sparkles :size="16" />{{ generatedEmails.length ? `生成 ${generatedEmails.length} 个邮箱` : '生成邮箱' }}</button></div>
        </form>
      </div>
    </Teleport>

    <div v-if="showBulkDelete" class="fixed inset-0 z-[75] !m-0 flex items-center justify-center bg-slate-950/60 p-3 backdrop-blur-[3px] sm:p-5" role="presentation" @click.self="closeBulkDeleteDialog">
      <form class="panel mailbox-operation-dialog mailbox-bulk-delete-dialog overflow-hidden p-0 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="domain-bulk-delete-title" @submit.prevent="submitBulkDeleteMailboxes">
        <header class="mailbox-dialog-heading"><div><h2 id="domain-bulk-delete-title"><Trash2 :size="18" />批量删除指定邮箱</h2><p>一行输入一个邮箱。提交后先补齐共享 IMAP 索引，删除对应云端邮件，再删除本地邮件和域名邮箱登记；任务会进入顶部删除队列。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('bulk-delete')" @click="closeBulkDeleteDialog"><X :size="16" /></button></header>
        <div class="mailbox-bulk-delete-fields"><label class="form-group"><span class="form-label">邮箱地址列表</span><textarea v-model="bulkDeleteEmails" class="field mailbox-bulk-delete-input" placeholder="code@example.com&#10;notice@example.com" spellcheck="false" autofocus required /><span class="form-help">已识别 {{ bulkDeleteEmailCount }} 个邮箱；重复地址会自动合并。</span></label><div v-if="bulkDeleteError" class="mailbox-bulk-delete-error" role="alert">{{ bulkDeleteError }}</div></div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('bulk-delete')" @click="closeBulkDeleteDialog">取消</button><button class="primary-button mailbox-bulk-delete-submit" :disabled="isBusy('bulk-delete') || !bulkDeleteEmailCount"><LoaderCircle v-if="isBusy('bulk-delete')" :size="15" class="animate-spin" /><Trash2 v-else :size="15" />{{ isBusy('bulk-delete') ? '正在加入队列' : `开始删除（${bulkDeleteEmailCount}）` }}</button></footer>
      </form>
    </div>

    <div v-if="selected" class="fixed inset-0 z-40 !m-0 flex items-center justify-center bg-slate-950/55 p-3 backdrop-blur-[3px] sm:p-5" role="presentation" @click.stop>
      <aside class="mailbox-detail-dialog max-h-[calc(100vh-1.5rem)] w-full max-w-2xl overflow-y-auto rounded-2xl border border-slate-200 bg-slate-50 shadow-2xl dark:border-slate-700 dark:bg-slate-950 sm:max-h-[calc(100vh-2.5rem)]" role="dialog" aria-modal="true" aria-labelledby="domain-mailbox-detail-title">
        <header class="sticky top-0 z-10 rounded-t-2xl border-b border-slate-200 bg-white/95 px-4 py-3 backdrop-blur dark:border-slate-800 dark:bg-slate-900/95"><div class="flex items-start justify-between gap-3"><div class="min-w-0"><div class="mb-1.5 flex flex-wrap items-center gap-1.5"><span :class="statusClass(selected.status)" class="rounded-md px-2 py-0.5 text-[10px] font-bold">{{ statusLabel(selected.status) }}</span><span :class="selected.api_active ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200' : 'bg-slate-200 text-slate-500 dark:bg-slate-700 dark:text-slate-300'" class="rounded-md px-2 py-0.5 text-[10px] font-bold">API</span><span class="rounded-md bg-sky-100 px-2 py-0.5 text-[10px] font-bold text-sky-700 dark:bg-sky-950/60 dark:text-sky-200">IMAP</span><span :class="supportsWebAPI(selected) ? 'bg-violet-100 text-violet-700 dark:bg-violet-950/60 dark:text-violet-200' : 'bg-slate-200 text-slate-500 dark:bg-slate-700 dark:text-slate-300'" class="rounded-md px-2 py-0.5 text-[10px] font-bold">Web API</span></div><h2 id="domain-mailbox-detail-title" class="truncate text-base font-black">{{ selected.email }}</h2><p class="mt-0.5 truncate text-[10px] text-sky-500">接收邮箱：{{ receiverForDomain(selected.domain) }}</p><p class="mt-0.5 truncate font-mono text-[10px] text-slate-400">{{ selected.id }}</p></div><button class="icon-button h-8 w-8 rounded-lg" title="关闭详情" @click="selected = null"><X :size="17" /></button></div></header>
        <div class="space-y-3 p-4">
          <div class="grid grid-cols-2 gap-2"><button class="detail-button detail-button-primary" :disabled="rowBusyAction(selected.id) === 'sync' || isDomainDeleteBusy(selected.id)" @click="quickSyncMailbox(selected)"><RefreshCw :size="14" :class="{ 'animate-spin': rowBusyAction(selected.id) === 'sync' }" />同步邮件</button><button class="detail-button detail-button-secondary" :disabled="rowBusyAction(selected.id) === 'code' || isDomainDeleteBusy(selected.id)" @click="quickGetCode(selected)"><LoaderCircle v-if="codeBusyVisible === `code-row:${selected.id}`" :size="14" class="animate-spin" /><KeyRound v-else :size="14" />获取验证码</button></div>

          <form class="rounded-xl border border-slate-200 bg-white p-3 dark:border-slate-800 dark:bg-slate-900" @submit.prevent="saveDetailEdit">
            <div class="mb-1 flex flex-wrap items-center justify-between gap-2"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">状态与接收</h3><div class="ml-auto flex items-center gap-2"><div class="flex items-center gap-1.5"><span class="whitespace-nowrap text-[10px] font-bold text-slate-400">使用状态</span><CardSelect v-model="detailEdit.status" class="detail-status-select" :options="mailboxDetailStatusOptions" aria-label="使用状态" compact /></div><button class="detail-button detail-button-secondary h-8" :disabled="isBusy('detail-save') || isDomainDeleteBusy(selected.id)"><LoaderCircle v-if="isBusy('detail-save')" :size="13" class="animate-spin" /><Save v-else :size="13" />保存</button></div></div>
            <label class="block space-y-1.5"><span class="text-[11px] font-bold text-slate-500 dark:text-slate-300">备注</span><input v-model="detailEdit.note" type="text" class="field detail-field" maxlength="1000" placeholder="可选备注" /></label>
            <div class="detail-setting-grid mt-3"><label class="detail-setting-row"><span><strong>公共取码 API</strong><small>控制外部接口取码和邮件查询</small></span><input v-model="detailEdit.api_active" class="detail-switch" type="checkbox" /></label><label class="detail-setting-row"><span><strong>域名邮箱收件</strong><small>{{ receiverChannelForDomain(selected.domain) }}</small></span><input v-model="detailEdit.icloud_active" class="detail-switch" type="checkbox" /></label></div>
          </form>

          <section class="rounded-xl border border-slate-200 bg-white p-3 dark:border-slate-800 dark:bg-slate-900"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">收件信息</h3><dl class="domain-mailbox-detail-list"><div><dt>接收域名</dt><dd>@{{ selected.domain }}</dd></div><div><dt>接收邮箱</dt><dd>{{ receiverForDomain(selected.domain) }}</dd></div><div><dt>收件通道</dt><dd>{{ receiverChannelForDomain(selected.domain) }}</dd></div><div><dt>本地邮件</dt><dd>{{ selected.receive_count }} 封</dd></div><div><dt>最近同步</dt><dd>{{ formatSyncTime(selected.last_sync_at) }}</dd></div></dl></section>
          <section class="rounded-xl border border-slate-200 bg-white p-3 dark:border-slate-800 dark:bg-slate-900">
            <div class="mb-2.5 flex items-start justify-between gap-3"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">本地邮件 <span class="ml-1 text-slate-400">{{ messages.length }}</span></h3><span class="whitespace-nowrap text-[10px] text-slate-400">同步于 {{ formatSyncTime(selected.last_sync_at) }}</span></div>
            <div v-if="!messages.length" class="mail-message-list flex items-center justify-center text-xs text-slate-400">暂无本地邮件</div>
            <div v-else class="mail-message-list">
              <button v-for="item in messages" :key="item.id" type="button" class="mail-message-row" :title="`查看完整邮件：${item.subject || '无主题'}`" @click="openMessage(item)">
                <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/50 dark:text-sky-300"><MailOpen :size="15" /></span>
                <span class="min-w-0 flex-1 text-left"><strong class="block truncate text-xs">{{ item.subject || '无主题' }}</strong><small class="mt-0.5 block truncate text-[10px] text-slate-400">{{ item.from || '未知发件人' }}</small></span>
                <span class="flex shrink-0 items-center gap-1.5"><time class="text-[9px] text-slate-400">{{ formatMessageTime(item.received_at) }}</time><ChevronRight :size="14" class="text-slate-300 dark:text-slate-600" /></span>
              </button>
            </div>
          </section>

          <section class="rounded-xl border border-rose-200/70 bg-white p-3 dark:border-rose-950 dark:bg-slate-900"><div class="mb-2.5"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">清理与删除</h3><p class="mt-0.5 text-[10px] leading-4 text-slate-400">删除前先补齐 IMAP 索引；覆盖完整时按远端标识快速处理，覆盖不完整时使用 iCloud Web 深度定位。</p></div><div class="grid gap-2 sm:grid-cols-2"><button class="detail-button detail-button-secondary" :disabled="isDomainDeleteBusy(selected.id)" @click="cleanSelectedMailboxMessages"><LoaderCircle v-if="isDomainDeleteBusy(selected.id)" :size="14" class="animate-spin" /><CloudOff v-else :size="14" />删除全部邮件</button><button class="detail-button detail-button-danger" :disabled="isDomainDeleteBusy(selected.id)" @click="deleteMailbox(selected)"><LoaderCircle v-if="isDomainDeleteBusy(selected.id)" :size="14" class="animate-spin" /><Trash2 v-else :size="14" />{{ isDomainDeleting(selected.id) ? '删除中' : isDomainDeleteQueued(selected.id) ? '排队中' : '删除域名邮箱' }}</button></div></section>
        </div>
      </aside>
    </div>

    <div v-if="codeDialogOpen" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/65 p-4 backdrop-blur-[4px]" role="presentation" @click.stop>
      <section class="panel mailbox-code-dialog overflow-hidden p-0 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="domain-mailbox-code-title">
        <header class="mailbox-code-heading flex items-start justify-between gap-4 border-b border-slate-200 px-5 py-4 dark:border-slate-700"><div class="min-w-0"><div class="mb-1.5 flex items-center gap-1.5"><span class="rounded-md bg-emerald-100 px-2 py-0.5 text-[10px] font-bold text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200">域名邮箱取码</span><span v-if="codeMailbox?.label" class="max-w-40 truncate rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-500 dark:bg-slate-800 dark:text-slate-300">{{ codeMailbox.label }}</span></div><h2 id="domain-mailbox-code-title" class="truncate text-base font-black">{{ codeMailbox?.email || codeResult?.email || '获取验证码' }}</h2><p class="mt-1 text-[11px] text-slate-400">同步最新邮件并提取验证码</p></div><button class="icon-button h-8 w-8 rounded-lg" title="关闭验证码弹窗" :disabled="codeDialogBusy" @click="closeCodeDialog"><X :size="17" /></button></header>
        <div class="mailbox-code-body p-5">
          <div v-if="codeDialogBusy" class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center"><span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-emerald-50 text-emerald-600 dark:bg-emerald-950/50 dark:text-emerald-300"><LoaderCircle v-if="codeBusyVisible" :size="24" class="animate-spin" /><KeyRound v-else :size="23" /></span><div><strong class="block text-sm">正在获取验证码</strong><span class="mt-1 block text-xs text-slate-400">正在同步并检查最新邮件，请稍候……</span></div></div>
          <div v-else-if="codeResult" class="mailbox-code-result space-y-4">
            <div class="mailbox-code-card rounded-2xl border border-emerald-200 bg-emerald-50 p-4 text-center dark:border-emerald-900 dark:bg-emerald-950/35"><div class="text-[11px] font-bold text-emerald-600 dark:text-emerald-300">最新验证码</div><button type="button" class="mailbox-code-value" title="点击复制验证码" :aria-label="`复制验证码 ${codeResult.code}`" @click="copyCode">{{ codeResult.code }}</button><div class="mt-2 truncate text-xs text-emerald-700/70 dark:text-emerald-300/70" :title="codeResult.subject">{{ codeResult.subject || '未提供邮件主题' }}</div></div>
            <div class="mailbox-code-stats grid grid-cols-2 gap-2 text-xs"><div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[10px] text-slate-400">收件数量</span><strong class="mt-0.5 block">{{ codeMailbox?.receive_count || 0 }} 封</strong></div><div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[10px] text-slate-400">收件时间</span><strong class="mt-0.5 block truncate" :title="formatTime(codeResult.received_at)">{{ formatTime(codeResult.received_at) }}</strong></div></div>
            <button class="primary-button mailbox-code-copy w-full" @click="copyCode"><Clipboard :size="15" />复制验证码</button>
          </div>
          <div v-else class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center"><span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-300"><KeyRound :size="23" /></span><div><strong class="block text-sm">暂未获取到验证码</strong><span class="mt-1 block max-w-sm text-xs leading-5 text-slate-400">{{ codeError || '请稍后重新取码。' }}</span></div><button class="secondary-button" @click="closeCodeDialog">关闭</button></div>
        </div>
      </section>
    </div>

    <div v-if="selectedMessage" class="fixed inset-0 z-[80] !m-0 flex items-center justify-center bg-slate-950/65 p-3 backdrop-blur-[4px] sm:p-5" role="presentation" @click.self="closeSelectedMessage">
      <article class="mail-message-dialog flex flex-col overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900" role="dialog" aria-modal="true" aria-labelledby="domain-mail-message-title">
        <header class="flex items-start justify-between gap-4 border-b border-slate-200 px-4 py-3.5 dark:border-slate-700 sm:px-5"><div class="min-w-0"><div class="mb-1.5 flex flex-wrap items-center gap-1.5"><span class="rounded-md bg-sky-100 px-2 py-0.5 text-[10px] font-bold text-sky-700 dark:bg-sky-950/60 dark:text-sky-200">完整邮件</span><span v-if="selectedMessage.source" class="rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-500 dark:bg-slate-800 dark:text-slate-300">{{ selectedMessage.source }}</span><span class="rounded-md bg-violet-100 px-2 py-0.5 text-[10px] font-bold text-violet-700 dark:bg-violet-950/60 dark:text-violet-200">{{ messageContentTypeLabel(selectedMessage) }}</span></div><h2 id="domain-mail-message-title" class="break-words text-base font-black leading-6">{{ selectedMessage.subject || '无主题' }}</h2><p class="mt-1 break-all text-[11px] text-slate-400">{{ selectedMessage.from || '未知发件人' }}</p></div><button class="icon-button h-8 w-8 rounded-lg" title="关闭完整邮件" @click="closeSelectedMessage"><X :size="17" /></button></header>
        <div class="mail-message-meta flex items-center justify-between gap-3 border-b border-slate-100 bg-slate-50 px-4 py-2 text-[10px] text-slate-400 dark:border-slate-800 dark:bg-slate-950/50 sm:px-5"><span class="truncate">收件邮箱：{{ selected?.email }}</span><div class="flex shrink-0 items-center gap-3"><div v-if="selectedMessageHasHTML" class="mail-message-view-switch"><button type="button" :class="{ active: messageViewMode !== 'text' }" @click="messageViewMode = 'html'">邮件视图</button><button type="button" :class="{ active: messageViewMode === 'text' }" @click="messageViewMode = 'text'">纯文本</button></div><time>{{ formatTime(selectedMessage.received_at) }}</time></div></div>
        <div class="mail-message-content relative min-h-0 flex-1 bg-slate-100 dark:bg-slate-950"><div v-if="selectedMessageLoading" class="mail-message-loading"><LoaderCircle :size="23" class="animate-spin" /><span>正在加载完整邮件</span></div><iframe v-if="showSelectedMessageHTML" class="mail-message-document" :srcdoc="selectedMessageHTMLDocument" sandbox="allow-popups allow-popups-to-escape-sandbox" title="HTML 邮件正文"></iframe><pre v-else class="mail-message-plain">{{ selectedMessage.body || selectedMessage.text_body || (selectedMessageLoading ? '正在加载完整邮件……' : '这封邮件没有正文内容。') }}</pre></div>
      </article>
    </div>
  </div>
</template>
