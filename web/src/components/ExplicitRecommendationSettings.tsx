import type { PreferenceAdjustment, RankingDiff } from '../api/types'
import PreferenceAdjustments from './PreferenceAdjustments'

export function ExplicitRecommendationSettings({ adjustments, csrfToken, readOnly = false, onRankingUpdated }: { adjustments: PreferenceAdjustment[]; csrfToken: string; readOnly?: boolean; onRankingUpdated?: (diff: RankingDiff) => void }) {
  if (!adjustments.some((adjustment) => adjustment.active)) return null
  return <aside className="explicit-settings" aria-label="Явные настройки рекомендаций">
    <div className="eyebrow">Явные настройки рекомендаций</div>
    <p>Эти корректировки влияют на рекомендации, но не изменяют профиль игры.</p>
    <PreferenceAdjustments adjustments={adjustments} csrfToken={csrfToken} readOnly={readOnly} onRankingUpdated={onRankingUpdated} />
  </aside>
}
