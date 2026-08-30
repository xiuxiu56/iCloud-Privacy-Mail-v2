export function defaultDomainMailPreview() {
  return {
    enabled: false,
    domains: [],
    routes: [],
    active_domain: '',
    receiver_type: 'apple_account',
    account_id: '',
    receiver_label: '',
    imap_host: '',
    imap_port: 993,
    imap_username: '',
    imap_tls: true,
    forward_to_email: '',
    match_mode: 'catch_all',
    auto_discover: true,
    default_api_active: true,
  }
}

export function normalizeDomain(value) {
  return String(value || '').trim().toLowerCase().replace(/^@+/, '').replace(/\.+$/, '')
}

export function normalizeDomains(value) {
  const items = Array.isArray(value) ? value : String(value || '').split(/[,\s;，；]+/)
  return [...new Set(items.map(normalizeDomain).filter(Boolean))]
}

export function normalizeDomainRoutes(value, fallbackDomains = [], fallbackReceiver = {}) {
  const sourceRoutes = Array.isArray(value) ? value : []
  const domains = normalizeDomains(fallbackDomains?.length ? fallbackDomains : sourceRoutes.map((item) => item?.domain))
  const routeByDomain = new Map(sourceRoutes.map((item) => [normalizeDomain(item?.domain), item]))
  return domains.map((domain) => {
    const route = routeByDomain.get(domain) || {}
    const receiverType = route.receiver_type === 'custom_imap'
      ? 'custom_imap'
      : (route.receiver_type === 'apple_account' ? 'apple_account' : (fallbackReceiver.receiver_type === 'custom_imap' ? 'custom_imap' : 'apple_account'))
    return {
	  id: String(route.id || '').trim(),
      domain,
      receiver_type: receiverType,
      account_id: String(route.account_id ?? fallbackReceiver.account_id ?? '').trim(),
      receiver_label: String(route.receiver_label ?? fallbackReceiver.receiver_label ?? '').trim(),
      imap_host: String(route.imap_host ?? fallbackReceiver.imap_host ?? '').trim().toLowerCase(),
      imap_port: Math.min(65535, Math.max(1, Number(route.imap_port ?? fallbackReceiver.imap_port) || 993)),
      imap_username: String(route.imap_username ?? fallbackReceiver.imap_username ?? '').trim(),
      imap_tls: Boolean(route.imap_tls ?? fallbackReceiver.imap_tls ?? true),
      forward_to_email: String(route.forward_to_email ?? fallbackReceiver.forward_to_email ?? '').trim().toLowerCase(),
	  imap_password_configured: Boolean(route.imap_password_configured),
    }
  })
}

export function normalizeDomainMailPreview(value = {}) {
  const fallback = defaultDomainMailPreview()
  const domains = normalizeDomains(value.domains?.length ? value.domains : value.domain)
  const routes = normalizeDomainRoutes(value.routes, domains, value)
  const requestedActiveDomain = normalizeDomain(value.active_domain)
  const activeDomain = domains.includes(requestedActiveDomain) ? requestedActiveDomain : (domains[0] || '')
  const activeRoute = routes.find((item) => item.domain === activeDomain)
  return {
    ...fallback,
    ...value,
    domains,
    routes,
    active_domain: activeDomain,
    domain: domains[0] || '',
    receiver_type: activeRoute?.receiver_type || (value.receiver_type === 'custom_imap' ? 'custom_imap' : 'apple_account'),
    account_id: String(activeRoute?.account_id ?? value.account_id ?? fallback.account_id).trim(),
    receiver_label: String(activeRoute?.receiver_label ?? value.receiver_label ?? fallback.receiver_label).trim(),
    imap_host: String(activeRoute?.imap_host ?? value.imap_host ?? fallback.imap_host).trim().toLowerCase(),
    imap_port: Math.min(65535, Math.max(1, Number(activeRoute?.imap_port ?? value.imap_port ?? fallback.imap_port) || 993)),
    imap_username: String(activeRoute?.imap_username ?? value.imap_username ?? fallback.imap_username).trim(),
    imap_tls: Boolean(activeRoute?.imap_tls ?? value.imap_tls ?? fallback.imap_tls),
    forward_to_email: String(activeRoute?.forward_to_email ?? value.forward_to_email ?? fallback.forward_to_email).trim().toLowerCase(),
    match_mode: value.match_mode === 'registered_only' ? 'registered_only' : 'catch_all',
    enabled: Boolean(value.enabled ?? fallback.enabled),
    auto_discover: Boolean(value.auto_discover ?? fallback.auto_discover),
    default_api_active: Boolean(value.default_api_active ?? fallback.default_api_active),
  }
}

export function loadDomainMailPreview() {
  return defaultDomainMailPreview()
}

export function saveDomainMailPreview(value) {
  return normalizeDomainMailPreview(value)
}

export function emailDomain(value) {
  const email = String(value || '').trim().toLowerCase()
  const at = email.lastIndexOf('@')
  return at >= 0 ? normalizeDomain(email.slice(at + 1)) : ''
}

export function domainRouteForEmail(email, config = loadDomainMailPreview()) {
  const normalized = normalizeDomainMailPreview(config)
  const domain = emailDomain(email)
  return normalized.routes.find((item) => item.domain === domain) || null
}

export function matchesDomainMailPreview(email, config = loadDomainMailPreview()) {
  const normalized = normalizeDomainMailPreview(config)
  return normalized.enabled && normalized.routes.some((item) => item.domain === emailDomain(email))
}
