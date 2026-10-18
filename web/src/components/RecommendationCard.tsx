import { useMemo, useState } from 'react'
import type { CSSProperties } from 'react'
import type { DeckCompatibility, PlatformSupport, Recommendation } from '../api/types'
import { bucketRelease } from '../calendar/dateBuckets'
import FeedbackForm from './FeedbackForm'
import FeedbackLauncher from './FeedbackLauncher'
import { useGameFeedback } from './FeedbackWorkflowContext'
import { CompactGameIdentity } from './CompactGameIdentity'
import { safeRecommendationEvidenceText, safeRecommendationExplanationText } from '../api/client'
import { isEvidenceKind } from '../api/types'
import type { FeedbackPresentationOrigin } from './FeedbackWorkflowContext'
import { reviewLabel } from '../ui/reviews'

const precisionLabels: Record<string, string> = { month: 'В течение месяца', quarter: 'В течение квартала', year: 'В течение года', coming_soon: 'Скоро', unknown: 'Дата уточняется' }
const knownRawLabels: Record<string, string> = { 'coming soon': 'Скоро', tba: 'Дата уточняется' }
const platformLabels: { key: keyof PlatformSupport; label: string }[] = [{ key: 'windows', label: 'Windows' }, { key: 'mac', label: 'macOS' }, { key: 'linux', label: 'Linux' }]
const deckLabels: Record<DeckCompatibility, string> = { verified: 'Steam Deck: проверено', playable: 'Steam Deck: можно играть', unsupported: 'Steam Deck: не поддерживается', unknown: 'Steam Deck: неизвестно' }
const neutralExplanation = 'Рекомендация основана на вашем игровом профиле, но конкретных совпадений пока недостаточно.'
const sandParticles = Array.from({ length: 24 }, (_, index) => {
  const y = 5 + ((index * 29) % 76)
  return {
    x: 3 + ((index * 37) % 94), y,
    size: 2 + ((index * 7) % 5),
    drift: -13 + ((index * 17) % 27),
    fall: 28 + ((index * 11) % 37),
    delay: Math.round(y * 3.4),
    duration: 190 + ((index * 19) % 70),
  }
})

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

