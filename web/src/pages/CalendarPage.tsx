import { useMemo, useState } from 'react'
import type { CalendarResponse, Recommendation } from '../api/types'
import ReleaseBuckets from '../components/ReleaseBuckets'
import ReleaseCalendar from '../components/ReleaseCalendar'
import RecommendationCard from '../components/RecommendationCard'

type CalendarPageProps = { calendar: CalendarResponse; month?: Date }

function filterCalendar(calendar: CalendarResponse, wishlistOnly: boolean, minimum: number, precision: string): CalendarResponse {
  const matches = (item: Recommendation) => (!wishlistOnly || (item.factors.wishlist ?? 0) > 0) && item.score >= minimum && (precision === 'all' || item.release.precision === precision)
  return {
    exactDates: calendar.exactDates.filter(matches), month: calendar.month.filter(matches), quarter: calendar.quarter.filter(matches),
    year: calendar.year.filter(matches), comingSoon: calendar.comingSoon.filter(matches), tba: calendar.tba.filter(matches),
  }
}

export default function CalendarPage({ calendar, month = new Date() }: CalendarPageProps) {
  const [view, setView] = useState<'month' | 'list'>('month')
  const [visibleMonth, setVisibleMonth] = useState(() => new Date(Date.UTC(month.getUTCFullYear(), month.getUTCMonth(), 1)))
  const [wishlistOnly, setWishlistOnly] = useState(false)
  const [minimum, setMinimum] = useState(0)
  const [precision, setPrecision] = useState('all')
  const filtered = useMemo(() => filterCalendar(calendar, wishlistOnly, minimum, precision), [calendar, wishlistOnly, minimum, precision])
  const all = [...filtered.exactDates, ...filtered.month, ...filtered.quarter, ...filtered.year, ...filtered.comingSoon, ...filtered.tba]
  const title = new Intl.DateTimeFormat('ru-RU', { month: 'long', year: 'numeric', timeZone: 'UTC' }).format(visibleMonth)
  const moveMonth = (delta: number) => setVisibleMonth((current) => new Date(Date.UTC(current.getUTCFullYear(), current.getUTCMonth() + delta, 1)))

  return (
    <section className="calendar-section" aria-labelledby="calendar-title">
      <header className="section-heading calendar-heading">
        <div><div className="eyebrow">Календарь релизов</div><div className="calendar-title-row"><h2 id="calendar-title">{title}</h2>{view === 'month' && <div className="month-nav"><button type="button" aria-label="Предыдущий месяц" onClick={() => moveMonth(-1)}>←</button><button type="button" aria-label="Следующий месяц" onClick={() => moveMonth(1)}>→</button></div>}</div></div>
        <div className="view-switch" aria-label="Вид календаря">
          <button type="button" aria-pressed={view === 'month'} onClick={() => setView('month')}>Месяц</button>
          <button type="button" aria-pressed={view === 'list'} onClick={() => setView('list')}>Список</button>
        </div>
      </header>
      <div className="filters" aria-label="Фильтры календаря">
        <label className="check-filter"><input type="checkbox" checked={wishlistOnly} onChange={(event) => setWishlistOnly(event.target.checked)} /> Только желаемое</label>
        <label>Точность даты<select value={precision} onChange={(event) => setPrecision(event.target.value)}><option value="all">Любая</option><option value="day">Точный день</option><option value="month">Месяц</option><option value="quarter">Квартал</option><option value="year">Год</option><option value="coming_soon">Coming Soon</option><option value="unknown">TBA</option></select></label>
        <label>Совпадение от {minimum}<input aria-label="Минимальное совпадение" type="range" min="0" max="90" step="10" value={minimum} onChange={(event) => setMinimum(Number(event.target.value))} /></label>
      </div>
      {view === 'month' ? (
        <><ReleaseCalendar month={visibleMonth} items={filtered.exactDates} /><ReleaseBuckets calendar={filtered} /></>
      ) : (
        <div className="release-list" data-testid="release-list">
          {all.length ? all.map((item) => <RecommendationCard item={item} compact key={item.id} />) : <p className="empty-note">По этим фильтрам ничего нет.</p>}
        </div>
      )}
    </section>
  )
}
