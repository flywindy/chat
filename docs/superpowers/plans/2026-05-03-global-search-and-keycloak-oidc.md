# Global Search + Keycloak OIDC Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Microsoft Teams-style global search bar with room type-ahead, Keycloak OIDC authentication for non-dev mode, and jump-to-message with historical buffer mode.

**Architecture:** Frontend SPA (no router) with DEV_MODE flag; when true uses account form, when false uses Keycloak PKCE flow. Global search dropdown uses spotlight index (room type-ahead), search results pane has Rooms + Messages tabs. Jump-to-message uses existing LoadSurroundingMessages API with buffer mode tracking in reducer.

**Tech Stack:** React 19 + Vite, Vitest + RTL, oidc-client-ts for PKCE, Keycloak 26.0 in docker-compose.

---

### Task 1: Update Keycloak realm export with alice + bob users

**Files:**
- Modify: `auth-service/deploy/keycloak/realm-export.json`

- [ ] **Step 1: Update realm-export.json**

Replace the entire `users` array and add Vite dev server to redirect URIs:

```json
{
  "realm": "chatapp",
  "enabled": true,
  "sslRequired": "none",
  "registrationAllowed": false,
  "loginWithEmailAllowed": true,
  "duplicateEmailsAllowed": false,
  "resetPasswordAllowed": false,
  "editUsernameAllowed": false,
  "bruteForceProtected": false,
  "roles": {
    "realm": [
      {
        "name": "user",
        "description": "Regular chat user"
      },
      {
        "name": "admin",
        "description": "Admin user"
      }
    ]
  },
  "clients": [
    {
      "clientId": "nats-chat",
      "name": "NATS Chat App",
      "enabled": true,
      "publicClient": true,
      "directAccessGrantsEnabled": true,
      "standardFlowEnabled": true,
      "implicitFlowEnabled": false,
      "redirectUris": [
        "http://localhost:3000/*",
        "http://localhost:5173/*"
      ],
      "webOrigins": [
        "http://localhost:3000",
        "http://localhost:5173"
      ],
      "protocol": "openid-connect",
      "attributes": {
        "pkce.code.challenge.method": "S256",
        "post.logout.redirect.uris": "http://localhost:3000/*,http://localhost:5173/*"
      },
      "defaultClientScopes": [
        "web-origins",
        "profile",
        "roles",
        "email"
      ],
      "protocolMappers": [
        {
          "name": "audience-nats-chat",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "config": {
            "included.client.audience": "nats-chat",
            "id.token.claim": "true",
            "access.token.claim": "true"
          }
        }
      ]
    }
  ],
  "users": [
    {
      "username": "alice",
      "email": "alice@example.com",
      "firstName": "Alice",
      "lastName": "Engineer",
      "enabled": true,
      "emailVerified": true,
      "credentials": [
        {
          "type": "password",
          "value": "password",
          "temporary": false
        }
      ],
      "realmRoles": [
        "user",
        "admin"
      ]
    },
    {
      "username": "bob",
      "email": "bob@example.com",
      "firstName": "Bob",
      "lastName": "Developer",
      "enabled": true,
      "emailVerified": true,
      "credentials": [
        {
          "type": "password",
          "value": "password",
          "temporary": false
        }
      ],
      "realmRoles": [
        "user"
      ]
    }
  ]
}
```

- [ ] **Step 2: Verify locally (manual test)**

Start Keycloak locally: `docker-compose -f docker-local/compose.services.yaml -f docker-local/compose.deps.yaml up keycloak`. Browse to `http://localhost:8180/admin/master/console/`. Login as admin (if prompted). Verify realm `chatapp` exists with users `alice` and `bob`, both with password `password`.

- [ ] **Step 3: Commit**

```bash
git add auth-service/deploy/keycloak/realm-export.json
git commit -m "chore(docker-local): add Keycloak realm with alice+bob users and PKCE client

Updated realm-export.json with:
- alice user (password: password)
- bob user (password: password)
- PKCE-enabled nats-chat client
- Redirect URIs for localhost:3000 (Docker) and localhost:5173 (Vite dev)

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 2: Add DEV_MODE and OIDC runtime config keys

**Files:**
- Modify: `chat-frontend/src/lib/runtimeConfig.js`
- Create: `chat-frontend/src/lib/runtimeConfig.test.js`

- [ ] **Step 1: Write failing test**

Create `chat-frontend/src/lib/runtimeConfig.test.js`:

```javascript
import { describe, it, expect, beforeEach, vi } from 'vitest'

describe('runtimeConfig', () => {
  beforeEach(() => {
    vi.resetModules()
    delete window.__APP_CONFIG__
  })

  it('DEV_MODE defaults to true when not overridden', async () => {
    const { DEV_MODE } = await import('./runtimeConfig.js')
    expect(DEV_MODE).toBe(true)
  })

  it('DEV_MODE is false when window.__APP_CONFIG__.DEV_MODE = "false"', async () => {
    window.__APP_CONFIG__ = { DEV_MODE: 'false' }
    const { DEV_MODE } = await import('./runtimeConfig.js')
    expect(DEV_MODE).toBe(false)
  })

  it('OIDC_ISSUER_URL defaults to Keycloak chatapp realm', async () => {
    const { OIDC_ISSUER_URL } = await import('./runtimeConfig.js')
    expect(OIDC_ISSUER_URL).toBe('http://localhost:8180/realms/chatapp')
  })

  it('OIDC_CLIENT_ID defaults to nats-chat', async () => {
    const { OIDC_CLIENT_ID } = await import('./runtimeConfig.js')
    expect(OIDC_CLIENT_ID).toBe('nats-chat')
  })

  it('OIDC_ISSUER_URL reads from window.__APP_CONFIG__', async () => {
    window.__APP_CONFIG__ = { OIDC_ISSUER_URL: 'https://custom-keycloak/realms/myrealm' }
    const { OIDC_ISSUER_URL } = await import('./runtimeConfig.js')
    expect(OIDC_ISSUER_URL).toBe('https://custom-keycloak/realms/myrealm')
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd chat-frontend
npm test -- src/lib/runtimeConfig.test.js
```

Expected: FAIL — 4 failures, DEV_MODE/OIDC_ISSUER_URL/OIDC_CLIENT_ID are not exported.

- [ ] **Step 3: Update runtimeConfig.js with new keys**

Append to `chat-frontend/src/lib/runtimeConfig.js`:

```javascript
export const DEV_MODE =
  (runtime.DEV_MODE ?? import.meta.env.VITE_DEV_MODE ?? 'true') === 'true'

export const OIDC_ISSUER_URL =
  runtime.OIDC_ISSUER_URL ||
  import.meta.env.VITE_OIDC_ISSUER_URL ||
  'http://localhost:8180/realms/chatapp'

export const OIDC_CLIENT_ID =
  runtime.OIDC_CLIENT_ID || import.meta.env.VITE_OIDC_CLIENT_ID || 'nats-chat'
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd chat-frontend
npm test -- src/lib/runtimeConfig.test.js
```

Expected: PASS — all 5 tests pass.

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/lib/runtimeConfig.js chat-frontend/src/lib/runtimeConfig.test.js
git commit -m "feat(chat-frontend): add DEV_MODE and OIDC runtime config

Added three new config keys to runtimeConfig.js:
- DEV_MODE (bool, default true): switches between account form and OIDC login
- OIDC_ISSUER_URL (string): Keycloak realm URL
- OIDC_CLIENT_ID (string): Keycloak client ID

All keys read from window.__APP_CONFIG__, then VITE_* env vars, then defaults.

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 3: Implement DEV_MODE login switch and Keycloak OIDC flow

**Files:**
- Create: `chat-frontend/src/lib/oidcClient.js`
- Modify: `chat-frontend/src/context/NatsContext.jsx`
- Modify: `chat-frontend/src/pages/LoginPage.jsx`
- Create: `chat-frontend/src/pages/OidcCallback.jsx`
- Modify: `chat-frontend/src/App.jsx`
- Create: `chat-frontend/src/pages/OidcCallback.test.jsx`
- Create: `chat-frontend/src/pages/LoginPage.test.jsx`

- [ ] **Step 1: Install oidc-client-ts**

```bash
cd chat-frontend
npm install oidc-client-ts
```

- [ ] **Step 2: Create oidcClient.js**

Create `chat-frontend/src/lib/oidcClient.js`:

```javascript
import { UserManager } from 'oidc-client-ts'
import { OIDC_ISSUER_URL, OIDC_CLIENT_ID } from './runtimeConfig'

let manager = null

export function getOidcManager() {
  if (!manager) {
    manager = new UserManager({
      authority: OIDC_ISSUER_URL,
      client_id: OIDC_CLIENT_ID,
      redirect_uri: `${window.location.origin}/oidc-callback`,
      response_type: 'code',
      scope: 'openid profile email',
    })
  }
  return manager
}
```

- [ ] **Step 3: Write failing test for OidcCallback**

Create `chat-frontend/src/pages/OidcCallback.test.jsx`:

```javascript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import OidcCallback from './OidcCallback'

vi.mock('../lib/oidcClient', () => ({
  getOidcManager: vi.fn(),
}))

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { getOidcManager } from '../lib/oidcClient'
import { useNats } from '../context/NatsContext'

describe('OidcCallback', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    sessionStorage.clear()
  })

  it('exchanges code for token and connects to NATS', async () => {
    const connect = vi.fn().mockResolvedValue()
    useNats.mockReturnValue({ connect })

    const mockUser = { access_token: 'test-token-123' }
    const mockManager = {
      signinRedirectCallback: vi.fn().mockResolvedValue(mockUser),
    }
    getOidcManager.mockReturnValue(mockManager)

    sessionStorage.setItem('pendingSiteId', 'site-A')
    const onDone = vi.fn()

    render(<OidcCallback onDone={onDone} />)

    await waitFor(() => {
      expect(mockManager.signinRedirectCallback).toHaveBeenCalled()
    })

    await waitFor(() => {
      expect(connect).toHaveBeenCalledWith({
        mode: 'sso',
        ssoToken: 'test-token-123',
        siteId: 'site-A',
      })
    })

    await waitFor(() => {
      expect(onDone).toHaveBeenCalled()
    })
  })

  it('shows error message on callback failure', async () => {
    useNats.mockReturnValue({ connect: vi.fn() })
    const mockManager = {
      signinRedirectCallback: vi.fn().mockRejectedValue(new Error('invalid code')),
    }
    getOidcManager.mockReturnValue(mockManager)

    render(<OidcCallback onDone={vi.fn()} />)

    await waitFor(() => {
      expect(screen.getByText(/invalid code/)).toBeInTheDocument()
    })
  })
})
```

- [ ] **Step 4: Write failing test for LoginPage**

Create `chat-frontend/src/pages/LoginPage.test.jsx`:

```javascript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import LoginPage from './LoginPage'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

vi.mock('../lib/runtimeConfig', () => ({
  DEV_MODE: true,
  DEFAULT_SITE_ID: 'site-local',
  OIDC_ISSUER_URL: 'http://localhost:8180/realms/chatapp',
  OIDC_CLIENT_ID: 'nats-chat',
}))

vi.mock('../lib/oidcClient', () => ({
  getOidcManager: vi.fn(() => ({
    signinRedirect: vi.fn(),
    getUser: vi.fn().mockResolvedValue(null),
  })),
}))

import { useNats } from '../context/NatsContext'

describe('LoginPage DEV_MODE=true', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('renders account and siteId form', () => {
    useNats.mockReturnValue({
      connect: vi.fn(),
      error: null,
    })

    render(<LoginPage />)
    expect(screen.getByLabelText(/Account/)).toBeInTheDocument()
    expect(screen.getByLabelText(/Site ID/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Connect/ })).toBeInTheDocument()
  })

  it('submits connect with account and siteId on form submit', async () => {
    const connect = vi.fn().mockResolvedValue()
    useNats.mockReturnValue({
      connect,
      error: null,
    })

    render(<LoginPage />)
    fireEvent.change(screen.getByLabelText(/Account/), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/Site ID/), { target: { value: 'site-A' } })
    fireEvent.click(screen.getByRole('button', { name: /Connect/ }))

    await waitFor(() => {
      expect(connect).toHaveBeenCalledWith({
        mode: 'dev',
        account: 'alice',
        siteId: 'site-A',
      })
    })
  })
})

