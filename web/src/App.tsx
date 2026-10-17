import { useEffect, useState } from 'react'

type Health = 'checking' | 'ok' | 'unavailable'

export default function App() {
  const [health, setHealth] = useState<Health>('checking')

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
    return () => controller.abort()
  }, [])

  return (
    <main className="mx-auto flex min-h-screen max-w-3xl items-center px-6 py-16">
      <section className="w-full rounded-3xl border border-white/10 bg-white/5 p-8 sm:p-12">
        <p className="mb-5 text-sm font-medium uppercase tracking-widest text-violet-300">
          Your next favorite is out there
        </p>
        <h1 className="text-4xl font-semibold tracking-tight sm:text-5xl">Next Game Crush</h1>
        <p className="mt-5 max-w-xl text-lg leading-relaxed text-slate-300">
          Discover upcoming Steam releases shaped by the games you love.
        </p>
        <p role="status" className="mt-8 text-sm text-slate-300">
          API health: {health === 'checking' ? 'checking…' : health}
        </p>
        <button
          type="button"
          disabled
          aria-describedby="login-availability"
          className="mt-6 rounded-xl bg-violet-400 px-5 py-3 font-semibold text-slate-950 disabled:cursor-not-allowed disabled:opacity-50"
        >
          Sign in with Steam
        </button>
        <p id="login-availability" className="mt-3 text-sm text-slate-400">
          Steam sign-in is coming soon.
        </p>
      </section>
    </main>
  )
}
