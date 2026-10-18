import type { Recommendation } from '../api/types'
import { bucketRelease } from '../calendar/dateBuckets'
import { CompactGameIdentity } from './CompactGameIdentity'
import FeedbackLauncher from './FeedbackLauncher'

const WEEKDAYS = ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс']

function dateKey(year: number, month: number, day: number) {
  return `${year}-${String(month + 1).padStart(2, '0')}-${String(day).padStart(2, '0')}`
}

export default function ReleaseCalendar({ month, items, feedbackEnabled = false }: { month: Date; items: Recommendation[]; feedbackEnabled?: boolean }) {
  const year = month.getUTCFullYear()
  const monthIndex = month.getUTCMonth()
  const firstWeekday = (new Date(Date.UTC(year, monthIndex, 1)).getUTCDay() + 6) % 7
  const days = new Date(Date.UTC(year, monthIndex + 1, 0)).getUTCDate()
  const cells = Array.from({ length: Math.ceil((firstWeekday + days) / 7) * 7 }, (_, index) => index - firstWeekday + 1)
  const weeks = Array.from({ length: cells.length / 7 }, (_, index) => cells.slice(index * 7, index * 7 + 7))
  const byDate = new Map<string, Recommendation[]>()
  for (const item of items) {
    const bucket = bucketRelease(item.release)
    if (bucket.kind !== 'exact') continue
    const current = byDate.get(bucket.date) ?? []
    current.push(item); byDate.set(bucket.date, current)
  }

  return (
    <div className="calendar-scroll">
      <table className="calendar-grid" data-testid="calendar-grid" aria-label="Календарь релизов">
        <thead><tr>{WEEKDAYS.map((day) => <th className="weekday" scope="col" key={day}>{day}</th>)}</tr></thead>
        <tbody>{weeks.map((week, weekIndex) => <tr key={`week-${weekIndex}`}>{week.map((day, dayIndex) => {
          if (day < 1 || day > days) return <td className="calendar-day calendar-day--empty" aria-hidden="true" key={`empty-${weekIndex}-${dayIndex}`} />
          const key = dateKey(year, monthIndex, day)
          const date = new Date(Date.UTC(year, monthIndex, day))
          const label = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(date).replace(' г.', '')
          return <td className="calendar-day" aria-label={label} key={key}>
            <span className="calendar-day__number">{day}</span>
            {(byDate.get(key) ?? []).map((item) => <div className="calendar-event" key={item.id}><CompactGameIdentity item={item} size="grid" /><span>{Math.round(item.score)}%</span>{feedbackEnabled && <FeedbackLauncher item={item} gameId={item.gameId} recommendationId={item.id} rankingVersion={item.rankingVersion} gameName={item.name} coverImageUrl={item.coverImageUrl} presentationOrigin={{ surface: 'calendar', recommendationId: item.id }} />}</div>)}
          </td>
        })}</tr>)}</tbody>
      </table>
    </div>
  )
}