describe('LoginPage DEV_MODE=false', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.doMock('../lib/runtimeConfig', () => ({
      DEV_MODE: false,
      DEFAULT_SITE_ID: 'site-local',
      OIDC_ISSUER_URL: 'http://localhost:8180/realms/chatapp',
      OIDC_CLIENT_ID: 'nats-chat',
    }))
  })

  afterEach(() => {
    vi.undoMocks()
  })

  it('renders siteId input and Sign in with Keycloak button', async () => {
    vi.resetModules()
    vi.doMock('../lib/runtimeConfig', () => ({
      DEV_MODE: false,
      DEFAULT_SITE_ID: 'site-local',
    }))

    const { default: LoginPageReloaded } = await import('./LoginPage')

    useNats.mockReturnValue({
      connect: vi.fn(),
      error: null,
    })

    render(<LoginPageReloaded />)
    expect(screen.getByLabelText(/Site ID/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Sign in with Keycloak/ })).toBeInTheDocument()
  })
})
```

- [ ] **Step 5: Create OidcCallback.jsx**

Create `chat-frontend/src/pages/OidcCallback.jsx`:

```javascript
import { useEffect, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { getOidcManager } from '../lib/oidcClient'

export default function OidcCallback({ onDone }) {
  const { connect } = useNats()
  const [error, setError] = useState(null)

  useEffect(() => {
    const siteId = sessionStorage.getItem('pendingSiteId') || 'site-local'
    sessionStorage.removeItem('pendingSiteId')

    getOidcManager()
      .signinRedirectCallback()
      .then((user) =>
        connect({ mode: 'sso', ssoToken: user.access_token, siteId })
      )
      .then(() => {
        window.history.replaceState({}, '', '/')
        onDone()
      })
      .catch((err) => setError(err.message))
  }, [connect, onDone])

  if (error) {
    return (
      <div className="login-page">
        <div className="login-form">
          <h1>Login Error</h1>
          <p className="login-error">{error}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="login-page">
      <div className="login-form">
        <p>Completing sign-in…</p>
      </div>
    </div>
  )
}
```

- [ ] **Step 6: Update NatsContext.jsx connect signature**

In `chat-frontend/src/context/NatsContext.jsx`, replace the `connectToNats` function and its usage:

```javascript
const connectToNats = useCallback(async (opts) => {
  setError(null)

  const { mode, account, ssoToken, siteId } = opts

  const nkey = createUser()
  const natsPublicKey = nkey.getPublicKey()

  // Determine auth request body based on mode
  let authBody
  if (mode === 'sso') {
    authBody = { ssoToken, natsPublicKey }
  } else {
    // mode === 'dev'
    authBody = { account, natsPublicKey }
  }

  const authResp = await fetch(`${authUrl}/auth`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(authBody),
  })

  if (!authResp.ok) {
    const body = await authResp.json().catch(() => ({}))
    throw new Error(body.error || `Auth failed: ${authResp.status}`)
  }

  const { natsJwt, user: userInfo } = await authResp.json()

  const nc = await natsConnect({
    servers: natsUrl,
    authenticator: jwtAuthenticator(natsJwt, nkey.getSeed()),
  })

  ncRef.current = nc
  setUser({ ...userInfo, siteId })
  setConnected(true)

  nc.closed().then((err) => {
    if (err) {
      setError(`Disconnected: ${err.message}`)
    }
    setConnected(false)
  })
}, [authUrl, natsUrl])
```

- [ ] **Step 7: Update LoginPage.jsx for DEV_MODE conditional**

Replace `chat-frontend/src/pages/LoginPage.jsx`:

```javascript
import { useState } from 'react'
import { useNats } from '../context/NatsContext'
import { DEV_MODE, DEFAULT_SITE_ID } from '../lib/runtimeConfig'
import { getOidcManager } from '../lib/oidcClient'

export default function LoginPage() {
  const { connect, error: natsError } = useNats()
  const defaultSiteId = DEFAULT_SITE_ID

  const [account, setAccount] = useState('')
  const [siteId, setSiteId] = useState(defaultSiteId)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)

  if (DEV_MODE) {
    // Dev mode: account form
    const handleSubmit = async (e) => {
      e.preventDefault()
      if (!account.trim()) return

      setLoading(true)
      setError(null)
      try {
        await connect({ mode: 'dev', account: account.trim(), siteId: siteId.trim() })
      } catch (err) {
        setError(err.message)
      } finally {
        setLoading(false)
      }
    }

    return (
      <div className="login-page">
        <form className="login-form" onSubmit={handleSubmit}>
          <h1>Chat</h1>
          <p className="login-subtitle">Dev Mode Login</p>

          <label htmlFor="account">Account</label>
          <input
            id="account"
            type="text"
            value={account}
            onChange={(e) => setAccount(e.target.value)}
            placeholder="e.g. alice"
            autoFocus
            disabled={loading}
          />

          <label htmlFor="siteId">Site ID</label>
          <input
            id="siteId"
            type="text"
            value={siteId}
            onChange={(e) => setSiteId(e.target.value)}
            disabled={loading}
          />

          <button type="submit" disabled={loading || !account.trim()}>
            {loading ? 'Connecting...' : 'Connect'}
          </button>

          {(error || natsError) && (
            <div className="login-error">{error || natsError}</div>
          )}
        </form>
      </div>
    )
  }

  // SSO mode: Keycloak login
  const handleSignIn = async () => {
    sessionStorage.setItem('pendingSiteId', siteId.trim())
    await getOidcManager().signinRedirect()
  }

  return (
    <div className="login-page">
      <form className="login-form" onSubmit={(e) => e.preventDefault()}>
        <h1>Chat</h1>
        <p className="login-subtitle">Keycloak Login</p>

        <label htmlFor="siteId">Site ID</label>
        <input
          id="siteId"
          type="text"
          value={siteId}
          onChange={(e) => setSiteId(e.target.value)}
        />

        <button type="button" onClick={handleSignIn}>
          Sign in with Keycloak
        </button>

        {natsError && <div className="login-error">{natsError}</div>}
      </form>
    </div>
  )
}
```

- [ ] **Step 8: Update App.jsx for OIDC callback path**

In `chat-frontend/src/App.jsx`, add path tracking:

```javascript
import { useEffect, useState } from 'react'
import { NatsProvider, useNats } from './context/NatsContext'
import { RoomEventsProvider } from './context/RoomEventsContext'
import LoginPage from './pages/LoginPage'
import ChatPage from './pages/ChatPage'
import OidcCallback from './pages/OidcCallback'

