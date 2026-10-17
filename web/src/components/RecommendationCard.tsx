import type { Recommendation } from '../api/types'
import { bucketRelease } from '../calendar/dateBuckets'

const precisionLabels: Record<string, string> = { month: 'В течение месяца', quarter: 'В течение квартала', year: 'В течение года', coming_soon: 'Скоро', unknown: 'Дата уточняется' }

export function releaseLabel(release: Recommendation['release']) {
  const bucket = bucketRelease(release)
  if (bucket.kind === 'exact') {
    const [year, month, day] = bucket.date.split('-').map(Number)
    return new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(new Date(Date.UTC(year, month - 1, day)))
  }
  return (release.precision !== 'day' ? release.raw : undefined) || precisionLabels[release.precision] || 'Дата уточняется'
}

export default function RecommendationCard({ item, featured = false, compact = false }: { item: Recommendation; featured?: boolean; compact?: boolean }) {
  const wishlist = (item.factors.wishlist ?? 0) > 0
  return (
    <article className={`recommendation-card${featured ? ' recommendation-card--featured' : ''}${compact ? ' recommendation-card--compact' : ''}`}>
      <div className="cover-panel" aria-label={`Обложка ${item.name} недоступна`}>
        <span className="cover-panel__mark" aria-hidden="true">{item.name.slice(0, 1).toUpperCase()}</span>
        <span>Обложка недоступна</span>
      </div>
      <div className="recommendation-card__body">
        <div className="card-meta">
          <span className={`date-chip date-chip--${item.release.precision}`}>{releaseLabel(item.release)}</span>
          {wishlist && <span className="wishlist-badge">♥ В желаемом</span>}
        </div>
        <h3>{item.name}</h3>
        <div className="score-row"><strong>{Math.round(item.score)}</strong><span>совпадение</span><span className="confidence-dot">{Math.round(item.confidence * 100)}% уверенность</span></div>
        {!compact && <p className="reason">{item.explanation || 'Рекомендация собрана из сохранённых факторов совпадения.'}</p>}
        {!compact && item.evidence.length > 0 && (
          <details className="evidence">
            <summary>Почему вам может понравиться</summary>
            <ul>{item.evidence.map((evidence) => <li key={evidence.id}>{evidence.text}</li>)}</ul>
            <p className="source-note">Источник объяснения: {item.generatedExplanation ? 'AI, только по сохранённым фактам' : 'детерминированные факторы'}</p>
          </details>
        )}
        {!compact && <div className="reaction-row" aria-label="Действия с рекомендацией">
          <button type="button" disabled title="Будет доступно после подключения обратной связи">Нравится</button>
          <button type="button" disabled title="Будет доступно после подключения обратной связи">Не моё</button>
          <button type="button" disabled title="Будет доступно после подключения напоминаний">Напомнить</button>
        </div>}
      </div>
    </article>
  )
}
