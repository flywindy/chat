// Runtime config: window.__APP_CONFIG__ in prod (rendered by nginx envsubst),
// import.meta.env.VITE_* in `vite dev`, literal defaults as last resort.

const runtime = (typeof window !== 'undefined' && window.__APP_CONFIG__) || {}

export const AUTH_URL =
  runtime.AUTH_URL || import.meta.env.VITE_AUTH_URL || 'http://localhost:8080'

export const NATS_URL =
  runtime.NATS_URL || import.meta.env.VITE_NATS_URL || 'ws://localhost:9222'

export const DEFAULT_SITE_ID =
  runtime.DEFAULT_SITE_ID || import.meta.env.VITE_DEFAULT_SITE_ID || 'site-local'

export const DEV_MODE =
  (runtime.DEV_MODE ?? import.meta.env.VITE_DEV_MODE ?? 'true') === 'true'

export const OIDC_ISSUER_URL =
  runtime.OIDC_ISSUER_URL ||
  import.meta.env.VITE_OIDC_ISSUER_URL ||
  'http://localhost:8180/realms/chatapp'

export const OIDC_CLIENT_ID =
  runtime.OIDC_CLIENT_ID || import.meta.env.VITE_OIDC_CLIENT_ID || 'nats-chat'
