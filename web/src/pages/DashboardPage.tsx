import { useCallback, useMemo, useState } from 'react'
import type { CurrentUser } from '../api/client'
import { useDashboardData, useSync } from '../api/hooks'
import type { CalendarResponse, DashboardLoadState, RankingDiff, Recommendation } from '../api/types'
import PreferenceProfile from '../components/PreferenceProfile'
import RecommendationCard from '../components/RecommendationCard'
import SyncProgress from '../components/SyncProgress'
import TestDigestButton from '../components/TestDigestButton'
import CalendarPage from './CalendarPage'

type DashboardPageProps = {
  user: CurrentUser
  signingOut: boolean
  onLogout: () => void
  initialState?: DashboardLoadState
}

function flatten(calendar: CalendarResponse) {
  return [...calendar.exactDates, ...calendar.month, ...calendar.quarter, ...calendar.year, ...calendar.comingSoon, ...calendar.tba]
    .sort((a, b) => b.score - a.score || a.name.localeCompare(b.name, 'ru'))
}

function UpcomingWindows({ calendar }: { calendar: CalendarResponse }) {
  const windows = [
    ['month', 'В этом месяце', calendar.month.length], ['quarter', 'В квартале', calendar.quarter.length],
    ['year', 'В этом году', calendar.year.length], ['soon', 'Coming Soon', calendar.comingSoon.length], ['tba', 'TBA', calendar.tba.length],
  ]
  return <aside className="windows-card"><div className="eyebrow">Без точного дня</div><h2>На горизонте</h2><ul>{windows.map(([key, label, count]) => <li key={key}><span>{label}</span><strong>{count}</strong></li>)}</ul><p>Мы не ставим игры в выдуманные даты.</p></aside>
}

export default function DashboardPage({ user, signingOut, onLogout, initialState }: DashboardPageProps) {
  const [rankingDiff, setRankingDiff] = useState<RankingDiff | null>(null)
  const { state, reload, emptyData } = useDashboardData(initialState)
  const refresh = useCallback(() => { void reload() }, [reload])
  const { sync, starting, requestError, run } = useSync(user.csrfToken, refresh)
  const rankingUpdated = useCallback((diff: RankingDiff) => { setRankingDiff(diff); void reload() }, [reload])
  const hasData = state.status === 'ready' || state.status === 'stale' || state.status === 'unknown' || state.status === 'empty'
  const data = hasData ? state.data : emptyData
  const recommendations = useMemo(() => flatten(data.calendar), [data.calendar])
  const best = recommendations[0]
  const freshness = {
    loading: ['Проверяем данные', 'checking'], error: ['Ошибка загрузки', 'error'], stale: ['Нужно обновить', 'stale'],
    unknown: ['Свежесть неизвестна', 'unknown'], empty: ['Профиль не собран', 'unknown'], ready: ['Данные актуальны', 'fresh'],
  }[state.status]

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">К содержанию</a>
    <header className="topbar">
      <a className="brand" href="/" aria-label="Next Game Crush"><span aria-hidden="true">N</span><strong>Next Game Crush</strong></a>
      <div className="topbar__actions">
        <span className={`freshness freshness--${freshness[1]}`}><i aria-hidden="true" />{freshness[0]}</span>
        <button className="secondary-button" type="button" onClick={() => { void run() }} disabled={starting}>{starting ? 'Обновляем…' : 'Синхронизировать'}</button>
        <details className="account-menu"><summary aria-label="Настройки профиля">•••</summary><p>Steam {user.steamId.slice(-6)}</p><button type="button" onClick={onLogout} disabled={signingOut}>{signingOut ? 'Выходим…' : 'Выйти'}</button></details>
      </div>
    </header>

    <main id="main-content">
      <section className="hero">
        <div className="eyebrow">Персональный игровой журнал</div>
        <h1>Во что влюбиться дальше</h1>
        <p>{hasData && data.profile?.summary ? data.profile.summary : 'Соберём вашу историю Steam и найдём будущие релизы, которые действительно попадают во вкус.'}</p>
        {hasData && data.profile && <span className="hero-confidence">Уверенность профиля · {Math.round(data.profile.confidence * 100)}%</span>}
      </section>

      <SyncProgress value={sync} requestError={requestError} />
      {state.status === 'loading' && <section className="state-card" role="status"><span className="spinner" aria-hidden="true" /><div><strong>Листаем вашу игровую историю</strong><p>Загружаем профиль и календарь…</p></div></section>}
      {state.status === 'error' && <section className="state-card state-card--error" role="alert"><div><strong>Дневник пока не открылся</strong><p>{state.message}</p></div><button type="button" onClick={() => { void reload() }}>Повторить</button></section>}
      {state.status === 'stale' && <section className="stale-banner" role="status"><strong>Показываем сохранённые рекомендации.</strong> {state.message}</section>}
      {state.status === 'unknown' && <section className="stale-banner stale-banner--unknown" role="status"><strong>Не удалось определить свежесть.</strong> {state.message}</section>}

      {hasData && <div className="dashboard-layout">
        <div className="dashboard-main">
          <section aria-labelledby="best-match-title">
            <div className="section-heading"><div><div className="eyebrow">Лучшее совпадение</div><h2 id="best-match-title">Ваш следующий фаворит</h2></div></div>
            {best ? <RecommendationCard item={best} featured csrfToken={user.csrfToken} onRankingUpdated={rankingUpdated} /> : <div className="empty-card"><strong>Пока рекомендаций нет</strong><p>Запустите синхронизацию — мы сопоставим любимые механики, темы и активность с будущими релизами.</p><button type="button" onClick={() => { void run() }} disabled={starting}>Собрать профиль</button></div>}
          </section>
          {recommendations.length > 1 && <section className="recommendations-section" aria-labelledby="more-title"><div className="section-heading"><div><div className="eyebrow">Ещё кандидаты</div><h2 id="more-title">Стоит присмотреться</h2></div><span>{recommendations.length - 1} игр</span></div><div className="recommendation-grid">{recommendations.slice(1, 7).map((item: Recommendation) => <RecommendationCard item={item} key={item.id} csrfToken={user.csrfToken} onRankingUpdated={rankingUpdated} />)}</div></section>}
        </div>
        <div className="right-rail"><PreferenceProfile profile={data.profile} /><UpcomingWindows calendar={data.calendar} /><TestDigestButton csrfToken={user.csrfToken} /></div>
      </div>}

      {hasData && <CalendarPage calendar={data.calendar} rankingDiff={rankingDiff} />}
    </main>
    <footer><span>Next Game Crush</span><span>Только ваш Steam · Локально и прозрачно</span></footer>
  </div>
}
