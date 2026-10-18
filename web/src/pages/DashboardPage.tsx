import { useCallback, useMemo, useState } from 'react'
import type { CurrentUser } from '../api/client'
import { useDashboardData, useSync } from '../api/hooks'
import type { CalendarResponse, CatalogStatus, DashboardLoadState, RankingDiff, WishlistCollection } from '../api/types'
import CatalogCoverage, { SourceTimestamp } from '../components/CatalogCoverage'
import PreferenceProfile from '../components/PreferenceProfile'
import RecommendationCard from '../components/RecommendationCard'
import SteamIdentity from '../components/SteamIdentity'
import SteamPreflightStatus from '../components/SteamPreflightStatus'
import SyncProgress from '../components/SyncProgress'
import TestDigestButton from '../components/TestDigestButton'
import CalendarPage from './CalendarPage'
import DiagnosticsPage from './DiagnosticsPage'
import FeedbackWorkflowProvider from '../components/FeedbackWorkflowProvider'
import { useFeedbackWorkflows } from '../components/FeedbackWorkflowContext'
import { applyFeedbackTombstones } from '../components/feedbackVisibility'

type DashboardPageProps = {
  user: CurrentUser
  signingOut: boolean
  onLogout: () => void
  onDataDeleted?: () => void
  initialState?: DashboardLoadState
}

const discoveryEmptyCopy: Record<CatalogStatus['status'], string> = {
  ready: 'Текущий ограниченный обход завершён. Подходящих новых игр пока нет.',
  backfilling: 'Поиск по каталогу продолжается — новые игры ещё могут появиться.',
  stale: 'Данные каталога устарели. Показываем результаты последнего сохранённого подбора.',
  unavailable: 'Каталог сейчас недоступен. Показываем результаты последнего сохранённого подбора.',
  disabled: 'Подбор использует проверенный локальный каталог. Автоматический поиск отключён.',
}

function WishlistFreshness({ wishlist }: { wishlist: WishlistCollection }) {
  const copy = {
    ready: 'Список желаемого актуален.',
    stale: 'Показываем сохранённый список желаемого. Обновить его пока не удалось.',
    unavailable: 'Список желаемого недоступен. Успешной загрузки ещё не было.',
    unknown: 'Список желаемого ещё не проверен.',
  }[wishlist.status]
  return <div className={`wishlist-freshness wishlist-freshness--${wishlist.status}`}>
    <p>{copy}</p>
    {wishlist.degradedReason === 'private_or_unavailable' && <p>Steam не предоставил доступ к списку желаемого.</p>}
    {wishlist.degradedReason === 'request_failed' && <p>Запрос к Steam завершился ошибкой.</p>}
    {wishlist.lastSuccessfulFetchAt && <p>Последняя успешная загрузка: <SourceTimestamp value={wishlist.lastSuccessfulFetchAt} /></p>}
  </div>
}

function UpcomingWindows({ calendar }: { calendar: CalendarResponse }) {
  const windows = [
    ['month', 'В этом месяце', calendar.month.length], ['quarter', 'В квартале', calendar.quarter.length],
    ['year', 'В этом году', calendar.year.length], ['soon', 'Скоро', calendar.comingSoon.length], ['tba', 'Дата уточняется', calendar.tba.length],
  ]
  return <aside className="windows-card"><div className="eyebrow">Без точного дня</div><h2>На горизонте</h2><ul>{windows.map(([key, label, count]) => <li key={key}><span>{label}</span><strong>{count}</strong></li>)}</ul><p>Мы не ставим игры в выдуманные даты.</p></aside>
}

