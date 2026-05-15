import { useNats } from '@/context/NatsContext'
import SearchBar from './SearchBar/SearchBar'
import ThemeToggle from './ThemeToggle/ThemeToggle'
import './style.css'

export default function AppHeader({ onSelectRoom, onEnterSearch }) {
  const { user, disconnect } = useNats()

  return (
    <header className="app-header">
      <div className="app-header-search">
        <SearchBar onSelectRoom={onSelectRoom} onEnterSearch={onEnterSearch} />
      </div>
      <span className="app-header-user">
        {user?.account} · {user?.siteId}
      </span>
      <ThemeToggle />
      <button type="button" className="app-header-logout" onClick={disconnect}>
        Logout
      </button>
    </header>
  )
}
