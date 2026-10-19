import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { CurrentUser } from '../api/client'
import { useDashboardData, useEntryRefresh, useSync } from '../api/hooks'
import type { CalendarResponse, CatalogStatus, DashboardLoadState, RankingDiff, Recommendation, RecommendationOverview, WishlistCollection } from '../api/types'
import CatalogCoverage, { SourceTimestamp } from '../components/CatalogCoverage'
import PreferenceProfile from '../components/PreferenceProfile'
import { ExplicitRecommendationSettings } from '../components/ExplicitRecommendationSettings'
import RecommendationCard from '../components/RecommendationCard'
import SteamIdentity from '../components/SteamIdentity'
import SteamPreflightStatus from '../components/SteamPreflightStatus'
import SyncProgress from '../components/SyncProgress'
import TestDigestButton from '../components/TestDigestButton'
import CalendarPage from './CalendarPage'
import DiagnosticsPage from './DiagnosticsPage'
import FeedbackWorkflowProvider from '../components/FeedbackWorkflowProvider'
import { useFeedbackWorkflows } from '../components/FeedbackWorkflowContext'
import type { FeedbackPresentationOrigin } from '../components/FeedbackWorkflowContext'
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

const verdictExitFallbackMs = 700
type DashboardLane = 'featured' | 'discoveries' | 'wishlist'
type DashboardFeedbackOrigin = Extract<FeedbackPresentationOrigin, { surface: 'dashboard' }>
type DashboardExitSnapshot = { item: Recommendation; lane: DashboardLane; index: number; successor?: { item: Recommendation; index: number } }
type DashboardPresentationState = { source: RecommendationOverview; tombstoneKey: string; reducedMotion: boolean; exits: Map<string, DashboardExitSnapshot>; completed: Set<string> }

function findDashboardSnapshot(overview: RecommendationOverview, gameId: string, origin: DashboardFeedbackOrigin): DashboardExitSnapshot | null {
  const matches = (item: Recommendation | null | undefined) => item?.gameId === gameId && item.id === origin.recommendationId
  if (origin.lane === 'featured') return matches(overview.discoveries.featured) ? { item: overview.discoveries.featured!, lane: 'featured', index: 0, ...(overview.discoveries.items[0] ? { successor: { item: overview.discoveries.items[0], index: 0 } } : {}) } : null
  const items = origin.lane === 'discoveries' ? overview.discoveries.items : overview.wishlist.items
  const index = items.findIndex(matches)
  return index >= 0 ? { item: items[index], lane: origin.lane, index } : null
}

function insertExitSnapshots(items: Recommendation[], exits: DashboardExitSnapshot[], lane: DashboardLane) {
  const result = [...items]
  for (const exit of exits.filter((value) => value.lane === lane).sort((left, right) => left.index - right.index)) {
    if (!result.some((item) => item.gameId === exit.item.gameId)) result.splice(Math.min(exit.index, result.length), 0, exit.item)
  }
  return result
}

