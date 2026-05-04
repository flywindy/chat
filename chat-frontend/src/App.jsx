import { useEffect, useState, useCallback } from 'react'
import { NatsProvider, useNats } from './context/NatsContext'
import { RoomEventsProvider } from './context/RoomEventsContext'
import LoginPage from './pages/LoginPage'
import ChatPage from './pages/ChatPage'
import OidcCallback from './pages/OidcCallback'

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
