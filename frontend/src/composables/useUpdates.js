import { computed, reactive, ref } from 'vue'
import { api } from '../api/client'

const readStorageKey = 'ipm_v2_read_announcements'
const updateState = reactive({
  loaded: false,
  loading: false,
  status: null,
  error: '',
})
const showChecking = ref(false)
const readAnnouncementIDs = ref(loadReadAnnouncementIDs())
let loadingPromise = null
let checkingTimer = null

function loadReadAnnouncementIDs() {
  try {
    const value = JSON.parse(localStorage.getItem(readStorageKey) || '[]')
    return Array.isArray(value) ? value.filter((item) => typeof item === 'string').slice(-200) : []
  } catch {
    return []
  }
}

function saveReadAnnouncementIDs() {
  localStorage.setItem(readStorageKey, JSON.stringify(readAnnouncementIDs.value.slice(-200)))
}

const announcements = computed(() => Array.isArray(updateState.status?.announcements) ? updateState.status.announcements : [])
const unreadAnnouncements = computed(() => {
  const read = new Set(readAnnouncementIDs.value)
  return announcements.value.filter((item) => item?.id && !read.has(item.id))
})
const currentVersion = computed(() => updateState.status?.current?.version || '2.2.3')
const currentCommit = computed(() => updateState.status?.current?.commit || 'unknown')

async function loadUpdates(force = false) {
  if (loadingPromise) return loadingPromise
  if (updateState.loaded && !force) return updateState.status

  updateState.loading = true
  updateState.error = ''
  showChecking.value = false
  checkingTimer = window.setTimeout(() => { showChecking.value = true }, 600)

  loadingPromise = api(`/api/update/status${force ? '?force=1' : ''}`)
    .then((status) => {
      updateState.status = status || null
      updateState.error = String(status?.error || '')
      updateState.loaded = true
      return updateState.status
    })
    .catch((error) => {
      updateState.error = error.message
      throw error
    })
    .finally(() => {
      if (checkingTimer) window.clearTimeout(checkingTimer)
      checkingTimer = null
      showChecking.value = false
      updateState.loading = false
      loadingPromise = null
    })

  return loadingPromise
}

function markAnnouncementRead(id) {
  const value = String(id || '').trim()
  if (!value || readAnnouncementIDs.value.includes(value)) return
  readAnnouncementIDs.value = [...readAnnouncementIDs.value, value].slice(-200)
  saveReadAnnouncementIDs()
}

function markAllAnnouncementsRead() {
  const ids = announcements.value.map((item) => String(item?.id || '').trim()).filter(Boolean)
  readAnnouncementIDs.value = [...new Set([...readAnnouncementIDs.value, ...ids])].slice(-200)
  saveReadAnnouncementIDs()
}

export function useUpdates() {
  return {
    updateState,
    showChecking,
    announcements,
    unreadAnnouncements,
    currentVersion,
    currentCommit,
    loadUpdates,
    markAnnouncementRead,
    markAllAnnouncementsRead,
  }
}
