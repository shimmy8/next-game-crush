import type { PreferenceProfileData } from '../api/types'

export default function PreferenceProfile({ profile }: { profile: PreferenceProfileData | null }) {
  return (
    <aside className="profile-card" aria-label="Профиль игры">
      <div className="eyebrow">Профиль игры</div>
      {profile ? (
        <>
          {profile.updating && <p className="profile-update" role="status">Профиль обновляется</p>}
          <p>{profile.summary}</p>
          <div className="confidence-line"><span>Уверенность профиля</span><strong>{Math.round(profile.confidence * 100)}%</strong></div>
          <div className="meter" aria-label={`Уверенность ${Math.round(profile.confidence * 100)} процентов`}><span style={{ width: `${Math.round(profile.confidence * 100)}%` }} /></div>
          <details>
            <summary>На чём это основано</summary>
            <p>{profile.evidenceCount} сигналов из сыгранных игр, недавней активности, достижений и завершений. Версия профиля {profile.version}.</p>
          </details>
        </>
      ) : <p className="muted">Синхронизируйте Steam, чтобы собрать персональный профиль.</p>}
    </aside>
  )
}