export default function RecommendationCard({ item, featured = false, compact = false, feedbackEnabled = false, presentationOrigin, readOnly = false, onVerdictAnimationEnd }: { item: Recommendation; featured?: boolean; compact?: boolean; feedbackEnabled?: boolean; presentationOrigin?: FeedbackPresentationOrigin; readOnly?: boolean; onVerdictAnimationEnd?(): void }) {
  const [failedCoverUrl, setFailedCoverUrl] = useState<string>()
  const workflow = useGameFeedback(item.gameId)
  const likeAnimationDelayMs = useMemo(
    () => workflow.likeEffectAnimationDelayMs,
    // The delay is an initial-phase snapshot. Only a new acceptance event may replace it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [workflow.likeEffectStartedAt],
  )
  const wishlist = item.wishlisted === true || (item.factors.wishlist ?? 0) > 0
  const showCover = Boolean(item.coverImageUrl && item.coverImageUrl !== failedCoverUrl)
  const source = item.explanationSource === 'ai' || item.explanationSource === 'deterministic' ? item.explanationSource : 'none'
  const explanation = safeRecommendationExplanationText(item.explanation)
  const seenEvidenceText = new Set<string>()
  const evidence = Array.isArray(item.evidence) ? item.evidence.flatMap((entry) => {
    if (!entry || typeof entry.id !== 'string' || !isEvidenceKind(entry.kind)) return []
    const text = safeRecommendationEvidenceText(entry.text)
    if (!text || seenEvidenceText.has(text)) return []
    seenEvidenceText.add(text)
    return [{ ...entry, text }]
  }) : []
  const substantiveEvidence = evidence.filter((entry) => entry.kind !== 'wishlist_context').slice(0, 2)
  const wishlistEvidence = evidence.find((entry) => entry.kind === 'wishlist_context')
  const aiVisible = source === 'ai' && Boolean(explanation)
  const deterministicVisible = source === 'deterministic' && (substantiveEvidence.length > 0 || Boolean(wishlistEvidence))
  const aiEvidence = aiVisible ? substantiveEvidence.filter((entry) => entry.text !== explanation) : []
  const disliked = workflow.tombstoned && workflow.action === 'dislike'
  const showLikeEffect = workflow.likeEffectActive && !disliked
  const reviews = reviewLabel(item.reviews)
  const surface = (includeFeedback: boolean) => <>
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
      <div className="compatibility-row" role="group" aria-label="Совместимость игры">
        {item.platforms ? platformLabels.filter(({ key }) => item.platforms?.[key]).map(({ key, label }) => (
          <span className="compatibility-badge" key={key}>{label}</span>
        )) : <span className="compatibility-badge">Платформы неизвестны</span>}
        <span className={`compatibility-badge compatibility-badge--${item.deckCompatibility}`}>{deckLabels[item.deckCompatibility]}</span>
      </div>
      {reviews && <p className="review-summary" role="note" aria-label={`Отзывы игроков в Steam: ${reviews}`}>{reviews}</p>}
      <div className="score-row"><strong>{Math.round(item.score)}</strong><span>совпадение</span><span className="confidence-dot">{Math.round(item.confidence * 100)}% уверенность</span></div>
      <a className="store-link" href={item.storeUrl} target="_blank" rel="noopener noreferrer">Открыть в Steam <span aria-hidden="true">↗</span></a>
      {aiVisible && <p className="reason">{explanation}</p>}
      {source === 'deterministic' && substantiveEvidence.length > 0 && <ul className="reason-list" aria-label="Почему вам может понравиться">{substantiveEvidence.map((entry) => <li key={entry.id}>{entry.text}</li>)}</ul>}
      {source === 'deterministic' && substantiveEvidence.length === 0 && wishlistEvidence && <p className="reason">{wishlistEvidence.text}</p>}
      {!aiVisible && !deterministicVisible && <p className="reason">{neutralExplanation}</p>}
      {aiEvidence.length > 0 && <details className="evidence"><summary>На чём основано</summary><ul>{aiEvidence.map((entry) => <li key={entry.id}>{entry.text}</li>)}</ul></details>}
      {aiVisible && <p className="source-note">AI-сводка по вашим игровым данным</p>}
      {deterministicVisible && <p className="source-note">По вашим игровым данным</p>}
      {includeFeedback && feedbackEnabled && <FeedbackForm gameId={item.gameId} recommendationId={item.id} rankingVersion={item.rankingVersion} gameName={item.name} initialAction={item.latestFeedbackAction} initialReasons={item.latestFeedbackReasons} presentationOrigin={presentationOrigin} readOnly={readOnly} />}
    </div>
  </>
  if (compact) return <article className="recommendation-card recommendation-card--compact compact-release-card">
    <CompactGameIdentity item={item} size="list" />
    <div className="compact-release-card__details"><span className={`date-chip date-chip--${item.release.precision}`}>{releaseLabel(item.release)}</span>{wishlist && <span className="wishlist-badge">♥ В желаемом</span>}
      <div className="compatibility-row" role="group" aria-label="Совместимость игры">
        {item.platforms ? platformLabels.filter(({ key }) => item.platforms?.[key]).map(({ key, label }) => (
          <span className="compatibility-badge" key={key}>{label}</span>
        )) : <span className="compatibility-badge">Платформы неизвестны</span>}
        <span className={`compatibility-badge compatibility-badge--${item.deckCompatibility}`}>{deckLabels[item.deckCompatibility]}</span>
      </div>
      <span>{Math.round(item.score)} · совпадение</span>
      {reviews && <p className="review-summary" role="note" aria-label={`Отзывы игроков в Steam: ${reviews}`}>{reviews}</p>}
      {feedbackEnabled && <FeedbackLauncher item={item} gameId={item.gameId} recommendationId={item.id} rankingVersion={item.rankingVersion} gameName={item.name} coverImageUrl={item.coverImageUrl} presentationOrigin={presentationOrigin} readOnly={readOnly} />}
    </div>
  </article>
  return (
    <article
      className={`recommendation-card${featured ? ' recommendation-card--featured' : ''}${compact ? ' recommendation-card--compact' : ''}${showLikeEffect ? ' recommendation-card--liked' : ''}${disliked ? ' recommendation-card--disliked' : ''}`}
      style={showLikeEffect ? { animationDelay: `${likeAnimationDelayMs}ms` } : undefined}
      onAnimationEnd={(event) => { if (disliked && (event.target as HTMLElement).classList.contains('verdict-sand__surface')) onVerdictAnimationEnd?.() }}
    >
      <div className="recommendation-card__original" aria-hidden={disliked || undefined} inert={disliked || undefined}>{surface(true)}</div>
      {disliked && <div className="verdict-sand" aria-hidden="true">
        <div className={`verdict-sand__surface${featured ? ' verdict-sand__surface--featured' : ''}`} aria-hidden="true" inert>{surface(false)}</div>
        <div className="verdict-sand__particles" aria-hidden="true" inert>
          {sandParticles.map((particle, index) => <i className="verdict-sand__particle" key={index} style={{
            '--sand-x': `${particle.x}%`, '--sand-y': `${particle.y}%`, '--sand-size': `${particle.size}px`,
            '--sand-drift': `${particle.drift}px`, '--sand-fall': `${particle.fall}px`,
            '--sand-delay': `${particle.delay}ms`, '--sand-duration': `${particle.duration}ms`,
          } as CSSProperties} />)}
        </div>
      </div>}
    </article>
  )
}