function DashboardContent({ user, signingOut, onLogout, onDataDeleted = onLogout, dashboard, rankingDiff, setRankingDiff }: DashboardPageProps & { dashboard: ReturnType<typeof useDashboardData>; rankingDiff: RankingDiff | null; setRankingDiff(diff: RankingDiff): void }) {
  const [readOnlyNotice, setReadOnlyNotice] = useState('')
  const { state, reload, emptyData } = dashboard
  const workflows = useFeedbackWorkflows()
  const refresh = useCallback(() => { void reload() }, [reload])
  const { sync, starting, requestError, preflight, preflightError, checking, run } = useSync(user.csrfToken, refresh)
  const rankingUpdated = useCallback((diff: RankingDiff) => { setRankingDiff(diff); void reload() }, [reload, setRankingDiff])
  const hasData = state.status === 'ready' || state.status === 'stale' || state.status === 'unknown' || state.status === 'empty'
	const sourceData = hasData ? state.data : emptyData
  const sourceGameIds = useMemo(() => new Set([sourceData.overview.discoveries.featured, ...sourceData.overview.discoveries.items, ...sourceData.overview.wishlist.items, ...Object.values(sourceData.overview.calendar).flat()].filter((item) => item).map((item) => item!.gameId)), [sourceData.overview])
  const tombstones = useMemo(() => new Set([...sourceGameIds].filter(workflows.isTombstoned)), [sourceGameIds, workflows])
	const data = useMemo(() => ({ ...sourceData, overview: applyFeedbackTombstones(sourceData.overview, tombstones) }), [sourceData, tombstones])
  const { discoveries, wishlist, calendar, catalogStatus } = data.overview
  const displayName = preflight?.displayName ?? user.displayName
  const avatarUrl = preflight?.avatarUrl ?? user.avatarUrl
  const syncBusy = checking || starting
  const readOnly = user.accessMode === 'demo'
  const requestSync = () => { if (readOnly) setReadOnlyNotice('Демо-режим только для просмотра — синхронизация доступна владельцу.'); else void run() }
  const freshness = {
    loading: ['Проверяем данные', 'checking'], error: ['Ошибка загрузки', 'error'], stale: ['Нужно обновить', 'stale'],
    unknown: ['Свежесть неизвестна', 'unknown'], empty: ['Профиль не собран', 'unknown'], ready: ['Данные актуальны', 'fresh'],
  }[state.status]

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">К содержанию</a>
    {readOnly && <section className="demo-banner" role="status" aria-label="Демо-режим"><strong>Демо-режим.</strong> Вы смотрите реальные данные владельца без возможности менять их или оставлять реакции.</section>}
    <header className="topbar">
      <a className="brand" href="/" aria-label="Next Game Crush"><span aria-hidden="true">N</span><strong>Next Game Crush</strong></a>
      <div className="topbar__actions">
        <span className={`freshness freshness--${freshness[1]}`}><i aria-hidden="true" />{freshness[0]}</span>
        <button className="secondary-button" type="button" onClick={requestSync} disabled={syncBusy}>{checking ? 'Проверяем…' : starting ? 'Обновляем…' : 'Синхронизировать'}</button>
        <details className="account-menu"><summary aria-label="Настройки профиля"><SteamIdentity displayName={displayName} avatarUrl={avatarUrl} compact /></summary><button type="button" onClick={onLogout} disabled={signingOut}>{signingOut ? 'Выходим…' : 'Выйти'}</button></details>
      </div>
    </header>

    <main id="main-content">
      {readOnlyNotice && <p className="demo-global-notice" role="status">{readOnlyNotice}</p>}
      <SteamPreflightStatus checking={checking} preflight={preflight} error={preflightError} onRetry={requestSync} />
      <SyncProgress value={sync} requestError={requestError} />
      {state.status === 'loading' && <section className="state-card" role="status"><span className="spinner" aria-hidden="true" /><div><strong>Листаем вашу игровую историю</strong><p>Загружаем профиль и календарь…</p></div></section>}
      {state.status === 'error' && <section className="state-card state-card--error" role="alert"><div><strong>Дневник пока не открылся</strong><p>{state.message}</p></div><button type="button" onClick={() => { void reload() }}>Повторить</button></section>}
      {state.status === 'stale' && <section className="stale-banner" role="status"><strong>Показываем сохранённые рекомендации.</strong> {state.message}</section>}
      {state.status === 'unknown' && <section className="stale-banner stale-banner--unknown" role="status"><strong>Не удалось определить свежесть.</strong> {state.message}</section>}

      {hasData && <div className="dashboard-layout">
        <div className="dashboard-main">
          <section aria-labelledby="best-match-title">
            <div className="section-heading"><div><div className="eyebrow">Лучшее совпадение</div><h1 id="best-match-title">Новая находка для вас</h1></div></div>
            {discoveries.featured ? <RecommendationCard item={discoveries.featured} featured feedbackEnabled readOnly={readOnly} /> : <div className="empty-card"><strong>Пока не нашли новых игр вне вашего списка желаемого</strong><p>{discoveryEmptyCopy[catalogStatus.status]}</p><p>Синхронизируйте профиль — мы сопоставим любимые механики, темы и активность с будущими релизами.</p><button type="button" onClick={requestSync} disabled={syncBusy}>Собрать профиль</button></div>}
          </section>
          <section className="recommendations-section" aria-labelledby="more-title"><div className="section-heading"><div><div className="eyebrow">Вне вашего желаемого</div><h2 id="more-title">Ещё новые игры</h2></div><span>Всего новых игр: {discoveries.total}</span></div>{discoveries.items.length > 0 && <div className="recommendation-grid">{discoveries.items.map((item) => <RecommendationCard item={item} key={item.id} feedbackEnabled readOnly={readOnly} />)}</div>}</section>
          <section className="recommendations-section wishlist-section" aria-labelledby="wishlist-title"><div className="section-heading"><div><div className="eyebrow">Следим за релизами</div><h2 id="wishlist-title">Уже в вашем желаемом</h2></div><span>Всего в желаемом: {wishlist.total}</span></div><WishlistFreshness wishlist={wishlist} />{wishlist.items.length > 0 ? <div className="recommendation-grid">{wishlist.items.map((item) => <RecommendationCard item={item} key={item.id} feedbackEnabled readOnly={readOnly} />)}</div> : wishlist.status === 'ready' ? <p className="wishlist-empty">В желаемом нет подходящих будущих релизов.</p> : null}</section>
        </div>
        <div className="right-rail"><PreferenceProfile profile={data.profile} csrfToken={user.csrfToken} onRankingUpdated={rankingUpdated} readOnly={readOnly} /><CatalogCoverage status={catalogStatus} /><UpcomingWindows calendar={calendar} /><TestDigestButton csrfToken={user.csrfToken} readOnly={readOnly} /><DiagnosticsPage csrfToken={user.csrfToken} onDeleted={onDataDeleted} readOnly={readOnly} /></div>
      </div>}

      {hasData && <CalendarPage calendar={calendar} rankingDiff={rankingDiff} feedbackEnabled readOnly={readOnly} />}
    </main>
    <footer><span>Next Game Crush</span><span>Только ваш Steam · Локально и прозрачно</span></footer>
  </div>
}

export default function DashboardPage(props: DashboardPageProps) {
  const dashboard = useDashboardData(props.initialState)
  const [rankingDiff, setRankingDiff] = useState<RankingDiff | null>(null)
  const rankingUpdated = useCallback((diff: RankingDiff) => setRankingDiff(diff), [])
  return <FeedbackWorkflowProvider csrfToken={props.user.csrfToken} reload={dashboard.reload} onRankingUpdated={rankingUpdated}><DashboardContent {...props} dashboard={dashboard} rankingDiff={rankingDiff} setRankingDiff={setRankingDiff} /></FeedbackWorkflowProvider>
}
