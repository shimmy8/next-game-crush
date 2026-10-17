import type { CurrentUser } from '../api/client'

type LoginPageProps = { user: CurrentUser | null; signingOut: boolean; onLogout: () => void }

export default function LoginPage({ user, signingOut, onLogout }: LoginPageProps) {
  return (
    <section className="w-full rounded-3xl border border-white/10 bg-white/5 p-8 sm:p-12">
      <p className="mb-5 text-sm font-medium uppercase tracking-widest text-violet-300">Your next favorite is out there</p>
      <h1 className="text-4xl font-semibold tracking-tight sm:text-5xl">Next Game Crush</h1>
      <p className="mt-5 max-w-xl text-lg leading-relaxed text-slate-300">Discover upcoming Steam releases shaped by the games you love.</p>
      {user ? (
        <div className="mt-8">
          <p className="text-slate-200">Signed in as SteamID {user.steamId}</p>
          <button type="button" onClick={onLogout} disabled={signingOut} className="mt-5 rounded-xl border border-white/20 px-5 py-3 font-semibold text-white disabled:opacity-50">
            {signingOut ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      ) : (
        <div className="mt-8">
          <p className="max-w-xl text-slate-300">Sign in through Steam to import your public game library, playtime, recent activity, wishlist, and achievement summaries. Next Game Crush never asks for your Steam password.</p>
          <a href="/auth/steam" className="mt-6 inline-block rounded-xl bg-violet-400 px-5 py-3 font-semibold text-slate-950">Sign in with Steam</a>
          <p className="mt-4 text-sm text-slate-400">Steam OpenID proves account ownership but cannot unlock private profile data. Private or unavailable sources will be shown as unavailable rather than treated as empty.</p>
        </div>
      )}
    </section>
  )
}