function AppContent() {
  const { connected } = useNats()
  const [path, setPath] = useState(window.location.pathname)

  useEffect(() => {
    const onPopState = () => setPath(window.location.pathname)
    window.addEventListener('popstate', onPopState)
    return () => window.removeEventListener('popstate', onPopState)
  }, [])

  if (path === '/oidc-callback') {
    return <OidcCallback onDone={() => setPath('/')} />
  }

  if (!connected) {
    return <LoginPage />
  }

  return (
    <RoomEventsProvider>
      <ChatPage />
    </RoomEventsProvider>
  )
}

export default function App() {
  return (
    <NatsProvider>
      <AppContent />
    </NatsProvider>
  )
}
```

- [ ] **Step 9: Run tests**

```bash
cd chat-frontend
npm test -- src/pages/OidcCallback.test.jsx src/pages/LoginPage.test.jsx
```

Expected: All tests pass.

- [ ] **Step 10: Commit**

```bash
git add chat-frontend/package.json chat-frontend/src/lib/oidcClient.js \
  chat-frontend/src/context/NatsContext.jsx \
  chat-frontend/src/pages/LoginPage.jsx \
  chat-frontend/src/pages/OidcCallback.jsx \
  chat-frontend/src/App.jsx \
  chat-frontend/src/pages/OidcCallback.test.jsx \
  chat-frontend/src/pages/LoginPage.test.jsx

git commit -m "feat(chat-frontend): DEV_MODE switch with Keycloak OIDC login

- Added oidc-client-ts for PKCE authorization code flow
- NatsContext.connect() now takes {mode, account/ssoToken, siteId}
- LoginPage: DEV_MODE=true shows account form, false shows Keycloak button
- Added OidcCallback page to handle OIDC redirect with code exchange
- App.jsx tracks path to detect /oidc-callback and render OidcCallback
- Both modes store siteId in pending field/sessionStorage for federation

Tests:
- OidcCallback: exchanges code, calls connect(mode:sso), calls onDone()
- LoginPage DEV_MODE=true: submits form with connect(mode:dev, account)
- LoginPage DEV_MODE=false: renders Keycloak button, stores siteId before redirect

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 4: Add global search bar with room type-ahead dropdown

**Files:**
- Modify: `chat-frontend/src/lib/subjects.js`
- Modify: `chat-frontend/src/lib/subjects.test.js`
- Create: `chat-frontend/src/components/SearchBar.jsx`
- Create: `chat-frontend/src/components/SearchBar.test.jsx`
- Modify: `chat-frontend/src/pages/ChatPage.jsx`
- Modify: `chat-frontend/src/pages/ChatPage.test.jsx`

- [ ] **Step 1: Add searchRooms to subjects.js and write failing test**

Update `chat-frontend/src/lib/subjects.js` to add at the end:

```javascript
export function searchRooms(account) {
  return `chat.user.${account}.request.search.rooms`
}
```

Update `chat-frontend/src/lib/subjects.test.js` to add:

```javascript
it('searchRooms builds the search rooms request subject', () => {
  expect(searchRooms('alice')).toBe('chat.user.alice.request.search.rooms')
})
```

Run: `npm test -- src/lib/subjects.test.js` → should fail (searchRooms not yet exported)

- [ ] **Step 2: Run test, then fix subjects.test.js**

After SearchBar.jsx tests below will validate the actual searchRooms call, keep the subjects test simple. Run test to confirm pass:

```bash
npm test -- src/lib/subjects.test.js
```

- [ ] **Step 3: Write failing SearchBar test**

Create `chat-frontend/src/components/SearchBar.test.jsx`:

```javascript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import SearchBar from './SearchBar'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

describe('SearchBar', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('does not search when query < 2 chars', async () => {
    const request = vi.fn()
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<SearchBar onSelectRoom={vi.fn()} onEnterSearch={vi.fn()} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'a' } })
    vi.runAllTimers()

    expect(request).not.toHaveBeenCalled()
  })

  it('fetches rooms after 250ms debounce when query >= 2 chars', async () => {
    const request = vi.fn().mockResolvedValue({
      results: [
        { roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<SearchBar onSelectRoom={vi.fn()} onEnterSearch={vi.fn()} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'fro' } })

    expect(request).not.toHaveBeenCalled()

    vi.advanceTimersByTime(250)
    await waitFor(() => {
      expect(request).toHaveBeenCalledWith(
        'chat.user.alice.request.search.rooms',
        { searchText: 'fro', scope: 'all', size: 8 }
      )
    })
  })

  it('shows results in dropdown', async () => {
    const request = vi.fn().mockResolvedValue({
      results: [
        { roomId: 'r1', roomName: 'frontend-team', roomType: 'c', siteId: 'site-A' },
        { roomId: 'r2', roomName: 'frontend-perf', roomType: 'c', siteId: 'site-A' },
      ],
      total: 2,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<SearchBar onSelectRoom={vi.fn()} onEnterSearch={vi.fn()} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'fro' } })

    vi.advanceTimersByTime(250)
    await waitFor(() => {
      expect(screen.getByText('frontend-team')).toBeInTheDocument()
      expect(screen.getByText('frontend-perf')).toBeInTheDocument()
    })
  })

  it('clicking result calls onSelectRoom and clears input', async () => {
    const onSelectRoom = vi.fn()
    const request = vi.fn().mockResolvedValue({
      results: [
        { roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<SearchBar onSelectRoom={onSelectRoom} onEnterSearch={vi.fn()} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'gen' } })

    vi.advanceTimersByTime(250)
    await waitFor(() => {
      fireEvent.click(screen.getByText('general'))
    })

    expect(onSelectRoom).toHaveBeenCalledWith({
      id: 'r1',
      name: 'general',
      type: 'c',
      siteId: 'site-A',
    })
    expect(screen.getByRole('textbox').value).toBe('')
  })

  it('Enter key calls onEnterSearch', async () => {
    const onEnterSearch = vi.fn()
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request: vi.fn().mockResolvedValue({ results: [], total: 0 }),
    })

    render(<SearchBar onSelectRoom={vi.fn()} onEnterSearch={onEnterSearch} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'test' } })

    vi.advanceTimersByTime(250)
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter' })

    expect(onEnterSearch).toHaveBeenCalledWith('test')
  })

  it('Escape key closes dropdown and clears input', async () => {
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request: vi.fn().mockResolvedValue({
        results: [{ roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' }],
        total: 1,
      }),
    })

    render(<SearchBar onSelectRoom={vi.fn()} onEnterSearch={vi.fn()} />)
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'gen' } })

    vi.advanceTimersByTime(250)
    await waitFor(() => {
      expect(screen.getByText('general')).toBeInTheDocument()
    })

    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Escape' })

    expect(screen.queryByText('general')).not.toBeInTheDocument()
    expect(screen.getByRole('textbox').value).toBe('')
  })
})
```

- [ ] **Step 4: Create SearchBar.jsx**

Create `chat-frontend/src/components/SearchBar.jsx`:

