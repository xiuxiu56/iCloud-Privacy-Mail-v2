<script setup>
import { computed } from 'vue'
import { Activity, Apple, AtSign, Boxes, ChevronRight, Cloud, Download, LayoutDashboard, MailPlus, Settings, X } from '@lucide/vue'
import ThemeToggle from './ThemeToggle.vue'
import { useUpdates } from '../composables/useUpdates'

const props = defineProps({
  open: Boolean,
  dark: Boolean,
})

const emit = defineEmits(['close', 'toggle-theme'])
const { currentVersion, currentCommit } = useUpdates()

const versionText = computed(() => {
  const value = String(currentVersion.value || '2.2.1').trim()
  return /^v/i.test(value) ? value : `v${value}`
})
const commitText = computed(() => {
  const value = String(currentCommit.value || '').trim()
  return value && value !== 'unknown' ? value.slice(0, 7) : ''
})

const items = [
  { name: 'dashboard', label: '控制台', icon: LayoutDashboard },
  { name: 'apple-accounts', label: 'Apple 账号', icon: Apple },
  { name: 'mailboxes', label: '邮箱池', icon: Boxes },
  { name: 'domain-mailboxes', label: '域名邮箱', icon: AtSign },
  { name: 'tasks', label: '创建隐私邮箱', icon: MailPlus },
  { name: 'exports', label: '本地导出', icon: Download },
  { name: 'settings', label: '系统设置', icon: Settings },
]

const drawerClass = computed(() => props.open ? 'sidebar-open' : 'sidebar-closed')
</script>

<template>
  <div v-if="open" class="sidebar-backdrop" role="presentation" @click="emit('close')" />
  <aside :class="drawerClass" class="sidebar">
    <div class="brand">
      <RouterLink to="/" class="brand-link" @click="emit('close')">
        <div class="brand-icon">
          <Cloud :size="21" />
        </div>
        <div class="brand-copy">
          <strong>Privacy Mail</strong>
          <span>本地控制台</span>
        </div>
      </RouterLink>
      <button class="sidebar-close" title="关闭导航" aria-label="关闭导航" @click="emit('close')"><X :size="18" /></button>
    </div>

    <nav class="sidebar-nav">
      <div class="nav-group-title">工作区</div>
      <RouterLink v-for="item in items" :key="item.name" :to="{ name: item.name }" class="nav-item" @click="emit('close')">
        <component :is="item.icon" :size="18" />
        <span>{{ item.label }}</span>
        <ChevronRight :size="14" class="nav-chevron" />
      </RouterLink>
    </nav>

    <div class="sidebar-footer">
      <div class="footer-theme-row">
        <div class="footer-service">
          <div class="online-row"><i />本地服务正常</div>
          <RouterLink :to="{ name: 'settings', hash: '#version-updates' }" class="version-link" :title="`当前版本 ${versionText}${commitText ? `，提交 ${currentCommit}` : ''}`" @click="emit('close')">
            <span>版本 {{ versionText }}</span><span v-if="commitText" class="font-mono">· {{ commitText }}</span>
          </RouterLink>
        </div>
        <ThemeToggle :dark="dark" @toggle="emit('toggle-theme')" />
      </div>
    </div>
  </aside>
</template>
