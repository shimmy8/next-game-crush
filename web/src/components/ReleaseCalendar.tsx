import type { Recommendation } from '../api/types'
import { bucketRelease } from '../calendar/dateBuckets'

const WEEKDAYS = ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс']

function dateKey(year: number, month: number, day: number) {
  return `${year}-${String(month + 1).padStart(2, '0')}-${String(day).padStart(2, '0')}`
}

export default function ReleaseCalendar({ month, items }: { month: Date; items: Recommendation[] }) {
  const year = month.getUTCFullYear()
  const monthIndex = month.getUTCMonth()
  const firstWeekday = (new Date(Date.UTC(year, monthIndex, 1)).getUTCDay() + 6) % 7
  const days = new Date(Date.UTC(year, monthIndex + 1, 0)).getUTCDate()
  const cells = Array.from({ length: Math.ceil((firstWeekday + days) / 7) * 7 }, (_, index) => index - firstWeekday + 1)
  const byDate = new Map<string, Recommendation[]>()
  for (const item of items) {
    const bucket = bucketRelease(item.release)
    if (bucket.kind !== 'exact') continue
    const current = byDate.get(bucket.date) ?? []
    current.push(item); byDate.set(bucket.date, current)
  }

  return (
    <div className="calendar-grid" data-testid="calendar-grid" role="grid" aria-label="Календарь релизов">
      {WEEKDAYS.map((day) => <div className="weekday" role="columnheader" key={day}>{day}</div>)}
      {cells.map((day, index) => {
        if (day < 1 || day > days) return <div className="calendar-day calendar-day--empty" role="gridcell" aria-hidden="true" key={`empty-${index}`} />
        const key = dateKey(year, monthIndex, day)
        const date = new Date(Date.UTC(year, monthIndex, day))
        const label = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(date).replace(' г.', '')
        return <div className="calendar-day" role="gridcell" aria-label={label} key={key}>
          <span className="calendar-day__number">{day}</span>
          {(byDate.get(key) ?? []).map((item) => <div className="calendar-event" key={item.id}><strong>{item.name}</strong><span>{Math.round(item.score)}%</span></div>)}
        </div>
      })}
    </div>
  )
}
