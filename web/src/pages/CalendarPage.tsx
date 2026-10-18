import { useMemo, useState } from 'react'
import type { CalendarResponse, DeckCompatibility, PlatformSupport, RankingDiff, Recommendation } from '../api/types'
import ReleaseBuckets from '../components/ReleaseBuckets'
import ReleaseCalendar from '../components/ReleaseCalendar'
import RecommendationCard from '../components/RecommendationCard'
import { bucketRelease } from '../calendar/dateBuckets'
import CalendarFeedbackProvider from '../components/CalendarFeedbackProvider'

type CalendarPageProps = { calendar: CalendarResponse; month?: Date; rankingDiff?: RankingDiff | null; feedbackEnabled?: boolean; readOnly?: boolean }
export type CalendarSourceFilter = 'all' | 'discoveries' | 'wishlist'
export type CalendarPlatformFilter = 'all' | keyof PlatformSupport | 'unknown'
export type CalendarDeckFilter = 'all' | DeckCompatibility

function filterCalendar(calendar: CalendarResponse, source: CalendarSourceFilter, minimum: number, precision: string, platform: CalendarPlatformFilter, deck: CalendarDeckFilter): CalendarResponse {
  const matches = (item: Recommendation) => {
    const inWishlist = item.wishlisted === true || (item.factors.wishlist ?? 0) > 0
    const matchesSource = source === 'all' || (source === 'wishlist' ? inWishlist : !inWishlist)
    const matchesPlatform = platform === 'all' || (platform === 'unknown' ? item.platforms === undefined : item.platforms?.[platform] === true)
    const matchesDeck = deck === 'all' || item.deckCompatibility === deck
    return matchesSource && item.score >= minimum && (precision === 'all' || item.release.precision === precision) && matchesPlatform && matchesDeck
  }
  return {
    exactDates: calendar.exactDates.filter(matches), month: calendar.month.filter(matches), quarter: calendar.quarter.filter(matches),
    year: calendar.year.filter(matches), comingSoon: calendar.comingSoon.filter(matches), tba: calendar.tba.filter(matches),
  }
}

export default function CalendarPage({ calendar, month = new Date(), rankingDiff, feedbackEnabled = false, readOnly = false }: CalendarPageProps) {
  const [view, setView] = useState<'month' | 'list'>('month')
  const [visibleMonth, setVisibleMonth] = useState(() => new Date(Date.UTC(month.getUTCFullYear(), month.getUTCMonth(), 1)))
  const [source, setSource] = useState<CalendarSourceFilter>('all')
  const [minimum, setMinimum] = useState(0)
  const [precision, setPrecision] = useState('all')
  const [platform, setPlatform] = useState<CalendarPlatformFilter>('all')
  const [deck, setDeck] = useState<CalendarDeckFilter>('all')
  const filtered = useMemo(() => filterCalendar(calendar, source, minimum, precision, platform, deck), [calendar, source, minimum, precision, platform, deck])
  const visibleGameIds = useMemo(() => new Set(Object.values(calendar).flat().map((item) => item.gameId)), [calendar])
  const exactForMonth = filtered.exactDates.filter((item) => {
    const bucket = bucketRelease(item.release)
    if (bucket.kind !== 'exact') return false
    const [year, monthIndex] = bucket.date.split('-').map(Number)
    return year === visibleMonth.getUTCFullYear() && monthIndex === visibleMonth.getUTCMonth() + 1
  }).sort((left, right) => (left.release.exactDate ?? '').localeCompare(right.release.exactDate ?? '') || left.name.localeCompare(right.name, 'ru'))
  const title = new Intl.DateTimeFormat('ru-RU', { month: 'long', year: 'numeric', timeZone: 'UTC' }).format(visibleMonth)
  const moveMonth = (delta: number) => setVisibleMonth((current) => new Date(Date.UTC(current.getUTCFullYear(), current.getUTCMonth() + delta, 1)))

  return (
    <CalendarFeedbackProvider visibleGameIds={visibleGameIds} readOnly={readOnly}><section className="calendar-section" aria-labelledby="calendar-title">
      {rankingDiff?.rebuildStatus === 'succeeded' && <div className="ranking-diff" role="status"><strong>Рекомендации обновлены · версия {rankingDiff.currentVersion}</strong><span>Добавлено: {rankingDiff.added.length} · Убрано: {rankingDiff.removed.length} · Переставлено: {rankingDiff.reordered.length}</span>{rankingDiff.adjustment?.active && <span>{rankingDiff.adjustment.label} — {rankingDiff.adjustment.effect}</span>}</div>}
      <header className="section-heading calendar-heading">
        <div><div className="eyebrow">Календарь релизов</div><div className="calendar-title-row"><h2 id="calendar-title">{title}</h2><div className="month-nav"><button type="button" aria-label="Предыдущий месяц" onClick={() => moveMonth(-1)}>←</button><button type="button" aria-label="Следующий месяц" onClick={() => moveMonth(1)}>→</button></div></div></div>
        <div className="view-switch" role="group" aria-label="Вид календаря">
          <button type="button" aria-pressed={view === 'month'} onClick={() => setView('month')}>Месяц</button>
          <button type="button" aria-pressed={view === 'list'} onClick={() => setView('list')}>Список</button>
        </div>
      </header>
      <div className="filters" aria-label="Фильтры календаря">
        <label>Источник игр<select value={source} onChange={(event) => setSource(event.target.value as CalendarSourceFilter)}><option value="all">Все</option><option value="discoveries">Новые для меня</option><option value="wishlist">В желаемом</option></select></label>
        <label>Точность даты<select value={precision} onChange={(event) => setPrecision(event.target.value)}><option value="all">Любая</option><option value="day">Точный день</option><option value="month">Месяц</option><option value="quarter">Квартал</option><option value="year">Год</option><option value="coming_soon">Скоро</option><option value="unknown">Дата уточняется</option></select></label>
        <label>Платформа<select value={platform} onChange={(event) => setPlatform(event.target.value as CalendarPlatformFilter)}><option value="all">Все</option><option value="windows">Windows</option><option value="mac">macOS</option><option value="linux">Linux</option><option value="unknown">Неизвестно</option></select></label>
        <label>Steam Deck<select value={deck} onChange={(event) => setDeck(event.target.value as CalendarDeckFilter)}><option value="all">Все</option><option value="verified">Проверено</option><option value="playable">Можно играть</option><option value="unsupported">Не поддерживается</option><option value="unknown">Неизвестно</option></select></label>
        <label>Совпадение от {minimum}<input aria-label="Минимальное совпадение" type="range" min="0" max="90" step="10" value={minimum} onChange={(event) => setMinimum(Number(event.target.value))} /></label>
      </div>
      {view === 'month' ? (
        <><ReleaseCalendar month={visibleMonth} items={filtered.exactDates} feedbackEnabled={feedbackEnabled} /><ReleaseBuckets calendar={filtered} feedbackEnabled={feedbackEnabled} /></>
      ) : (
        <><div className="release-list" data-testid="release-list" aria-label={`Релизы с точной датой: ${title}`}>
          {exactForMonth.length ? exactForMonth.map((item) => <RecommendationCard item={item} compact key={item.id} feedbackEnabled={feedbackEnabled} presentationOrigin={{ surface: 'calendar', recommendationId: item.id }} readOnly={readOnly} />) : <p className="empty-note">В этом месяце точных дат по фильтрам нет.</p>}
        </div><ReleaseBuckets calendar={filtered} feedbackEnabled={feedbackEnabled} /></>
      )}
    </section></CalendarFeedbackProvider>
  )
}