function usePrefersReducedMotion() {
  const query = useMemo(() => typeof window.matchMedia === 'function' ? window.matchMedia('(prefers-reduced-motion: reduce)') : null, [])
  const [reduced, setReduced] = useState(() => query?.matches ?? false)
  useEffect(() => {
    if (!query) return
    const changed = (event: MediaQueryListEvent) => setReduced(event.matches)
    query.addEventListener('change', changed)
    return () => query.removeEventListener('change', changed)
  }, [query])
  return reduced
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
  const pendingVerdictTimers = useRef(new Map<string, number>())
  const refresh = useCallback(() => { void reload() }, [reload])
  const { sync, starting, requestError, preflight, preflightError, checking, run } = useSync(user.csrfToken, refresh)
  const entryRefresh = useEntryRefresh(user, refresh)
  const rankingUpdated = useCallback((diff: RankingDiff) => { setRankingDiff(diff); void reload() }, [reload, setRankingDiff])
  const hasData = state.status === 'ready' || state.status === 'stale' || state.status === 'unknown' || state.status === 'empty'
	const sourceData = hasData ? state.data : emptyData
  const sourceGameIds = useMemo(() => new Set([sourceData.overview.discoveries.featured, ...sourceData.overview.discoveries.items, ...sourceData.overview.wishlist.items, ...Object.values(sourceData.overview.calendar).flat()].filter((item) => item).map((item) => item!.gameId)), [sourceData.overview])
	const reducedMotion = usePrefersReducedMotion()
	const [presentation, setPresentation] = useState<DashboardPresentationState>(() => ({ source: sourceData.overview, tombstoneKey: '', reducedMotion, exits: new Map(), completed: new Set() }))
	const trackedGameIds = useMemo(() => new Set([...sourceGameIds, ...presentation.exits.keys()]), [presentation.exits, sourceGameIds])
  const tombstones = useMemo(() => new Set([...trackedGameIds].filter(workflows.isTombstoned)), [trackedGameIds, workflows])
	const tombstoneKey = [...tombstones].sort().join('|')
	if (presentation.source !== sourceData.overview || presentation.tombstoneKey !== tombstoneKey || presentation.reducedMotion !== reducedMotion) {
		const exits = new Map([...presentation.exits].filter(([gameId]) => tombstones.has(gameId)))
		const completed = new Set([...presentation.completed].filter((gameId) => tombstones.has(gameId)))
		if (reducedMotion) {
			exits.clear()
			for (const gameId of tombstones) completed.add(gameId)
		} else {
			for (const gameId of tombstones) {
				if (exits.has(gameId) || completed.has(gameId)) continue
				const origin = workflows.stateFor(gameId).acceptedOrigin
				if (origin?.surface !== 'dashboard') { completed.add(gameId); continue }
				const snapshot = findDashboardSnapshot(presentation.source, gameId, origin) ?? findDashboardSnapshot(sourceData.overview, gameId, origin)
				if (snapshot) exits.set(gameId, snapshot); else completed.add(gameId)
			}
		}
		setPresentation({ source: sourceData.overview, tombstoneKey, reducedMotion, exits, completed })
	}
	const finishVerdictExit = useCallback((gameId: string) => {
		const timer = pendingVerdictTimers.current.get(gameId)
		if (timer !== undefined) window.clearTimeout(timer)
		pendingVerdictTimers.current.delete(gameId)
		setPresentation((current) => {
			if (!current.exits.has(gameId)) return current
			const exits = new Map(current.exits); exits.delete(gameId)
			const completed = new Set(current.completed); completed.add(gameId)
			return { ...current, exits, completed }
		})
	}, [])
	useEffect(() => {
		for (const [gameId, timer] of pendingVerdictTimers.current) {
			if (presentation.exits.has(gameId)) continue
			window.clearTimeout(timer); pendingVerdictTimers.current.delete(gameId)
		}
		for (const gameId of presentation.exits.keys()) {
			if (pendingVerdictTimers.current.has(gameId)) continue
			pendingVerdictTimers.current.set(gameId, window.setTimeout(() => finishVerdictExit(gameId), verdictExitFallbackMs))
		}
	}, [finishVerdictExit, presentation.exits])
	useEffect(() => () => { pendingVerdictTimers.current.forEach((timer) => window.clearTimeout(timer)); pendingVerdictTimers.current.clear() }, [])
	const data = useMemo(() => ({ ...sourceData, overview: applyFeedbackTombstones(sourceData.overview, tombstones) }), [sourceData, tombstones])
  const { discoveries, wishlist, calendar, catalogStatus } = data.overview
	const exits = [...presentation.exits.values()]
	const featuredExit = exits.find((exit) => exit.lane === 'featured')
	const featured = featuredExit?.item ?? discoveries.featured
	const heldSuccessor = featuredExit?.successor && !tombstones.has(featuredExit.successor.item.gameId)
		? featuredExit.successor
		: featuredExit && discoveries.featured ? { item: discoveries.featured, index: 0 } : undefined
	const discoveryItems = insertExitSnapshots(discoveries.items, heldSuccessor ? [...exits, { ...heldSuccessor, lane: 'discoveries' }] : exits, 'discoveries')
	const wishlistItems = insertExitSnapshots(wishlist.items, exits, 'wishlist')
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
      <SyncProgress value={sync ?? entryRefresh.sync} requestError={requestError} />
      {state.status === 'loading' && <section className="state-card" role="status"><span className="spinner" aria-hidden="true" /><div><strong>Листаем вашу игровую историю</strong><p>Загружаем профиль и календарь…</p></div></section>}
      {state.status === 'error' && <section className="state-card state-card--error" role="alert"><div><strong>Дневник пока не открылся</strong><p>{state.message}</p></div><button type="button" onClick={() => { void reload() }}>Повторить</button></section>}
      {state.status === 'stale' && <section className="stale-banner" role="status"><strong>Показываем сохранённые рекомендации.</strong> {state.message}</section>}
      {state.status === 'unknown' && <section className="stale-banner stale-banner--unknown" role="status"><strong>Не удалось определить свежесть.</strong> {state.message}</section>}

      {hasData && <div className="dashboard-layout">
        <div className="dashboard-main">
          <section aria-labelledby="best-match-title">
            <div className="section-heading"><div><div className="eyebrow">Лучшее совпадение</div><h1 id="best-match-title">Новая находка для вас</h1></div></div>
            {featured ? <RecommendationCard key={featured.gameId} item={featured} featured feedbackEnabled presentationOrigin={{ surface: 'dashboard', lane: 'featured', recommendationId: featured.id }} readOnly={readOnly} onVerdictAnimationEnd={() => finishVerdictExit(featured.gameId)} /> : <div className="empty-card"><strong>Пока не нашли новых игр вне вашего списка желаемого</strong><p>{discoveryEmptyCopy[catalogStatus.status]}</p><p>Синхронизируйте профиль — мы сопоставим любимые механики, темы и активность с будущими релизами.</p><button type="button" onClick={requestSync} disabled={syncBusy}>Собрать профиль</button></div>}
          </section>
          <section className="recommendations-section" aria-labelledby="more-title"><div className="section-heading"><div><div className="eyebrow">Вне вашего желаемого</div><h2 id="more-title">Ещё новые игры</h2></div><span>Всего новых игр: {discoveries.total}</span></div>{discoveryItems.length > 0 && <div className="recommendation-grid">{discoveryItems.map((item) => <RecommendationCard item={item} key={item.id} feedbackEnabled presentationOrigin={{ surface: 'dashboard', lane: 'discoveries', recommendationId: item.id }} readOnly={readOnly} onVerdictAnimationEnd={() => finishVerdictExit(item.gameId)} />)}</div>}</section>
          <section className="recommendations-section wishlist-section" aria-labelledby="wishlist-title"><div className="section-heading"><div><div className="eyebrow">Следим за релизами</div><h2 id="wishlist-title">Уже в вашем желаемом</h2></div><span>Всего в желаемом: {wishlist.total}</span></div><WishlistFreshness wishlist={wishlist} />{wishlistItems.length > 0 ? <div className="recommendation-grid">{wishlistItems.map((item) => <RecommendationCard item={item} key={item.id} feedbackEnabled presentationOrigin={{ surface: 'dashboard', lane: 'wishlist', recommendationId: item.id }} readOnly={readOnly} onVerdictAnimationEnd={() => finishVerdictExit(item.gameId)} />)}</div> : wishlist.status === 'ready' ? <p className="wishlist-empty">В желаемом нет подходящих будущих релизов.</p> : null}</section>
        </div>
        <div className="right-rail">
          {!data.profile && (entryRefresh.status === 'checking' || entryRefresh.status === 'syncing' || (sync && !['succeeded', 'ready', 'dead', 'failed'].includes(sync.status)))
            ? <aside className="profile-card profile-card--building" aria-label="Профиль игры"><div className="eyebrow">Профиль игры</div><p role="status">Синхронизируем Steam и собираем профиль по вашей игровой активности.</p></aside>
            : <PreferenceProfile profile={data.profile} />}
          {data.profile && entryRefresh.status === 'error' && <p className="profile-refresh-error" role="status">Не удалось обновить профиль. Используем предыдущую версию.</p>}
          <ExplicitRecommendationSettings adjustments={data.adjustments} csrfToken={user.csrfToken} onRankingUpdated={rankingUpdated} readOnly={readOnly} />
          <CatalogCoverage status={catalogStatus} /><UpcomingWindows calendar={calendar} /><TestDigestButton csrfToken={user.csrfToken} readOnly={readOnly} /><DiagnosticsPage csrfToken={user.csrfToken} onDeleted={onDataDeleted} readOnly={readOnly} />
        </div>
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
