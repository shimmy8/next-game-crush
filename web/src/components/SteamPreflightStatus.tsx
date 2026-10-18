import type { PreflightResponse } from '../api/types'

type Props = {
  checking: boolean
  preflight: PreflightResponse | null
  error: string
  onRetry: () => void
}

function PrivacyGuidance() {
  return <>
    <ol>
      <li>Откройте свой профиль Steam.</li>
      <li>Нажмите «Редактировать профиль».</li>
      <li>Перейдите в «Настройки приватности».</li>
      <li>Для «Мой профиль» и «Доступ к игровой информации» выберите «Открытый».</li>
      <li>Сохраните изменения, вернитесь сюда и нажмите «Проверить снова».</li>
    </ol>
    <p>Отдельно скрытые игры останутся недоступны — проверьте настройки нужных игр. Список желаемого и достижения могут быть недоступны независимо от этих настроек, но они не блокируют синхронизацию.</p>
  </>
}

function PreflightActions({ onRetry }: Pick<Props, 'onRetry'>) {
  return <div className="preflight-status__actions">
    <a href="https://steamcommunity.com/my/edit/settings" target="_blank" rel="noreferrer">Открыть настройки Steam</a>
    <button type="button" onClick={onRetry}>Проверить снова</button>
  </div>
}

export default function SteamPreflightStatus({ checking, preflight, error, onRetry }: Props) {
  if (checking) {
    return <section className="preflight-status" role="status"><span className="spinner" aria-hidden="true" /><div><strong>Проверяем приватность Steam</strong><p>Убеждаемся, что профиль и игровая информация доступны.</p></div></section>
  }
  if (error) {
    return <section className="preflight-status preflight-status--error" role="alert">
      <div className="preflight-status__content">
        <strong>Проверка Steam не завершилась</strong>
        <p>{error}. Это не означает, что профиль закрыт. Проверьте настройки ниже и повторите запрос.</p>
        <PrivacyGuidance />
      </div>
      <PreflightActions onRetry={onRetry} />
    </section>
  }
  if (!preflight || preflight.canSync) return null

  const heading = preflight.reason === 'profile_private'
    ? 'Профиль Steam закрыт'
    : 'Доступ к игровой информации не подтверждён'

  return <section className="preflight-status preflight-status--blocked" role="alert">
    <div className="preflight-status__content">
      <strong>{heading}</strong>
      <PrivacyGuidance />
    </div>
    <PreflightActions onRetry={onRetry} />
  </section>
}