```javascript
import { useState, useRef, useEffect } from 'react'
import { useNats } from '../context/NatsContext'
import { searchRooms } from '../lib/subjects'

export default function SearchBar({ onSelectRoom, onEnterSearch }) {
  const { user, request } = useNats()
  const [query, setQuery] = useState('')
  const [results, setResults] = useState([])
  const [loading, setLoading] = useState(false)
  const [activeIdx, setActiveIdx] = useState(0)
  const debounceRef = useRef(null)
  const inputRef = useRef(null)

  // Ctrl+K global shortcut
  useEffect(() => {
    const handler = (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
        e.preventDefault()
        inputRef.current?.focus()
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [])

  const handleChange = (e) => {
    const q = e.target.value
    setQuery(q)
    clearTimeout(debounceRef.current)

    if (q.length < 2) {
      setResults([])
      return
    }

    debounceRef.current = setTimeout(async () => {
      setLoading(true)
      try {
        const resp = await request(searchRooms(user.account), {
          searchText: q,
          scope: 'all',
          size: 8,
        })
        setResults(resp.results ?? [])
      } catch {
        setResults([])
      } finally {
        setLoading(false)
      }
      setActiveIdx(0)
    }, 250)
  }

  const handleKeyDown = (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActiveIdx((i) => Math.min(i + 1, results.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActiveIdx((i) => Math.max(i - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (query.length >= 2) onEnterSearch(query)
    } else if (e.key === 'Escape') {
      setQuery('')
      setResults([])
      inputRef.current?.blur()
    }
  }

  const handleClick = (hit) => {
    onSelectRoom({
      id: hit.roomId,
      name: hit.roomName,
      type: hit.roomType,
      siteId: hit.siteId,
    })
    setQuery('')
    setResults([])
  }

  return (
    <div className="search-bar-wrap">
      <input
        ref={inputRef}
        type="text"
        className="search-bar"
        value={query}
        onChange={handleChange}
        onKeyDown={handleKeyDown}
        placeholder="Search rooms..."
        aria-label="Search rooms"
      />
      {query.length >= 2 && results.length > 0 && (
        <div className="search-dropdown" role="listbox">
          {results.map((hit, idx) => (
            <div
              key={hit.roomId}
              className={`search-result ${idx === activeIdx ? 'active' : ''}`}
              onClick={() => handleClick(hit)}
              role="option"
              aria-selected={idx === activeIdx}
            >
              <div className="result-type">
                {hit.roomType === 'c' ? '#' : '@'}
              </div>
              <div className="result-name">{hit.roomName}</div>
            </div>
          ))}
          <div className="search-footer">
            <span>↑↓ navigate · Enter see all · Esc close</span>
            <span>{results.length} rooms</span>
          </div>
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 5: Run SearchBar tests**

```bash
npm test -- src/components/SearchBar.test.jsx
```

Expected: All tests pass.

- [ ] **Step 6: Update ChatPage.jsx**

In `chat-frontend/src/pages/ChatPage.jsx`, add SearchBar to header and manage search state:

```javascript
import { useState } from 'react'
// ... existing imports
import SearchBar from '../components/SearchBar'

export default function ChatPage() {
  const { user, disconnect } = useNats()
  const { summaries, setActiveRoom } = useRoomSummaries()
  const [selectedRoom, setSelectedRoom] = useState(null)
  const [showCreateRoom, setShowCreateRoom] = useState(false)
  const [showMembers, setShowMembers] = useState(false)
  const [searchQuery, setSearchQuery] = useState(null) // NEW

  // ... existing useEffect and handlers

  const handleSelectRoom = (room) => {
    setSelectedRoom(room)
    setActiveRoom(room?.id ?? null)
    setShowMembers(false)
    setSearchQuery(null) // Clear search when selecting room
  }

  const isChannel = selectedRoom?.type === 'channel'

  return (
    <div className="chat-layout">
      <div className="chat-header">
        <span className="chat-header-title">Chat</span>
        <div className="chat-header-search">
          <SearchBar
            onSelectRoom={handleSelectRoom}
            onEnterSearch={(q) => setSearchQuery(q)}
          />
        </div>
        {isChannel && (
          <>
            <button
              type="button"
              className="chat-header-logout"
              onClick={() => setShowMembers(true)}
            >
              Members
            </button>
            <LeaveRoomButton room={selectedRoom} />
          </>
        )}
        <span className="chat-header-user">
          {user?.account} &middot; {user?.siteId}
        </span>
        <button className="chat-header-logout" onClick={disconnect}>
          Logout
        </button>
      </div>
      <div className="chat-body">
        <div className="chat-sidebar">
          <RoomList
            selectedRoomId={selectedRoom?.id}
            onSelectRoom={handleSelectRoom}
          />
          <button
            className="create-room-btn"
            onClick={() => setShowCreateRoom(true)}
          >
            + Create Room
          </button>
        </div>
        <div className="chat-main">
          {searchQuery ? (
            <div>Search Results (to be implemented in Task 5)</div>
          ) : (
            <>
              <MessageArea room={selectedRoom} />
              <MessageInput room={selectedRoom} />
            </>
          )}
        </div>
      </div>
      {/* ... rest of component unchanged */}
    </div>
  )
}
```

- [ ] **Step 7: Update ChatPage.test.jsx**

Add SearchBar mock to the vi.mock calls:

```javascript
vi.mock('../components/SearchBar', () => ({
  default: ({ onSelectRoom, onEnterSearch }) => (
    <input
      data-testid="search-bar"
      onKeyDown={(e) => {
        if (e.key === 'Enter') onEnterSearch('test-query')
      }}
    />
  ),
}))
```

Run tests: `npm test -- src/pages/ChatPage.test.jsx` → should pass.

- [ ] **Step 8: Commit**

```bash
git add chat-frontend/src/lib/subjects.js chat-frontend/src/lib/subjects.test.js \
  chat-frontend/src/components/SearchBar.jsx chat-frontend/src/components/SearchBar.test.jsx \
  chat-frontend/src/pages/ChatPage.jsx chat-frontend/src/pages/ChatPage.test.jsx

git commit -m "feat(chat-frontend): centered global search bar with room type-ahead

- Added searchRooms(account) subject builder
- SearchBar component: debounced 250ms search after 2 chars, max 8 results
- Dropdown with ↑↓ keyboard nav, Enter → search results, Esc → close
- Clicking result navigates to room and closes dropdown
- Ctrl+K global shortcut focuses search bar
- Integrated into ChatPage header, centered with sidebar + logout on right
- Tests: debounce timing, result display, keyboard nav, click handling

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 5: Add search results pane with Rooms and Messages tabs

**Files:**
- Modify: `chat-frontend/src/lib/subjects.js`
- Modify: `chat-frontend/src/lib/subjects.test.js`
- Create: `chat-frontend/src/pages/SearchResultsPane.jsx`
- Create: `chat-frontend/src/pages/SearchResultsPane.test.jsx`
- Modify: `chat-frontend/src/pages/ChatPage.jsx`
- Modify: `chat-frontend/src/pages/ChatPage.test.jsx`

- [ ] **Step 1: Add searchMessages to subjects.js**

Update `chat-frontend/src/lib/subjects.js`:

```javascript
export function searchMessages(account) {
  return `chat.user.${account}.request.search.messages`
}
```

Update `chat-frontend/src/lib/subjects.test.js`:

```javascript
it('searchMessages builds the search messages request subject', () => {
  expect(searchMessages('alice')).toBe('chat.user.alice.request.search.messages')
})
```

Run: `npm test -- src/lib/subjects.test.js` → PASS

- [ ] **Step 2: Write failing SearchResultsPane test**

Create `chat-frontend/src/pages/SearchResultsPane.test.jsx`:

