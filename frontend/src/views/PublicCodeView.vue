<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { AlertCircle, AtSign, Check, ChevronRight, ClipboardCopy, Cloud, Inbox, KeyRound, LoaderCircle, Mail, MailOpen, Moon, RefreshCw, Sun, X } from '@lucide/vue'
import { api } from '../api/client'
import { useToast } from '../composables/useToast'
import { domainRouteForEmail, loadDomainMailPreview, matchesDomainMailPreview, normalizeDomainMailPreview } from '../domainMailPreview'

const { success: showSuccess, error: showError } = useToast()

const email = ref('')
const enabled = ref(false)
const statusLoading = ref(true)
const dark = ref(false)
const domainConfig = ref(loadDomainMailPreview())

const codeDialogOpen = ref(false)
const codeEmail = ref('')
const codeLoading = ref(false)
const codeLoadingVisible = ref(false)
const codeResult = ref(null)
const codeError = ref('')
const copied = ref(false)

const mailLoading = ref(false)
const mailLoadingVisible = ref(false)
const mailError = ref('')
const messages = ref([])
const messagesTotal = ref(0)
const loadedEmail = ref('')
const lastSyncAt = ref('')

const selectedMessage = ref(null)
const selectedMessageLoading = ref(false)
const selectedMessageError = ref('')
const messageViewMode = ref('html')
let codeLoadingTimer
let mailLoadingTimer
let localRefreshTimer
let localRefreshRunning = false
let messageLoadRequestID = 0
const messageDetailCache = new Map()
const messageRequestCache = new Map()

const normalizedEmail = computed(() => email.value.trim().toLowerCase())
const emailReady = computed(() => /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalizedEmail.value))
const emailMailboxMeta = computed(() => mailboxMeta(normalizedEmail.value))
const selectedMessageHasHTML = computed(() => {
  const message = selectedMessage.value
  if (!message) return false
  return Boolean(message.has_html || String(message.html_body || '').trim() || String(message.content_type || '').toLowerCase().includes('text/html') || looksLikeHTML(message.body))
})
const selectedMessageHTML = computed(() => {
  const message = selectedMessage.value
  if (!message) return ''
  return String(message.html_body || (looksLikeHTML(message.body) ? message.body : '')).trim()
})
const selectedMessageHTMLDocument = computed(() => buildEmailHTMLDocument(selectedMessageHTML.value))
const selectedMessageDocumentKey = computed(() => `${selectedMessage.value?.id || 'message'}:${selectedMessageHTML.value.length}`)
const showSelectedMessageHTML = computed(() => selectedMessageHasHTML.value && Boolean(selectedMessageHTML.value) && messageViewMode.value === 'html')

function mailboxMeta(value) {
  const target = String(value || '').trim().toLowerCase()
  const domainRoute = domainRouteForEmail(target, domainConfig.value)
  if (matchesDomainMailPreview(target, domainConfig.value) && domainRoute) {
    return { label: '域名邮箱', description: `已匹配系统配置的接收域名 @${domainRoute.domain}`, kind: 'domain' }
  }
  return { label: 'iCloud 隐私邮箱', description: '从已绑定 Apple 账号查询', kind: 'icloud' }
}

function publicMailboxError(err, targetEmail) {
  if (err?.code === 'mailbox_not_found' && mailboxMeta(targetEmail).kind === 'domain') {
    return '该域名邮箱尚未在本地发现；收到第一封邮件后会自动建立。'
  }
  return err.message
}

function applyTheme(value) {
  dark.value = value
  document.documentElement.classList.toggle('dark', value)
  localStorage.setItem('ipm_v2_theme', value ? 'dark' : 'light')
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '-'
  return date.toLocaleString('zh-CN', { hour12: false })
}

function formatMessageTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '-'
  const options = date.getFullYear() === new Date().getFullYear()
    ? { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }
    : { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }
  return date.toLocaleString('zh-CN', options)
}

function looksLikeHTML(value) {
  return /<(?:!doctype|html|head|body|style|table|div|p|a|img|span)\b/i.test(String(value || ''))
}

