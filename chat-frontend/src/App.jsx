import { useEffect, useState, useCallback } from 'react'
import { NatsProvider, useNats } from '@/context/NatsContext'
import { RoomEventsProvider } from '@/context/RoomEventsContext'
import { ThreadEventsProvider } from '@/context/ThreadEventsContext'
import LoginPage from '@/pages/LoginPage'
import MainApp from '@/components/MainApp/MainApp'
import OidcCallback from '@/pages/OidcCallback'
import ErrorBoundary from '@/components/shared/ErrorBoundary'

function AppContent() {
  const { connected } = useNats()
  const [pathname, setPathname] = useState(
    typeof window !== 'undefined' ? window.location.pathname : '/'
  )

  useEffect(() => {
    const onPop = () => setPathname(window.location.pathname)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  // OidcCallback calls onDone() after it finishes; we use that to refresh
  // our local view of window.location.pathname, since history.replaceState
  // doesn't fire popstate.
  const handleOidcDone = useCallback(() => {
    setPathname(window.location.pathname)
  }, [])

  if (pathname === '/oidc-callback') {
    return <OidcCallback onDone={handleOidcDone} />
  }

  if (!connected) {
    return <LoginPage />
  }

  return (
    <RoomEventsProvider>
      <ThreadEventsProvider>
        <MainApp />
      </ThreadEventsProvider>
    </RoomEventsProvider>
  )
}

export default function App() {
  // The boundary wraps NatsProvider so an error inside the provider's
  // initial render (e.g. a malformed runtime config) also caught.
  return (
    <ErrorBoundary>
      <NatsProvider>
        <AppContent />
      </NatsProvider>
    </ErrorBoundary>
  )
}