```javascript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import SearchResultsPane from './SearchResultsPane'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

describe('SearchResultsPane', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('fetches and displays room results immediately', async () => {
    const request = vi.fn().mockResolvedValue({
      results: [
        { roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(
      <SearchResultsPane
        query="gen"
        onClose={vi.fn()}
        onSelectRoom={vi.fn()}
        onJumpToMessage={vi.fn()}
      />
    )

    await waitFor(() => {
      expect(screen.getByText('general')).toBeInTheDocument()
    })

    expect(request).toHaveBeenCalledWith(
      'chat.user.alice.request.search.rooms',
      expect.objectContaining({ searchText: 'gen' })
    )
  })

  it('Rooms tab shows results, Messages tab fetches on click', async () => {
    const request = vi.fn().mockImplementation((subject) => {
      if (subject.includes('.search.rooms')) {
        return Promise.resolve({
          results: [{ roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' }],
          total: 1,
        })
      }
      if (subject.includes('.search.messages')) {
        return Promise.resolve({
          results: [
            { messageId: 'm1', roomId: 'r1', content: 'hello', createdAt: '2026-04-17T10:00:00Z', userAccount: 'bob' },
          ],
          total: 1,
        })
      }
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(
      <SearchResultsPane
        query="test"
        onClose={vi.fn()}
        onSelectRoom={vi.fn()}
        onJumpToMessage={vi.fn()}
      />
    )

    // Room results shown on Rooms tab
    await waitFor(() => {
      expect(screen.getByText('general')).toBeInTheDocument()
    })

    // Click Messages tab
    fireEvent.click(screen.getByRole('tab', { name: /Messages/ }))

    // Messages results show
    await waitFor(() => {
      expect(screen.getByText('hello')).toBeInTheDocument()
    })
  })

  it('clicking room result calls onSelectRoom and onClose', async () => {
    const onSelectRoom = vi.fn()
    const onClose = vi.fn()
    const request = vi.fn().mockResolvedValue({
      results: [
        { roomId: 'r1', roomName: 'general', roomType: 'c', siteId: 'site-A' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(
      <SearchResultsPane
        query="gen"
        onClose={onClose}
        onSelectRoom={onSelectRoom}
        onJumpToMessage={vi.fn()}
      />
    )

    await waitFor(() => {
      fireEvent.click(screen.getByText('general'))
    })

    expect(onSelectRoom).toHaveBeenCalledWith({
      id: 'r1',
      name: 'general',
      type: 'c',
      siteId: 'site-A',
    })
    expect(onClose).toHaveBeenCalled()
  })
})
```

- [ ] **Step 3: Create SearchResultsPane.jsx**

Create `chat-frontend/src/pages/SearchResultsPane.jsx`:

```javascript
import { useEffect, useState } from 'react'
import { useNats } from '../context/NatsContext'
import { searchRooms, searchMessages } from '../lib/subjects'

export default function SearchResultsPane({
  query,
  onClose,
  onSelectRoom,
  onJumpToMessage,
}) {
  const { user, request } = useNats()
  const [activeTab, setActiveTab] = useState('rooms')
  const [roomResults, setRoomResults] = useState([])
  const [roomTotal, setRoomTotal] = useState(0)
  const [msgResults, setMsgResults] = useState([])
  const [msgTotal, setMsgTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [msgFetched, setMsgFetched] = useState(false)

  // Fetch rooms on mount
  useEffect(() => {
    if (!query || !user) return
    setLoading(true)
    request(searchRooms(user.account), {
      searchText: query,
      scope: 'all',
      size: 50,
    })
      .then((resp) => {
        setRoomResults(resp.results ?? [])
        setRoomTotal(resp.total ?? 0)
      })
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [query, user, request])

  // Fetch messages when tab clicked
  const handleMessagesTab = () => {
    setActiveTab('messages')
    if (msgFetched) return

    setLoading(true)
    request(searchMessages(user.account), {
      searchText: query,
      size: 50,
    })
      .then((resp) => {
        setMsgResults(resp.results ?? [])
        setMsgTotal(resp.total ?? 0)
        setMsgFetched(true)
      })
      .catch(() => {})
      .finally(() => setLoading(false))
  }

  const handleRoomClick = (hit) => {
    onSelectRoom({
      id: hit.roomId,
      name: hit.roomName,
      type: hit.roomType,
      siteId: hit.siteId,
    })
    onClose()
  }

  const handleMessageClick = (hit) => {
    onJumpToMessage(hit.roomId, hit.messageId)
    onClose()
  }

  return (
    <div className="search-results-pane">
      <div className="search-results-header">
        <h2>Search Results: "{query}"</h2>
        <button className="search-results-close" onClick={onClose}>
          ✕
        </button>
      </div>

      <div className="search-results-tabs">
        <button
          className={`tab ${activeTab === 'rooms' ? 'active' : ''}`}
          onClick={() => setActiveTab('rooms')}
          role="tab"
          aria-label="Rooms"
        >
          Rooms ({roomTotal})
        </button>
        <button
          className={`tab ${activeTab === 'messages' ? 'active' : ''}`}
          onClick={handleMessagesTab}
          role="tab"
          aria-label="Messages"
        >
          Messages ({msgTotal})
        </button>
      </div>

      <div className="search-results-content">
        {activeTab === 'rooms' && (
          <div className="room-results">
            {loading && <div className="loading">Loading rooms...</div>}
            {!loading && roomResults.length === 0 && (
              <div className="empty">No rooms found</div>
            )}
            {roomResults.map((hit) => (
              <div
                key={hit.roomId}
                className="result-item"
                onClick={() => handleRoomClick(hit)}
              >
                <span className="result-type">
                  {hit.roomType === 'c' ? '#' : '@'}
                </span>
                <span className="result-name">{hit.roomName}</span>
              </div>
            ))}
          </div>
        )}

        {activeTab === 'messages' && (
          <div className="message-results">
            {loading && <div className="loading">Loading messages...</div>}
            {!loading && msgResults.length === 0 && (
              <div className="empty">No messages found</div>
            )}
            {msgResults.map((hit) => (
              <div
                key={hit.messageId}
                className="result-item"
                onClick={() => handleMessageClick(hit)}
              >
                <div className="msg-content">{hit.content}</div>
                <div className="msg-meta">
                  {hit.userAccount} · {new Date(hit.createdAt).toLocaleString()}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
```

- [ ] **Step 4: Run SearchResultsPane tests**

```bash
npm test -- src/pages/SearchResultsPane.test.jsx
```

Expected: All tests pass.

- [ ] **Step 5: Update ChatPage.jsx to render SearchResultsPane**

In `chat-frontend/src/pages/ChatPage.jsx`, replace the conditional in chat-main:

```javascript
const [searchQuery, setSearchQuery] = useState(null)

// ... in render, replace the chat-main section:
<div className="chat-main">
  {searchQuery ? (
    <SearchResultsPane
      query={searchQuery}
      onClose={() => setSearchQuery(null)}
      onSelectRoom={handleSelectRoom}
      onJumpToMessage={() => {}} // Will be implemented in Task 6
    />
  ) : (
    <>
      <MessageArea room={selectedRoom} />
      <MessageInput room={selectedRoom} />
    </>
  )}
</div>
```

Also add import:
```javascript
import SearchResultsPane from './SearchResultsPane'
```

- [ ] **Step 6: Update ChatPage.test.jsx**

Add SearchResultsPane mock:

```javascript
vi.mock('./SearchResultsPane', () => ({
  default: ({ onClose }) => (
    <div
      data-testid="search-results"
      onClick={() => onClose()}
    >
      Search Results
    </div>
  ),
}))
```

Run: `npm test -- src/pages/ChatPage.test.jsx` → should pass

- [ ] **Step 7: Commit**

