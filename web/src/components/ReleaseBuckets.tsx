import type { CalendarResponse, Recommendation } from '../api/types'
import { releaseLabel } from './RecommendationCard'
import { CompactGameIdentity } from './CompactGameIdentity'
import FeedbackLauncher from './FeedbackLauncher'

const bucketInfo: Array<[keyof Omit<CalendarResponse, 'exactDates'>, string]> = [
  ['month', 'Месяц'], ['quarter', 'Квартал'], ['year', 'Год'], ['comingSoon', 'Скоро'], ['tba', 'Дата уточняется'],
]

export default function ReleaseBuckets({ calendar, feedbackEnabled = false }: { calendar: CalendarResponse; feedbackEnabled?: boolean }) {
  return <div className="release-buckets">
    {bucketInfo.map(([key, title]) => calendar[key].length > 0 && (
      <section className="release-bucket" data-testid={`release-bucket-${key}`} key={key}>
        <div className="release-bucket__title"><h3>{title}</h3><span>{calendar[key].length}</span></div>
        <div className="bucket-items">{calendar[key].map((item: Recommendation) => <article key={item.id}><div><CompactGameIdentity item={item} size="list" /><span>{releaseLabel(item.release)}</span>{feedbackEnabled && <FeedbackLauncher item={item} gameId={item.gameId} recommendationId={item.id} rankingVersion={item.rankingVersion} gameName={item.name} coverImageUrl={item.coverImageUrl} />}</div><b>{Math.round(item.score)}</b></article>)}</div>
      </section>
    ))}
  </div>
}
