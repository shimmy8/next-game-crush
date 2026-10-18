import type { CurrentUser } from '../api/client'

type LoginPageProps = { user: CurrentUser | null; signingOut: boolean; onLogout: () => void; demoEnabled?: boolean }

export default function LoginPage({ user, signingOut, onLogout, demoEnabled = false }: LoginPageProps) {
  return (
    <section className="w-full rounded-3xl border border-white/10 bg-white/5 p-8 sm:p-12">
      <p className="mb-5 text-sm font-medium uppercase tracking-widest text-violet-300">Ваш следующий фаворит уже где-то рядом</p>
      <h1 className="text-4xl font-semibold tracking-tight sm:text-5xl">Next Game Crush</h1>
      <p className="mt-5 max-w-xl text-lg leading-relaxed text-slate-300">Находите будущие релизы Steam с учётом игр, которые вы любите.</p>
      {user ? (
        <div className="mt-8">
          <p className="text-slate-200">{user.accessMode === 'demo' ? 'Вы вошли в демо-режиме' : `Вы вошли как Steam ID ${user.steamId}`}</p>
          <button type="button" onClick={onLogout} disabled={signingOut} className="mt-5 rounded-xl border border-white/20 px-5 py-3 font-semibold text-white disabled:opacity-50">
            {signingOut ? 'Выходим…' : 'Выйти'}
          </button>
        </div>
      ) : (
        <div className="mt-8">
          <p className="max-w-xl text-slate-300">Войдите через Steam, чтобы импортировать открытую библиотеку игр, время в играх, недавнюю активность, список желаемого и сводку достижений. Next Game Crush никогда не запрашивает ваш пароль Steam.</p>
          <div className="login-actions">
            <a href="/auth/steam" className="rounded-xl bg-violet-400 px-5 py-3 font-semibold text-slate-950">Войти через Steam</a>
            {demoEnabled && <form action="/auth/demo" method="post"><button type="submit" formAction="/auth/demo" className="demo-login-button">Войти в демо-режим</button></form>}
          </div>
          <p className="mt-4 text-sm text-slate-400">Steam OpenID подтверждает владение аккаунтом, но не открывает доступ к закрытым данным профиля. Закрытые или недоступные источники будут отмечены как недоступные, а не показаны как пустые.</p>
        </div>
      )}
    </section>
  )
}