```bash
git add chat-frontend/src/lib/subjects.js chat-frontend/src/lib/subjects.test.js \
  chat-frontend/src/pages/SearchResultsPane.jsx chat-frontend/src/pages/SearchResultsPane.test.jsx \
  chat-frontend/src/pages/ChatPage.jsx chat-frontend/src/pages/ChatPage.test.jsx

git commit -m "feat(chat-frontend): search results pane with Rooms and Messages tabs

- Added searchMessages(account) subject builder
- SearchResultsPane: split view with Rooms (loaded immediately) and Messages (lazy-loaded on tab click)
- Click room result → navigate to room + close pane
- Click message result → calls onJumpToMessage (wiring in Task 6)
- Search results replace main pane; sidebar stays visible
- Tests: tab switching, lazy message load, room/message click handling

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 6: Implement jumpToMessage with surrounding messages + historical buffer mode

**Files:**
- Modify: `chat-frontend/src/lib/subjects.js`
- Modify: `chat-frontend/src/lib/subjects.test.js`
- Modify: `chat-frontend/src/lib/roomEventsReducer.js`
- Modify: `chat-frontend/src/lib/roomEventsReducer.test.js`
- Modify: `chat-frontend/src/context/RoomEventsContext.jsx`
- Modify: `chat-frontend/src/context/RoomEventsContext.test.jsx`
- Modify: `chat-frontend/src/components/MessageArea.jsx`
- Modify: `chat-frontend/src/components/MessageArea.test.jsx`
- Modify: `chat-frontend/src/pages/ChatPage.jsx`
- Modify: `chat-frontend/src/styles/index.css`

- [ ] **Step 1: Add msgSurrounding to subjects and write test**

Update `chat-frontend/src/lib/subjects.js`:

```javascript
export function msgSurrounding(account, roomId, siteId) {
  return `chat.user.${account}.request.room.${roomId}.${siteId}.msg.surrounding`
}
```

Update `chat-frontend/src/lib/subjects.test.js`:

```javascript
it('msgSurrounding builds the surrounding messages request subject', () => {
  expect(msgSurrounding('alice', 'r1', 'site-A')).toBe(
    'chat.user.alice.request.room.r1.site-A.msg.surrounding'
  )
})
```

Run: `npm test -- src/lib/subjects.test.js` → PASS

- [ ] **Step 2: Write failing reducer test for new actions**

Add to `chat-frontend/src/lib/roomEventsReducer.test.js`:

```javascript
describe('roomEventsReducer: buffer mode actions', () => {
  it('REPLACE_ROOM_BUFFER sets messages, bufferMode=historical, focusMessageId', () => {
    const msgs = [
      { id: 'm0', content: 'before', createdAt: '2026-04-17T09:00:00Z' },
      { id: 'm1', content: 'focus', createdAt: '2026-04-17T10:00:00Z' },
      { id: 'm2', content: 'after', createdAt: '2026-04-17T11:00:00Z' },
    ]
    const state = { ...initialState, roomState: { a: emptyRoomState() } }
    const next = roomEventsReducer(state, {
      type: 'REPLACE_ROOM_BUFFER',
      roomId: 'a',
      messages: msgs,
      focusMessageId: 'm1',
    })
    expect(next.roomState.a.messages).toEqual(msgs)
    expect(next.roomState.a.bufferMode).toBe('historical')
    expect(next.roomState.a.focusMessageId).toBe('m1')
    expect(next.roomState.a.pendingLiveMessages).toEqual([])
  })

  it('RESET_TO_LIVE_TAIL merges pending messages and resets mode', () => {
    const state = {
      ...initialState,
      roomState: {
        a: {
          ...emptyRoomState(),
          messages: [
            { id: 'm0', content: 'old', createdAt: '2026-04-17T09:00:00Z' },
          ],
          bufferMode: 'historical',
          pendingLiveMessages: [
            { id: 'm1', content: 'new1', createdAt: '2026-04-17T10:00:00Z' },
            { id: 'm2', content: 'new2', createdAt: '2026-04-17T11:00:00Z' },
          ],
          focusMessageId: 'm0',
        },
      },
    }
    const next = roomEventsReducer(state, {
      type: 'RESET_TO_LIVE_TAIL',
      roomId: 'a',
    })
    expect(next.roomState.a.messages.map((m) => m.id)).toEqual(['m0', 'm1', 'm2'])
    expect(next.roomState.a.bufferMode).toBe('live')
    expect(next.roomState.a.pendingLiveMessages).toEqual([])
    expect(next.roomState.a.focusMessageId).toBeNull()
  })
})
```

Run: `npm test -- src/lib/roomEventsReducer.test.js` → should fail (actions not implemented)

- [ ] **Step 3: Update roomEventsReducer.js**

In `emptyRoomState()`, add new fields:

```javascript
function emptyRoomState() {
  return {
    messages: [],
    hasLoadedHistory: false,
    historyError: null,
    unreadCount: 0,
    hasMention: false,
    mentionAll: false,
    lastMsgAt: null,
    lastMsgId: null,
    bufferMode: 'live',       // NEW
    pendingLiveMessages: [],  // NEW
    focusMessageId: null,     // NEW
  }
}
```

In `MESSAGE_RECEIVED` case, check bufferMode:

```javascript
case 'MESSAGE_RECEIVED': {
  const evt = action.event
  const roomId = evt.roomId
  const prev = state.roomState[roomId] ?? emptyRoomState()

  // If in historical mode, stash in pending instead of appending
  if (prev.bufferMode === 'historical') {
    const pending = [...prev.pendingLiveMessages, evt.message]
    return {
      ...state,
      roomState: { ...state.roomState, [roomId]: { ...prev, pendingLiveMessages: pending } },
    }
  }

  // Normal live mode: append to messages
  if (prev.messages.some((m) => m.id === evt.message.id)) return state
  const messages = appendBounded(prev.messages, evt.message)
  const isActive = state.activeRoomId === roomId
  // ... rest of existing logic
}
```

Add two new action cases before the `default`:

```javascript
case 'REPLACE_ROOM_BUFFER': {
  const prev = state.roomState[action.roomId] ?? emptyRoomState()
  return {
    ...state,
    roomState: {
      ...state.roomState,
      [action.roomId]: {
        ...prev,
        messages: action.messages,
        bufferMode: 'historical',
        pendingLiveMessages: [],
        focusMessageId: action.focusMessageId,
      },
    },
  }
}

case 'RESET_TO_LIVE_TAIL': {
  const prev = state.roomState[action.roomId] ?? emptyRoomState()
  let messages = prev.messages
  for (const msg of prev.pendingLiveMessages) {
    messages = appendBounded(messages, msg)
  }
  return {
    ...state,
    roomState: {
      ...state.roomState,
      [action.roomId]: {
        ...prev,
        messages,
        bufferMode: 'live',
        pendingLiveMessages: [],
        focusMessageId: null,
      },
    },
  }
}
```

- [ ] **Step 4: Run reducer tests**

```bash
npm test -- src/lib/roomEventsReducer.test.js
```

Expected: All tests pass (including new buffer mode tests).

- [ ] **Step 5: Write failing RoomEventsContext test**

Add to `chat-frontend/src/context/RoomEventsContext.test.jsx`:

```javascript
it('jumpToMessage fetches surrounding messages and updates buffer', async () => {
  const surrounding = [
    { id: 'm0', content: 'before', createdAt: '2026-04-17T09:00:00Z', sender: { account: 'bob' } },
    { id: 'm1', content: 'central', createdAt: '2026-04-17T10:00:00Z', sender: { account: 'alice' } },
    { id: 'm2', content: 'after', createdAt: '2026-04-17T11:00:00Z', sender: { account: 'bob' } },
  ]
  const request = vi.fn().mockImplementation((subject) => {
    if (subject.includes('.msg.surrounding')) {
      return Promise.resolve({
        messages: surrounding,
        moreBefore: false,
        moreAfter: false,
      })
    }
    if (subject.endsWith('.rooms.list')) return Promise.resolve({ rooms: [] })
    throw new Error('unexpected: ' + subject)
  })
  const nats = mockNats({ request })

  function Trigger() {
    const { messages, bufferMode, focusMessageId } = useRoomEvents('a')
    const { jumpToMessage } = useRoomSummaries()
    return (
      <div>
        <button onClick={() => jumpToMessage('a', 'm1')}>jump</button>
        <div data-testid="mode">{bufferMode}</div>
        <div data-testid="focus">{focusMessageId ?? ''}</div>
        <div data-testid="messages">{messages.map((m) => m.id).join(',')}</div>
      </div>
    )
  }

  render(wrap(<Trigger />, nats))
  await act(async () => {
    screen.getByText('jump').click()
  })

  await waitFor(() => {
    expect(screen.getByTestId('mode').textContent).toBe('historical')
  })
  expect(screen.getByTestId('focus').textContent).toBe('m1')
  expect(screen.getByTestId('messages').textContent).toBe('m0,m1,m2')
})
```

- [ ] **Step 6: Update RoomEventsContext.jsx**

Add `jumpToMessage` and `resetToLiveTail` functions, then expose via hooks:

```javascript
const jumpToMessage = useCallback(
  async (roomId, messageId) => {
    if (!user) return
    try {
      const resp = await request(msgSurrounding(user.account, roomId, user.siteId), {
        messageId,
        limit: 50,
      })
      const messages = resp.messages ?? []
      dispatch({
        type: 'REPLACE_ROOM_BUFFER',
        roomId,
        messages,
        focusMessageId: messageId,
      })
    } catch (err) {
      slog.warn('jump to message failed', { error: err.message })
    }
  },
  [user, request]
)

const resetToLiveTail = useCallback((roomId) => {
  dispatch({ type: 'RESET_TO_LIVE_TAIL', roomId })
}, [])

