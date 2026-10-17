import type { PreferenceProfileData } from '../api/types'

export default function PreferenceProfile({ profile }: { profile: PreferenceProfileData | null }) {
  return (
    <aside className="profile-card" aria-labelledby="taste-title">
      <div className="eyebrow">Профиль вкуса</div>
      <h2 id="taste-title">Ваша игровая ДНК</h2>
      {profile ? (
        <>
          <p>{profile.summary || 'Профиль собран из вашей библиотеки и активности.'}</p>
          <div className="confidence-line"><span>Уверенность профиля</span><strong>{Math.round(profile.confidence * 100)}%</strong></div>
          <div className="meter" aria-label={`Уверенность ${Math.round(profile.confidence * 100)} процентов`}><span style={{ width: `${Math.round(profile.confidence * 100)}%` }} /></div>
          <details>
            <summary>На чём это основано</summary>
            <p>{profile.evidenceCount} сигналов из игр, активности, списка желаемого и достижений. Версия профиля {profile.version}.</p>
          </details>
        </>
      ) : <p className="muted">Синхронизируйте Steam, чтобы собрать персональный профиль.</p>}
    </aside>
  )
}