function messageContentTypeLabel(message) {
  const contentType = String(message?.content_type || '').toLowerCase()
  if (message?.has_html || String(message?.html_body || '').trim() || contentType.includes('text/html') || looksLikeHTML(message?.body)) return 'HTML 邮件'
  if (contentType.includes('text/plain')) return '纯文本邮件'
  return '邮件正文'
}

function shrinkEmailFontSizes(value) {
  return String(value || '').replace(/(font-size\s*:\s*)(\d+(?:\.\d+)?)px/gi, (match, prefix, rawSize) => {
    const size = Number(rawSize)
    if (!Number.isFinite(size) || size < 12) return match
    return `${prefix}${Math.max(11, Math.round(size * 0.82))}px`
  })
}

function buildEmailHTMLDocument(value) {
  if (!value || typeof DOMParser === 'undefined') return ''
  const documentNode = new DOMParser().parseFromString(value, 'text/html')
  documentNode.querySelectorAll('script, iframe, object, embed, form, input, button, textarea, select, base, meta[http-equiv="refresh"], link[rel="stylesheet"], link[rel="preload"], link[rel="preconnect"], link[rel="dns-prefetch"]').forEach((element) => element.remove())
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
  documentNode.querySelectorAll('style').forEach((element) => {
    element.textContent = shrinkEmailFontSizes(element.textContent).replace(/@font-face\s*\{[^}]*\}/gi, '')
  })
  documentNode.querySelectorAll('[style]').forEach((element) => {
    element.setAttribute('style', shrinkEmailFontSizes(element.getAttribute('style')))
  })
  documentNode.querySelectorAll('img[src]').forEach((element) => {
    const source = String(element.getAttribute('src') || '')
    const width = Number.parseInt(element.getAttribute('width') || '', 10)
    const height = Number.parseInt(element.getAttribute('height') || '', 10)
    if ((width > 0 && width <= 2 && height > 0 && height <= 2) || /\/(?:wf\/open|pixel|track)(?:[/?]|$)/i.test(source)) {
      element.remove()
      return
    }
    element.setAttribute('loading', 'lazy')
    element.setAttribute('decoding', 'async')
    element.setAttribute('referrerpolicy', 'no-referrer')
  })
  const policy = documentNode.createElement('meta')
  policy.setAttribute('http-equiv', 'Content-Security-Policy')
  policy.setAttribute('content', "default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline'; font-src data:; media-src 'none'; script-src 'none'; object-src 'none'; frame-src 'none'; form-action 'none'")
  const viewport = documentNode.createElement('meta')
  viewport.setAttribute('name', 'viewport')
  viewport.setAttribute('content', 'width=device-width, initial-scale=1')
  const readerStyle = documentNode.createElement('style')
  readerStyle.textContent = 'html{color-scheme:light;background:#fff;scrollbar-width:thin;scrollbar-color:#cbd5e1 transparent}body{box-sizing:border-box;min-height:100%;margin:0;padding:24px;color:#172033;background:#fff;font-size:13px;opacity:1!important;visibility:visible!important;overflow-wrap:anywhere}::-webkit-scrollbar{width:8px;height:8px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{border:2px solid transparent;border-radius:9999px;background:#cbd5e1;background-clip:content-box}img{max-width:100%;height:auto}table{max-width:100%}pre{white-space:pre-wrap}a{color:#0a6cff}*,*::before,*::after{animation:none!important;transition:none!important}'
  documentNode.head.prepend(policy, viewport)
  documentNode.head.append(readerStyle)
  return `<!doctype html>\n${documentNode.documentElement.outerHTML}`
}

async function loadStatus() {
  statusLoading.value = true
  try {
    const data = await api('/api/v1/public-code/status')
    enabled.value = Boolean(data.enabled)
    domainConfig.value = normalizeDomainMailPreview(data.domain_mail || {})
  } catch (err) {
    enabled.value = false
    mailError.value = err.message
  } finally {
    statusLoading.value = false
  }
}

