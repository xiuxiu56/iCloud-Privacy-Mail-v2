import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

function previewData(path) {
  if (path.startsWith('/auth/status')) return { authenticated: true, setup_required: false, admin: { username: '预览管理员' } }
  if (path === '/apple-accounts') return {
    items: [
      { id: 'account-1', apple_id: 'first@icloud.com', imap_saved: true },
      { id: 'account-2', apple_id: 'second@icloud.com', imap_saved: true },
    ],
  }
  if (path === '/apple-accounts/account-1') return { account: { id: 'account-1', apple_id: 'first@icloud.com', imap_email: 'first@icloud.com' } }
  if (path === '/apple-accounts/account-2') return { account: { id: 'account-2', apple_id: 'second@icloud.com', imap_email: 'second@icloud.com' } }
  if (path === '/settings') return { settings: {}, runtime: {} }
  return {}
}

export default defineConfig({
  plugins: [{
    name: 'preview-api',
    configureServer(server) {
      server.middlewares.use('/api', (request, response) => {
        response.setHeader('Content-Type', 'application/json; charset=utf-8')
        response.end(JSON.stringify({ success: true, data: previewData(request.url || '') }))
      })
    },
  }, vue(), tailwindcss()],
  server: { host: '127.0.0.1', port: 18788 },
})
