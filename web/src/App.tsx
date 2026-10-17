import { useEffect, useState } from 'react'
import { getCurrentUser, logout, type CurrentUser } from './api/client'
import LoginPage from './pages/LoginPage'
import DashboardPage from './pages/DashboardPage'

type Health = 'checking' | 'ok' | 'unavailable'

export default function App() {
  const [health, setHealth] = useState<Health>('checking')
  const [user, setUser] = useState<CurrentUser | null>(null)
  const [signingOut, setSigningOut] = useState(false)

  useEffect(() => {
    const controller = new AbortController()
    async function checkHealth() {
      try {
        const response = await fetch('/api/health', { signal: controller.signal })
        if (!response.ok) throw new Error('Health request failed')
        const data: { status?: string } = await response.json()
        setHealth(data.status === 'ok' ? 'ok' : 'unavailable')
      } catch {
        if (!controller.signal.aborted) setHealth('unavailable')
      }
    }
    void checkHealth()
    void getCurrentUser(controller.signal).then(setUser).catch(() => {
      if (!controller.signal.aborted) setUser(null)
    })
    return () => controller.abort()
  }, [])

  async function signOut() {
    if (!user) return
    setSigningOut(true)
    try {
      await logout(user.csrfToken)
      setUser(null)
    } finally {
      setSigningOut(false)
    }
  }

  if (user) return <DashboardPage user={user} signingOut={signingOut} onLogout={() => { void signOut() }} onDataDeleted={() => setUser(null)} />

  return <main className="login-shell"><div className="login-shell__inner"><LoginPage user={null} signingOut={signingOut} onLogout={() => { void signOut() }} /><p role="status" className="api-health">API: {health === 'checking' ? 'проверяем…' : health === 'ok' ? 'на связи' : 'недоступен'}</p></div></main>
}