async function getCode() {
  if (!emailReady.value || !enabled.value || codeLoading.value) return
  codeEmail.value = normalizedEmail.value
  codeDialogOpen.value = true
  codeLoading.value = true
  codeLoadingVisible.value = false
  codeResult.value = null
  codeError.value = ''
  copied.value = false
  clearTimeout(codeLoadingTimer)
  codeLoadingTimer = window.setTimeout(() => {
    if (codeLoading.value) codeLoadingVisible.value = true
  }, 500)
  try {
    codeResult.value = await api(`/api/v1/public-code?email=${encodeURIComponent(codeEmail.value)}&wait_ms=15000`)
  } catch (err) {
    codeError.value = publicMailboxError(err, codeEmail.value)
  } finally {
    await refreshLocalMessages(codeEmail.value)
    clearTimeout(codeLoadingTimer)
    codeLoading.value = false
    codeLoadingVisible.value = false
  }
}

function applyMessageList(data, requestEmail) {
  if (loadedEmail.value !== requestEmail) return
  messages.value = data.items || []
  messagesTotal.value = Number(data.total || messages.value.length)
  lastSyncAt.value = data.last_sync_at || ''
  if (data.sync_error) mailError.value = data.sync_error
  window.setTimeout(() => messages.value.slice(0, 6).forEach(prefetchMessage), 0)
}

async function refreshLocalMessages(targetEmail = loadedEmail.value) {
  const requestEmail = String(targetEmail || '').trim().toLowerCase()
  if (!requestEmail || localRefreshRunning) return
  localRefreshRunning = true
  if (loadedEmail.value !== requestEmail) {
    loadedEmail.value = requestEmail
    messages.value = []
    messagesTotal.value = 0
    messageDetailCache.clear()
    messageRequestCache.clear()
  }
  try {
    const data = await api(`/api/v1/public-code/messages?email=${encodeURIComponent(requestEmail)}&limit=50`)
    applyMessageList(data, requestEmail)
  } catch {
    // 静默刷新失败时保留当前列表，下次轮询继续读取本地缓存。
  } finally {
    localRefreshRunning = false
  }
}

async function getMessages() {
  if (!emailReady.value || !enabled.value || mailLoading.value) return
  const requestEmail = normalizedEmail.value
  if (loadedEmail.value !== requestEmail) {
    messages.value = []
    messagesTotal.value = 0
    messageDetailCache.clear()
    messageRequestCache.clear()
  }
  loadedEmail.value = requestEmail
  mailLoading.value = true
  mailLoadingVisible.value = false
  mailError.value = ''
  clearTimeout(mailLoadingTimer)
  mailLoadingTimer = window.setTimeout(() => {
    if (mailLoading.value) mailLoadingVisible.value = true
  }, 350)
  try {
    const data = await api(`/api/v1/public-code/messages?email=${encodeURIComponent(requestEmail)}&sync=1&limit=50`)
    applyMessageList(data, requestEmail)
    mailError.value = data.sync_error || ''
  } catch (err) {
    mailError.value = publicMailboxError(err, requestEmail)
  } finally {
    clearTimeout(mailLoadingTimer)
    mailLoading.value = false
    mailLoadingVisible.value = false
  }
}

function messageCacheKey(item, targetEmail = loadedEmail.value) {
  return `${targetEmail}:${item?.id || ''}`
}

async function loadMessageDetail(item, targetEmail = loadedEmail.value) {
  const key = messageCacheKey(item, targetEmail)
  if (messageDetailCache.has(key)) return messageDetailCache.get(key)
  if (messageRequestCache.has(key)) return messageRequestCache.get(key)
  const request = api(`/api/v1/public-code/messages/${encodeURIComponent(item.id)}?email=${encodeURIComponent(targetEmail)}`)
    .then((data) => {
      const detail = data.message || item
      messageDetailCache.set(key, detail)
      return detail
    })
    .finally(() => messageRequestCache.delete(key))
  messageRequestCache.set(key, request)
  return request
}

function prefetchMessage(item) {
  if (!item?.id || !loadedEmail.value) return
  loadMessageDetail(item).catch(() => {})
}

async function copyCode() {
  if (!codeResult.value?.code) return
  try {
    await navigator.clipboard.writeText(codeResult.value.code)
    copied.value = true
    showSuccess('验证码已复制', 2500)
    window.setTimeout(() => { copied.value = false }, 1800)
  } catch {
    showError('复制验证码失败，请重试')
  }
}