const value = useMemo(
  () => ({ state, loadHistory, setActiveRoom, jumpToMessage, resetToLiveTail }),
  [state, loadHistory, setActiveRoom, jumpToMessage, resetToLiveTail]
)
```

Add import for msgSurrounding:

```javascript
import {
  msgHistory,
  roomEvent,
  roomsGet,
  roomsList,
  subscriptionUpdate,
  roomMetadataUpdate,
  userRoomEvent,
  msgSurrounding, // NEW
} from '../lib/subjects'
```

Update `useRoomSummaries` to expose `jumpToMessage`:

```javascript
export function useRoomSummaries() {
  const { state, setActiveRoom, jumpToMessage } = useRoomEventsInternal()
  return {
    summaries: state.summaries,
    setActiveRoom,
    error: state.roomsError,
    jumpToMessage, // NEW
  }
}
```

Update `useRoomEvents` to expose new buffer fields and methods:

```javascript
export function useRoomEvents(roomId) {
  const { state, loadHistory, jumpToMessage, resetToLiveTail } = useRoomEventsInternal()
  const room = state.roomState[roomId]
  const load = useCallback(() => loadHistory(roomId), [loadHistory, roomId])
  return useMemo(
    () => ({
      messages: room?.messages ?? [],
      hasLoadedHistory: !!room?.hasLoadedHistory,
      historyError: room?.historyError ?? null,
      bufferMode: room?.bufferMode ?? 'live',     // NEW
      pendingCount: room?.pendingLiveMessages?.length ?? 0,  // NEW
      focusMessageId: room?.focusMessageId ?? null,  // NEW
      loadHistory: load,
      jumpToMessage: useCallback((msgId) => jumpToMessage(roomId, msgId), [jumpToMessage, roomId]),  // NEW
      resetToLiveTail: useCallback(() => resetToLiveTail(roomId), [resetToLiveTail, roomId]),  // NEW
    }),
    [room, load, jumpToMessage, resetToLiveTail, roomId]
  )
}
```

- [ ] **Step 7: Run RoomEventsContext tests**

```bash
npm test -- src/context/RoomEventsContext.test.jsx
```

Expected: All tests pass (including new jumpToMessage test).

- [ ] **Step 8: Update MessageArea.jsx**

Add scroll-to-focus and flash animation. Import hook:

```javascript
const { messages, hasLoadedHistory, historyError, loadHistory, focusMessageId } = useRoomEvents(room?.id ?? null)
```

Add useEffect to scroll and flash:

```javascript
useEffect(() => {
  if (!focusMessageId) return
  const elem = document.querySelector(`[data-message-id="${focusMessageId}"]`)
  if (elem) {
    elem.scrollIntoView({ behavior: 'smooth', block: 'center' })
    elem.classList.add('flash-jump')
    const timer = setTimeout(() => elem.classList.remove('flash-jump'), 2000)
    return () => clearTimeout(timer)
  }
}, [focusMessageId])
```

Add data attribute to message div:

```javascript
<div key={msg.id} className="message" data-message-id={msg.id}>
  {/* ... existing content ... */}
</div>
```

- [ ] **Step 9: Add flash animation CSS**

Append to `chat-frontend/src/styles/index.css`:

```css
.message.flash-jump {
  animation: flash-jump 2s ease-out;
}

@keyframes flash-jump {
  0% {
    background: rgba(88, 101, 242, 0.2);
  }
  100% {
    background: transparent;
  }
}
```

- [ ] **Step 10: Update MessageArea.test.jsx**

Add tests for focus/flash behavior. Add to mock return:

```javascript
bufferMode: 'live',
pendingCount: 0,
focusMessageId: null,
jumpToMessage: vi.fn(),
resetToLiveTail: vi.fn(),
```

- [ ] **Step 11: Update ChatPage.jsx**

Add handleJumpToMessage and pass to SearchResultsPane:

```javascript
const { jumpToMessage } = useRoomSummaries() // Import from useRoomSummaries

const handleJumpToMessage = async (roomId, messageId) => {
  const room = summaries.find((r) => r.id === roomId)
  if (room) handleSelectRoom(room)
  setSearchQuery(null) // Close search results
  await jumpToMessage(roomId, messageId)
}

// In render, pass to SearchResultsPane:
<SearchResultsPane
  query={searchQuery}
  onClose={() => setSearchQuery(null)}
  onSelectRoom={handleSelectRoom}
  onJumpToMessage={handleJumpToMessage}  // NOW CONNECTED
/>
```

- [ ] **Step 12: Commit**

```bash
git add chat-frontend/src/lib/subjects.js chat-frontend/src/lib/subjects.test.js \
  chat-frontend/src/lib/roomEventsReducer.js chat-frontend/src/lib/roomEventsReducer.test.js \
  chat-frontend/src/context/RoomEventsContext.jsx chat-frontend/src/context/RoomEventsContext.test.jsx \
  chat-frontend/src/components/MessageArea.jsx chat-frontend/src/components/MessageArea.test.jsx \
  chat-frontend/src/pages/ChatPage.jsx chat-frontend/src/styles/index.css

git commit -m "feat(chat-frontend): jumpToMessage with LoadSurroundingMessages + historical buffer

- Added msgSurrounding(account, roomId, siteId) subject builder
- roomEventsReducer: added bufferMode (live|historical), pendingLiveMessages[], focusMessageId
- New actions: REPLACE_ROOM_BUFFER (jump to message), RESET_TO_LIVE_TAIL (back to live)
- MESSAGE_RECEIVED: stashes messages in pendingLiveMessages when in historical mode
- RoomEventsContext: jumpToMessage() fetches surrounding msgs, replaces buffer, sets focus
- MessageArea: scrolls to focusMessageId + flash yellow animation (2s fade)
- ChatPage.handleJumpToMessage wired to SearchResultsPane; closes search on jump
- Tests: reducer buffer mode, RoomEvents jumpToMessage, MessageArea scroll/flash

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 7: Add Ctrl+F in-room search strip

**Files:**
- Create: `chat-frontend/src/components/InRoomSearch.jsx`
- Create: `chat-frontend/src/components/InRoomSearch.test.jsx`
- Modify: `chat-frontend/src/components/MessageArea.jsx`
- Modify: `chat-frontend/src/components/MessageArea.test.jsx`

- [ ] **Step 1: Write failing InRoomSearch test**

Create `chat-frontend/src/components/InRoomSearch.test.jsx`:

```javascript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import InRoomSearch from './InRoomSearch'

vi.mock('../context/NatsContext', () => ({
  useNats: vi.fn(),
}))

import { useNats } from '../context/NatsContext'

describe('InRoomSearch', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('fetches message results after 300ms debounce', async () => {
    const request = vi.fn().mockResolvedValue({
      results: [
        { messageId: 'm1', content: 'hello world', createdAt: '2026-04-17T10:00:00Z', userAccount: 'bob' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<InRoomSearch roomId="r1" onClose={vi.fn()} onJumpToMessage={vi.fn()} />)

    fireEvent.change(screen.getByPlaceholderText(/Search messages/), { target: { value: 'hello' } })
    vi.advanceTimersByTime(300)

    await waitFor(() => {
      expect(request).toHaveBeenCalledWith(
        'chat.user.alice.request.search.messages',
        expect.objectContaining({ searchText: 'hello', roomIds: ['r1'] })
      )
    })

    await waitFor(() => {
      expect(screen.getByText('hello world')).toBeInTheDocument()
    })
  })

  it('clicking result calls onJumpToMessage', async () => {
    const onJumpToMessage = vi.fn()
    const request = vi.fn().mockResolvedValue({
      results: [
        { messageId: 'm1', content: 'test', createdAt: '2026-04-17T10:00:00Z', userAccount: 'alice' },
      ],
      total: 1,
    })
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request,
    })

    render(<InRoomSearch roomId="r1" onClose={vi.fn()} onJumpToMessage={onJumpToMessage} />)

    fireEvent.change(screen.getByPlaceholderText(/Search messages/), { target: { value: 'test' } })
    vi.advanceTimersByTime(300)

    await waitFor(() => {
      fireEvent.click(screen.getByText('test'))
    })

    expect(onJumpToMessage).toHaveBeenCalledWith('m1')
  })

  it('X button calls onClose', () => {
    useNats.mockReturnValue({
      user: { account: 'alice' },
      request: vi.fn(),
    })

    const onClose = vi.fn()
    render(<InRoomSearch roomId="r1" onClose={onClose} onJumpToMessage={vi.fn()} />)

    fireEvent.click(screen.getByRole('button', { name: '✕' }))
    expect(onClose).toHaveBeenCalled()
  })
})
```

- [ ] **Step 2: Create InRoomSearch.jsx**

Create `chat-frontend/src/components/InRoomSearch.jsx`:

