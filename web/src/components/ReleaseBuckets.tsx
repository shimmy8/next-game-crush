import type { CalendarResponse, Recommendation } from '../api/types'
import { releaseLabel } from './RecommendationCard'

const bucketInfo: Array<[keyof Omit<CalendarResponse, 'exactDates'>, string]> = [
  ['month', 'Месяц'], ['quarter', 'Квартал'], ['year', 'Год'], ['comingSoon', 'Coming Soon'], ['tba', 'TBA'],
]

export default function ReleaseBuckets({ calendar }: { calendar: CalendarResponse }) {
  return <div className="release-buckets">
    {bucketInfo.map(([key, title]) => calendar[key].length > 0 && (
      <section className="release-bucket" data-testid={`release-bucket-${key}`} key={key}>
        <div className="release-bucket__title"><h3>{title}</h3><span>{calendar[key].length}</span></div>
        <div className="bucket-items">{calendar[key].map((item: Recommendation) => <article key={item.id}><div><strong>{item.name}</strong><span>{releaseLabel(item.release)}</span></div><b>{Math.round(item.score)}</b></article>)}</div>
      </section>
    ))}
  </div>
}