function closeCodeDialog() {
  if (codeLoading.value) return
  codeDialogOpen.value = false
  codeEmail.value = ''
  codeResult.value = null
  codeError.value = ''
  copied.value = false
}

async function openMessage(item) {
  if (!item?.id || !loadedEmail.value) return
  const requestID = ++messageLoadRequestID
  const targetEmail = loadedEmail.value
  const cached = messageDetailCache.get(messageCacheKey(item, targetEmail))
  selectedMessage.value = cached || item
  selectedMessageLoading.value = !cached
  selectedMessageError.value = ''
  messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
  if (cached) return
  try {
    const detail = await loadMessageDetail(item, targetEmail)
    if (requestID !== messageLoadRequestID || selectedMessage.value?.id !== item.id) return
    selectedMessage.value = detail
    messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
  } catch (err) {
    if (requestID === messageLoadRequestID) selectedMessageError.value = err.message
  } finally {
    if (requestID === messageLoadRequestID) selectedMessageLoading.value = false
  }
}

function closeSelectedMessage() {
  messageLoadRequestID++
  selectedMessage.value = null
  selectedMessageLoading.value = false
  selectedMessageError.value = ''
}

function handleEscape(event) {
  if (event.key !== 'Escape') return
  if (selectedMessage.value) closeSelectedMessage()
  else if (codeDialogOpen.value) closeCodeDialog()
}

onMounted(() => {
  domainConfig.value = loadDomainMailPreview()
  const saved = localStorage.getItem('ipm_v2_theme')
  applyTheme(saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches)
  const presetEmail = new URLSearchParams(window.location.search).get('email')
  if (presetEmail) email.value = presetEmail
  window.addEventListener('keydown', handleEscape)
  localRefreshTimer = window.setInterval(() => {
    if (!document.hidden && loadedEmail.value && !mailLoading.value) void refreshLocalMessages()
  }, 10000)
  loadStatus()
})

onBeforeUnmount(() => {
  clearTimeout(codeLoadingTimer)
  clearTimeout(mailLoadingTimer)
  clearInterval(localRefreshTimer)
  window.removeEventListener('keydown', handleEscape)
})
</script>

