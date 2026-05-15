// OIDC client singleton for Keycloak (or any OIDC-compliant) login.
// Uses authorization code flow with PKCE.
import { UserManager, WebStorageStateStore } from 'oidc-client-ts'
import { OIDC_CLIENT_ID, OIDC_ISSUER_URL } from '@/lib/runtimeConfig'

let manager = null

export function getOidcManager() {
  if (manager) return manager

  const redirectUri = `${window.location.origin}/oidc-callback`

  manager = new UserManager({
    authority: OIDC_ISSUER_URL,
    client_id: OIDC_CLIENT_ID,
    redirect_uri: redirectUri,
    post_logout_redirect_uri: window.location.origin,
    response_type: 'code',
    scope: 'openid profile email',
    userStore: new WebStorageStateStore({ store: window.sessionStorage }),
  })

  return manager
}

// Test-only helper to reset the singleton between tests.
export function _resetOidcManagerForTests() {
  manager = null
}
