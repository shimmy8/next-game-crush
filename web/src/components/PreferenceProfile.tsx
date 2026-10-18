import type { PreferenceProfileData, RankingDiff } from '../api/types'
import PreferenceAdjustments from './PreferenceAdjustments'

export default function PreferenceProfile({ profile, csrfToken, onRankingUpdated, readOnly = false }: { profile: PreferenceProfileData | null; csrfToken?: string; onRankingUpdated?: (diff: RankingDiff) => void; readOnly?: boolean }) {
  return (
    <aside className="profile-card" aria-label="Профиль вкуса">
      <div className="eyebrow">Профиль вкуса</div>
      {profile ? (
        <>
          <p>{profile.summary || 'Профиль собран из вашей библиотеки и активности.'}</p>
          <div className="confidence-line"><span>Уверенность профиля</span><strong>{Math.round(profile.confidence * 100)}%</strong></div>
          <div className="meter" aria-label={`Уверенность ${Math.round(profile.confidence * 100)} процентов`}><span style={{ width: `${Math.round(profile.confidence * 100)}%` }} /></div>
          <details>
            <summary>На чём это основано</summary>
            <p>{profile.evidenceCount} сигналов из игр, активности, списка желаемого и достижений. Версия профиля {profile.version}.</p>
          </details>
          {csrfToken && <PreferenceAdjustments adjustments={profile.adjustments ?? []} csrfToken={csrfToken} onRankingUpdated={onRankingUpdated} readOnly={readOnly} />}
        </>
      ) : <p className="muted">Синхронизируйте Steam, чтобы собрать персональный профиль.</p>}
    </aside>
  )
}