<template>
  <main class="relative min-h-screen overflow-hidden bg-slate-50 text-slate-800 transition-colors dark:bg-slate-950 dark:text-slate-100">
    <div class="pointer-events-none absolute inset-0 overflow-hidden"><div class="absolute -left-36 -top-48 h-[34rem] w-[34rem] rounded-full bg-sky-200/45 blur-3xl dark:bg-sky-900/20" /><div class="absolute -bottom-52 -right-36 h-[38rem] w-[38rem] rounded-full bg-indigo-200/35 blur-3xl dark:bg-indigo-950/20" /></div>
    <div class="relative mx-auto flex min-h-screen w-full max-w-5xl flex-col px-4 py-5 sm:px-6 sm:py-6">
      <header class="flex items-center justify-between gap-4"><div class="flex items-center gap-3"><span class="flex h-10 w-10 items-center justify-center rounded-xl bg-sky-500 text-white shadow-lg shadow-sky-500/20"><Cloud :size="21" /></span><div><strong class="block text-sm font-black text-slate-900 dark:text-white">获取验证码与邮件</strong><span class="text-[10px] text-slate-400">支持隐私邮箱、收件邮箱和域名邮箱</span></div></div><div class="flex items-center gap-2"><span v-if="statusLoading" class="flex h-8 w-8 items-center justify-center text-slate-400"><LoaderCircle :size="16" class="animate-spin" /></span><span v-else :class="enabled ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-300' : 'bg-slate-200 text-slate-500 dark:bg-slate-800 dark:text-slate-300'" class="rounded-full px-2.5 py-1 text-[10px] font-bold">{{ enabled ? '服务已开启' : '服务未开启' }}</span><button type="button" class="icon-button h-8 w-8 rounded-lg border border-slate-200 bg-white shadow-sm dark:border-slate-700 dark:bg-slate-800" :title="dark ? '切换亮色主题' : '切换暗色主题'" @click="applyTheme(!dark)"><Sun v-if="dark" :size="15" /><Moon v-else :size="15" /></button></div></header>

      <section class="mt-6 overflow-hidden rounded-2xl border border-white/90 bg-white/90 shadow-xl shadow-slate-200/50 backdrop-blur-xl dark:border-slate-800 dark:bg-slate-900/90 dark:shadow-black/20">
        <form class="flex flex-col gap-3 px-5 py-4 sm:flex-row sm:items-end sm:px-6" @submit.prevent="getCode"><label class="form-group min-w-0 flex-1"><span class="form-label public-code-form-label"><span>邮箱地址</span><em v-if="emailReady" class="public-mailbox-type" :class="`is-${emailMailboxMeta.kind}`"><AtSign v-if="emailMailboxMeta.kind === 'domain'" :size="11" /><Mail v-else :size="11" />{{ emailMailboxMeta.label }}</em></span><span class="field-wrap"><Mail :size="16" class="field-icon" /><input v-model.trim="email" class="field field-leading public-code-email-field" type="email" inputmode="email" autocomplete="email" spellcheck="false" placeholder="name@icloud.com 或 user@example.com" :disabled="statusLoading || !enabled" required /></span></label><div class="flex shrink-0 gap-2"><button type="button" class="secondary-button h-10 min-w-[112px]" :disabled="statusLoading || !enabled || !emailReady || mailLoading" @click="getMessages"><LoaderCircle v-if="mailLoadingVisible" :size="15" class="animate-spin" /><Inbox v-else :size="15" />{{ mailLoadingVisible ? '正在同步' : '获取邮件' }}</button><button class="primary-button h-10 min-w-[112px]" :disabled="statusLoading || !enabled || !emailReady || codeLoading"><KeyRound :size="15" />获取验证码</button></div></form>
      </section>

      <section class="mt-4 overflow-hidden rounded-2xl border border-white/90 bg-white/90 shadow-lg shadow-slate-200/40 backdrop-blur-xl dark:border-slate-800 dark:bg-slate-900/90 dark:shadow-black/20">
        <header class="flex items-center justify-between gap-3 border-b border-slate-100 px-5 py-3.5 dark:border-slate-800 sm:px-6"><div class="flex min-w-0 items-center gap-2.5"><span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/60 dark:text-sky-300"><Inbox :size="15" /></span><div class="min-w-0"><h2 class="text-xs font-black">邮件列表 <span class="ml-1 text-slate-400">{{ messages.length ? `${messages.length}${messagesTotal > messages.length ? ` / ${messagesTotal}` : ''}` : '' }}</span></h2><p class="mt-0.5 truncate text-[9px] text-slate-400">{{ loadedEmail || '获取邮件后会显示在这里' }}</p></div></div><div class="flex shrink-0 items-center gap-2"><time v-if="lastSyncAt" class="hidden text-[9px] text-slate-400 sm:inline">同步于 {{ formatTime(lastSyncAt) }}</time><button type="button" class="icon-button h-8 w-8 rounded-lg" title="刷新邮件列表" :disabled="statusLoading || !enabled || !emailReady || mailLoading" @click="getMessages"><RefreshCw :size="14" :class="{ 'animate-spin': mailLoadingVisible }" /></button></div></header>
        <div v-if="mailError" class="mx-5 mt-3 flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-[10px] leading-4 text-amber-700 dark:border-amber-900 dark:bg-amber-950/35 dark:text-amber-300 sm:mx-6"><AlertCircle :size="14" class="mt-0.5 shrink-0" /><span>{{ mailError }}</span></div>
        <div v-if="mailLoading && !messages.length" class="public-code-message-state flex flex-col items-center justify-center gap-2 text-center"><LoaderCircle v-if="mailLoadingVisible" :size="21" class="animate-spin text-sky-500" /><Inbox v-else :size="21" class="text-slate-300" /><strong class="text-xs text-slate-500 dark:text-slate-300">正在同步最新邮件</strong><span class="text-[10px] text-slate-400">请稍候，邮件会直接显示在列表中。</span></div>
        <div v-else-if="messages.length" class="mail-message-list public-code-message-list m-3 sm:m-4"><button v-for="item in messages" :key="item.id" type="button" class="mail-message-row" :title="`查看完整邮件：${item.subject || '无主题'}`" @mouseenter="prefetchMessage(item)" @focus="prefetchMessage(item)" @click="openMessage(item)"><span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/50 dark:text-sky-300"><MailOpen :size="16" /></span><span class="min-w-0 flex-1 text-left"><strong class="block truncate text-[11px] font-semibold">{{ item.subject || '无主题' }}</strong><small class="mt-0.5 block truncate text-[9px] text-slate-400">{{ item.from || '未知发件人' }}</small></span><span class="flex shrink-0 items-center gap-2"><em class="hidden rounded-md bg-slate-100 px-1.5 py-0.5 text-[8px] font-semibold not-italic text-slate-400 dark:bg-slate-800 sm:inline">{{ messageContentTypeLabel(item) }}</em><time class="text-[9px] text-slate-400">{{ formatMessageTime(item.received_at) }}</time><ChevronRight :size="14" class="text-slate-300 dark:text-slate-600" /></span></button></div>
        <div v-else class="public-code-message-state flex flex-col items-center justify-center px-5 text-center"><span class="flex h-11 w-11 items-center justify-center rounded-xl bg-slate-100 text-slate-400 dark:bg-slate-800"><Mail :size="20" /></span><strong class="mt-3 text-xs text-slate-600 dark:text-slate-300">{{ loadedEmail ? '当前邮箱暂无本地邮件' : '等待获取邮件' }}</strong><p class="mt-1.5 text-[10px] leading-4 text-slate-400">{{ loadedEmail ? '可以点击右上角按钮重新同步。' : '输入邮箱并点击“获取邮件”。' }}</p></div>
      </section>
      <footer class="py-4 text-center text-[10px] text-slate-400">只查询当前输入邮箱；域名邮箱按邮件原始收件人识别</footer>
    </div>

    <div v-if="codeDialogOpen" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/65 p-4 backdrop-blur-[4px]" role="presentation" @click.self="closeCodeDialog"><section class="panel mailbox-code-dialog overflow-hidden p-0 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="public-code-title"><header class="mailbox-code-heading flex items-start justify-between gap-4 border-b border-slate-200 px-5 py-4 dark:border-slate-700"><div class="min-w-0"><div class="mb-1.5 flex items-center gap-1.5"><span class="rounded-md bg-emerald-100 px-2 py-0.5 text-[10px] font-bold text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200">邮箱取码</span></div><h2 id="public-code-title" class="truncate text-sm font-black">{{ codeEmail || '获取验证码' }}</h2><p class="mt-1 text-[10px] text-slate-400">同步最新邮件并提取验证码</p></div><button class="icon-button h-8 w-8 rounded-lg" title="关闭验证码弹窗" :disabled="codeLoading" @click="closeCodeDialog"><X :size="16" /></button></header><div class="mailbox-code-body p-5"><div v-if="codeLoading" class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center"><span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-emerald-50 text-emerald-600 dark:bg-emerald-950/50 dark:text-emerald-300"><LoaderCircle v-if="codeLoadingVisible" :size="24" class="animate-spin" /><KeyRound v-else :size="23" /></span><div><strong class="block text-sm">正在获取验证码</strong><span class="mt-1 block text-xs text-slate-400">正在同步并检查最新邮件，请稍候……</span></div></div><div v-else-if="codeResult" class="mailbox-code-result space-y-4"><div class="mailbox-code-card rounded-2xl border border-emerald-200 bg-emerald-50 p-4 text-center dark:border-emerald-900 dark:bg-emerald-950/35"><div class="text-[10px] font-bold text-emerald-600 dark:text-emerald-300">最新验证码</div><button type="button" class="mailbox-code-value" title="点击复制验证码" @click="copyCode">{{ codeResult.code }}</button><div class="mt-2 truncate text-[10px] text-emerald-700/70 dark:text-emerald-300/70" :title="codeResult.subject">{{ codeResult.subject || '未提供邮件主题' }}</div></div><div class="grid grid-cols-2 gap-2 text-[10px]"><div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[9px] text-slate-400">发件人</span><strong class="mt-0.5 block truncate" :title="codeResult.from">{{ codeResult.from || '-' }}</strong></div><div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[9px] text-slate-400">收件时间</span><strong class="mt-0.5 block truncate">{{ formatTime(codeResult.received_at) }}</strong></div></div><button class="primary-button w-full" @click="copyCode"><Check v-if="copied" :size="15" /><ClipboardCopy v-else :size="15" />{{ copied ? '已复制' : '复制验证码' }}</button></div><div v-else class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center"><span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-300"><KeyRound :size="23" /></span><div><strong class="block text-sm">暂未获取到验证码</strong><span class="mt-1 block max-w-sm text-xs leading-5 text-slate-400">{{ codeError || '请稍后重新取码。' }}</span></div><button class="secondary-button" @click="closeCodeDialog">关闭</button></div></div></section></div>

    <div v-if="selectedMessage" class="fixed inset-0 z-[80] !m-0 flex items-center justify-center bg-slate-950/65 p-3 backdrop-blur-[4px] sm:p-5" role="presentation" @click.self="closeSelectedMessage"><article class="mail-message-dialog flex flex-col overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900" role="dialog" aria-modal="true" aria-labelledby="public-message-title"><header class="flex items-start justify-between gap-4 border-b border-slate-200 px-4 py-3.5 dark:border-slate-700 sm:px-5"><div class="min-w-0"><div class="mb-1.5 flex flex-wrap items-center gap-1.5"><span class="rounded-md bg-sky-100 px-2 py-0.5 text-[10px] font-bold text-sky-700 dark:bg-sky-950/60 dark:text-sky-200">完整邮件</span><span v-if="selectedMessage.source" class="rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-500 dark:bg-slate-800 dark:text-slate-300">{{ selectedMessage.source }}</span><span class="rounded-md bg-violet-100 px-2 py-0.5 text-[10px] font-bold text-violet-700 dark:bg-violet-950/60 dark:text-violet-200">{{ messageContentTypeLabel(selectedMessage) }}</span></div><h2 id="public-message-title" class="break-words text-base font-black leading-6">{{ selectedMessage.subject || '无主题' }}</h2><p class="mt-1 break-all text-[11px] text-slate-400">{{ selectedMessage.from || '未知发件人' }}</p></div><button class="icon-button h-8 w-8 rounded-lg" title="关闭完整邮件" @click="closeSelectedMessage"><X :size="17" /></button></header><div class="mail-message-meta flex items-center justify-between gap-3 border-b border-slate-100 bg-slate-50 px-4 py-2 text-[10px] text-slate-400 dark:border-slate-800 dark:bg-slate-950/50 sm:px-5"><span class="truncate">收件邮箱：{{ loadedEmail }}</span><div class="flex shrink-0 items-center gap-3"><div v-if="selectedMessageHasHTML" class="mail-message-view-switch"><button type="button" :class="{ active: messageViewMode === 'html' }" @click="messageViewMode = 'html'">邮件视图</button><button type="button" :class="{ active: messageViewMode === 'text' }" @click="messageViewMode = 'text'">纯文本</button></div><time>{{ formatTime(selectedMessage.received_at) }}</time></div></div><div class="mail-message-content relative min-h-0 flex-1 bg-slate-100 dark:bg-slate-950"><div v-if="selectedMessageLoading" class="mail-message-loading"><LoaderCircle :size="23" class="animate-spin" /><span>正在加载完整邮件</span></div><div v-else-if="selectedMessageError" class="mail-message-loading"><AlertCircle :size="23" class="text-rose-500" /><span>{{ selectedMessageError }}</span></div><iframe v-if="showSelectedMessageHTML" :key="selectedMessageDocumentKey" class="mail-message-document" :srcdoc="selectedMessageHTMLDocument" sandbox="allow-popups allow-popups-to-escape-sandbox" title="HTML 邮件正文"></iframe><pre v-else class="mail-message-plain">{{ selectedMessage.body || (selectedMessageLoading ? '正在加载完整邮件……' : '这封邮件没有正文内容。') }}</pre></div></article></div>
  </main>
</template>
