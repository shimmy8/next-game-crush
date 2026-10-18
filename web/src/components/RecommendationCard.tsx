import { useState } from 'react'
import type { Recommendation } from '../api/types'
import type { RankingDiff } from '../api/types'
import { bucketRelease } from '../calendar/dateBuckets'
import FeedbackForm from './FeedbackForm'

const precisionLabels: Record<string, string> = { month: 'В течение месяца', quarter: 'В течение квартала', year: 'В течение года', coming_soon: 'Скоро', unknown: 'Дата уточняется' }
const knownRawLabels: Record<string, string> = { 'coming soon': 'Скоро', tba: 'Дата уточняется' }
const legacyTechnicalText = /(?:deterministic\s+score|coming_soon\s*:|(?:positiveSimilarity|negativeSimilarity|wishlist|tagAffinity|playtimeAffinity|achievementAffinity|recentActivityAffinity|positiveFeedback|novelty|negativeFeedback|dateUncertainty|confidence)"?\s*:)/i

function isJSON(text: string) {
  if (!text.startsWith('{') && !text.startsWith('[')) return false
  try {
    const value: unknown = JSON.parse(text)
    return typeof value === 'object' && value !== null
  } catch {
    return false
  }
}

function safePresentationText(text: string) {
  const trimmed = text.trim()
  return trimmed && !isJSON(trimmed) && !legacyTechnicalText.test(trimmed) ? trimmed : ''
}

export function releaseLabel(release: Recommendation['release']) {
  const bucket = bucketRelease(release)
  if (bucket.kind === 'exact') {
    const [year, month, day] = bucket.date.split('-').map(Number)
    return new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(new Date(Date.UTC(year, month - 1, day)))
  }
  const raw = release.precision !== 'day' ? release.raw?.trim() : undefined
  const knownRawLabel = raw ? knownRawLabels[raw.toLocaleLowerCase('en-US')] : undefined
  return knownRawLabel || raw || precisionLabels[release.precision] || 'Дата уточняется'
}

export default function RecommendationCard({ item, featured = false, compact = false, csrfToken, onRankingUpdated }: { item: Recommendation; featured?: boolean; compact?: boolean; csrfToken?: string; onRankingUpdated?: (diff: RankingDiff) => void }) {
  const [failedCoverUrl, setFailedCoverUrl] = useState<string>()
  const wishlist = (item.factors.wishlist ?? 0) > 0
  const showCover = Boolean(item.coverImageUrl && item.coverImageUrl !== failedCoverUrl)
  const explanation = safePresentationText(item.explanation) || 'Рекомендация собрана по вашим игровым предпочтениям.'
  const evidence = item.evidence.filter((entry) => safePresentationText(entry.text))
  return (
    <article className={`recommendation-card${featured ? ' recommendation-card--featured' : ''}${compact ? ' recommendation-card--compact' : ''}`}>
      {showCover ? (
        <div className="cover-panel cover-panel--image">
          <img className="cover-panel__image" src={item.coverImageUrl} alt={`Обложка игры ${item.name}`} onError={() => setFailedCoverUrl(item.coverImageUrl)} />
        </div>
      ) : (
        <div className="cover-panel" role="img" aria-label={`Обложка ${item.name} недоступна`}>
          <span className="cover-panel__mark" aria-hidden="true">{item.name.slice(0, 1).toUpperCase()}</span>
          <span aria-hidden="true">Обложка недоступна</span>
        </div>
      )}
      <div className="recommendation-card__body">
        <div className="card-meta">
          <span className={`date-chip date-chip--${item.release.precision}`}>{releaseLabel(item.release)}</span>
          {wishlist && <span className="wishlist-badge">♥ В желаемом</span>}
        </div>
        <h3>{item.name}</h3>
        <div className="score-row"><strong>{Math.round(item.score)}</strong><span>совпадение</span><span className="confidence-dot">{Math.round(item.confidence * 100)}% уверенность</span></div>
        <a className="store-link" href={item.storeUrl} target="_blank" rel="noopener noreferrer">Открыть в Steam <span aria-hidden="true">↗</span></a>
        {!compact && <p className="reason">{explanation}</p>}
        {!compact && evidence.length > 0 && (
          <details className="evidence">
            <summary>Почему вам может понравиться</summary>
            <ul>{evidence.map((entry) => <li key={entry.id}>{entry.text}</li>)}</ul>
          </details>
        )}
        {!compact && <p className="source-note">Источник объяснения: {item.generatedExplanation ? 'AI, только по сохранённым фактам' : 'детерминированные факторы'}</p>}
        {!compact && csrfToken && <FeedbackForm recommendationId={item.id} rankingVersion={item.rankingVersion} csrfToken={csrfToken} onRankingUpdated={onRankingUpdated} />}
      </div>
    </article>
  )
}