```javascript
import { useState, useRef } from 'react'
import { useNats } from '../context/NatsContext'
import { searchMessages } from '../lib/subjects'

export default function InRoomSearch({ roomId, onClose, onJumpToMessage }) {
  const { user, request } = useNats()
  const [query, setQuery] = useState('')
  const [results, setResults] = useState([])
  const [loading, setLoading] = useState(false)
  const debounceRef = useRef(null)
  const inputRef = useRef(null)

  const handleChange = (e) => {
    const q = e.target.value
    setQuery(q)
    clearTimeout(debounceRef.current)

    if (q.length < 2) {
      setResults([])
      return
    }

    debounceRef.current = setTimeout(async () => {
      setLoading(true)
      try {
        const resp = await request(searchMessages(user.account), {
          searchText: q,
          roomIds: [roomId],
          size: 20,
        })
        setResults(resp.results ?? [])
      } catch {
        setResults([])
      } finally {
        setLoading(false)
      }
    }, 300)
  }

  const handleClick = (hit) => {
    onJumpToMessage(hit.messageId)
    onClose()
  }

  return (
    <div className="in-room-search-strip">
      <input
        ref={inputRef}
        type="text"
        className="in-room-search-input"
        value={query}
        onChange={handleChange}
        placeholder="Search messages..."
        autoFocus
      />
      <span className="search-count">
        {loading ? 'Searching...' : `${results.length} result${results.length !== 1 ? 's' : ''}`}
      </span>
      <button className="search-close" onClick={onClose} aria-label="Close search">
        ✕
      </button>

      {results.length > 0 && (
        <div className="search-results-inline">
          {results.map((hit) => (
            <div
              key={hit.messageId}
              className="inline-result"
              onClick={() => handleClick(hit)}
            >
              <div className="result-content">{hit.content}</div>
              <div className="result-meta">
                {hit.userAccount} · {new Date(hit.createdAt).toLocaleTimeString()}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 3: Run InRoomSearch tests**

```bash
npm test -- src/components/InRoomSearch.test.jsx
```

Expected: All tests pass.

- [ ] **Step 4: Update MessageArea.jsx**

Add state and Ctrl+F handler:

```javascript
const [ctrlFOpen, setCtrlFOpen] = useState(false)

useEffect(() => {
  const handler = (e) => {
    if (!room) return
    if ((e.ctrlKey || e.metaKey) && e.key === 'f') {
      e.preventDefault()
      setCtrlFOpen(true)
    }
    if (e.key === 'Escape') setCtrlFOpen(false)
  }
  window.addEventListener('keydown', handler)
  return () => window.removeEventListener('keydown', handler)
}, [room])
```

In render, add InRoomSearch inside message-list or as sibling:

```javascript
{ctrlFOpen && (
  <InRoomSearch
    roomId={room.id}
    onClose={() => setCtrlFOpen(false)}
    onJumpToMessage={(msgId) => jumpToMessage(msgId)}
  />
)}
```

Add import:
```javascript
import InRoomSearch from './InRoomSearch'
```

- [ ] **Step 5: Update MessageArea.test.jsx**

Add Ctrl+F test:

```javascript
it('Ctrl+F opens search strip', () => {
  useRoomEvents.mockReturnValue({
    messages: [],
    hasLoadedHistory: false,
    historyError: null,
    bufferMode: 'live',
    pendingCount: 0,
    focusMessageId: null,
    loadHistory: vi.fn().mockResolvedValue(),
    jumpToMessage: vi.fn(),
    resetToLiveTail: vi.fn(),
  })

  render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)

  fireEvent.keyDown(window, { ctrlKey: true, key: 'f' })
  expect(screen.getByText(/Search messages/)).toBeInTheDocument()
})
```

- [ ] **Step 6: Commit**

```bash
git add chat-frontend/src/components/InRoomSearch.jsx chat-frontend/src/components/InRoomSearch.test.jsx \
  chat-frontend/src/components/MessageArea.jsx chat-frontend/src/components/MessageArea.test.jsx

git commit -m "feat(chat-frontend): Ctrl+F in-room message search strip

- Added InRoomSearch component: search input + inline results list
- Debounced 300ms search in current room via search-service (RoomIDs filter)
- Ctrl+F (Win/Mac) opens strip; Esc closes strip
- Clicking result jumps to message and closes search
- Tests: debounce timing, result display, click handling, Ctrl+F keydown

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

### Task 8: Add jump-to-latest pill for historical slice mode

**Files:**
- Modify: `chat-frontend/src/components/MessageArea.jsx`
- Modify: `chat-frontend/src/components/MessageArea.test.jsx`

- [ ] **Step 1: Update MessageArea.jsx**

Add "Jump to latest" pill rendering. In the message list area, add:

```javascript
{bufferMode === 'historical' && pendingCount > 0 && (
  <div className="jump-latest-pill">
    <button onClick={() => resetToLiveTail()}>
      Jump to latest ({pendingCount} new)
    </button>
  </div>
)}
```

Also update useEffect to auto-scroll to bottom when returning to live mode:

```javascript
useEffect(() => {
  if (bufferMode === 'live') {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }
}, [bufferMode])
```

Extract `bufferMode`, `pendingCount`, `resetToLiveTail` from the hook:

```javascript
const {
  messages,
  hasLoadedHistory,
  historyError,
  loadHistory,
  bufferMode,
  pendingCount,
  focusMessageId,
  jumpToMessage,
  resetToLiveTail,
} = useRoomEvents(room?.id ?? null)
```

- [ ] **Step 2: Add CSS for the pill**

Append to `chat-frontend/src/styles/index.css`:

```css
.jump-latest-pill {
  padding: 0.75rem;
  margin: 0.5rem;
  background: #5865f2;
  border-radius: 24px;
  text-align: center;
}

.jump-latest-pill button {
  background: none;
  border: none;
  color: white;
  cursor: pointer;
  font-size: 13px;
  font-weight: 500;
}

.jump-latest-pill button:hover {
  text-decoration: underline;
}
```

- [ ] **Step 3: Write test**

Add to `chat-frontend/src/components/MessageArea.test.jsx`:

```javascript
it('shows jump-to-latest pill when in historical mode with pending messages', () => {
  const resetToLiveTail = vi.fn()
  useRoomEvents.mockReturnValue({
    messages: [
      { id: 'm1', content: 'old', createdAt: '2026-04-17T09:00:00Z', sender: { account: 'bob' } },
    ],
    hasLoadedHistory: true,
    historyError: null,
    bufferMode: 'historical',
    pendingCount: 3,
    focusMessageId: 'm1',
    loadHistory: vi.fn(),
    jumpToMessage: vi.fn(),
    resetToLiveTail,
  })

  render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)

  expect(screen.getByText(/Jump to latest \(3 new\)/)).toBeInTheDocument()

  fireEvent.click(screen.getByRole('button', { name: /Jump to latest/ }))
  expect(resetToLiveTail).toHaveBeenCalled()
})

it('hides jump-to-latest pill when in live mode', () => {
  useRoomEvents.mockReturnValue({
    messages: [],
    hasLoadedHistory: false,
    historyError: null,
    bufferMode: 'live',
    pendingCount: 0,
    focusMessageId: null,
    loadHistory: vi.fn(),
    jumpToMessage: vi.fn(),
    resetToLiveTail: vi.fn(),
  })

  render(<MessageArea room={{ id: 'r1', name: 'general', type: 'channel', userCount: 2 }} />)

  expect(screen.queryByText(/Jump to latest/)).not.toBeInTheDocument()
})
```

- [ ] **Step 4: Run tests**

```bash
npm test -- src/components/MessageArea.test.jsx
```

Expected: All tests pass (including pill tests).

- [ ] **Step 5: Commit**

```bash
git add chat-frontend/src/components/MessageArea.jsx chat-frontend/src/components/MessageArea.test.jsx \
  chat-frontend/src/styles/index.css

git commit -m "feat(chat-frontend): jump-to-latest pill for historical message buffer

- Show pill when bufferMode=historical and pendingCount > 0
- Clicking pill calls resetToLiveTail, merges pending messages, scrolls to bottom
- Auto-scroll to bottom when transitioning back to live mode
- Styled as blue rounded pill with white text
- Tests: pill display in historical mode, click handling, hidden in live mode

https://claude.ai/code/session_01PG1wK1WFz71QzuLNS83rak"
```

---

## Execution Checklist

Implement tasks 1–8 in order. After each task commits:
1. Verify all tests pass: `npm test`
2. Test manually in Vite dev server if applicable: `npm run dev`
3. Commit with the provided message

**After all 8 tasks:**
- [ ] Run full test suite: `npm test` (target ≥80% coverage on modified files)
- [ ] Run `npm run build` (no errors)
- [ ] Manual smoke test:
  - DEV_MODE=true login with account
  - DEV_MODE=false login with Keycloak (test realms alice + bob, password=`password`)
  - Search bar: type "fro" → dropdown shows rooms, click navigates, Enter shows search pane
  - Search results: Rooms tab instant, Messages tab lazy-loaded
  - Click message result → room selected + jump-to-message scrolls + flash
  - Ctrl+F in room → search strip, type, click result jumps
  - Jump latest pill appears when viewing historical slice, click merges

**Done!** Push branch to `claude/review-sleepy-swartz-mp0HQ` when all tasks complete.
