import type { CalendarResponse, Recommendation } from '../api/types'
import { releaseLabel } from './RecommendationCard'
import { CompactGameIdentity } from './CompactGameIdentity'
import FeedbackLauncher from './FeedbackLauncher'
import { reviewLabel } from '../ui/reviews'

const bucketInfo: Array<[keyof Omit<CalendarResponse, 'exactDates'>, string]> = [
  ['month', 'Месяц'], ['quarter', 'Квартал'], ['year', 'Год'], ['comingSoon', 'Скоро'], ['tba', 'Дата уточняется'],
]

export default function ReleaseBuckets({ calendar, feedbackEnabled = false }: { calendar: CalendarResponse; feedbackEnabled?: boolean }) {
  return <div className="release-buckets">
    {bucketInfo.map(([key, title]) => calendar[key].length > 0 && (
      <section className="release-bucket" data-testid={`release-bucket-${key}`} key={key}>
        <div className="release-bucket__title"><h3>{title}</h3><span>{calendar[key].length}</span></div>
        <div className="bucket-items">{calendar[key].map((item: Recommendation) => {
          const reviews = reviewLabel(item.reviews)
          return <article key={item.id}><div><CompactGameIdentity item={item} size="list" /><span>{releaseLabel(item.release)}</span>
            {reviews && <p className="review-summary" role="note" aria-label={`Отзывы игроков в Steam: ${reviews}`}>{reviews}</p>}
            {feedbackEnabled && <FeedbackLauncher item={item} gameId={item.gameId} recommendationId={item.id} rankingVersion={item.rankingVersion} gameName={item.name} coverImageUrl={item.coverImageUrl} presentationOrigin={{ surface: 'calendar', recommendationId: item.id }} />}</div><b>{Math.round(item.score)}</b></article>
        })}</div>
      </section>
    ))}
  </div>
}
