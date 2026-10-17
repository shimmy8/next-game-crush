import type { SyncStatus } from '../api/types'

const STATUS_COPY: Record<string, string> = { queued: 'В очереди', running: 'В работе', waiting: 'Ожидает этапы', retryable: 'Ожидает повтора', succeeded: 'Готово', ready: 'Готово', failed: 'Сбой этапа', dead: 'Сбой этапа' }

export default function SyncProgress({ value, requestError }: { value: SyncStatus | null; requestError?: string }) {
  if (!value && !requestError) return null
  const unavailable = value?.stages?.some((stage) => stage.availability === 'unavailable') ?? false
  const degraded = ['succeeded', 'ready'].includes(value?.status ?? '') && (value?.stages?.some((stage) => Boolean(stage.error) || ['failed', 'dead'].includes(stage.status) || stage.availability === 'request_failed') ?? false)
  const incomplete = value ? ['queued', 'running', 'waiting', 'retryable'].includes(value.status) : false
  const state = requestError ? 'request-failed' : unavailable ? 'unavailable' : ['dead', 'failed'].includes(value?.status ?? '') ? 'failed' : degraded ? 'degraded' : incomplete ? 'incomplete' : 'ready'
  const headline = {
    unavailable: 'Источник недоступен',
    'request-failed': 'Не удалось проверить статус',
    failed: 'Синхронизация завершилась с ошибкой',
    degraded: 'Обновлено не полностью',
    incomplete: 'Синхронизация не завершена',
    ready: 'Синхронизация завершена',
  }[state]
  return (
    <section className={`sync-progress sync-progress--${state}`} role="status" aria-live="polite">
      <div className="sync-progress__head">
        <div><strong>{headline}</strong>{requestError && state === 'request-failed' && <small>{requestError}</small>}{state === 'failed' && value?.error && <small>{value.error}</small>}</div>
        {value && <span>{Math.max(0, Math.min(100, value.progress))}%</span>}
      </div>
      {value && <progress max="100" value={value.progress}>{value.progress}%</progress>}
      {value?.stages?.length ? <ul>{value.stages.map((stage) => <li key={`${stage.kind}-${stage.stage}`}><span>{stage.stage}</span><span>{stage.availability === 'unavailable' ? 'Недоступно' : stage.availability === 'not_modified' ? 'Актуально' : stage.availability === 'request_failed' || stage.error ? 'Сбой этапа' : STATUS_COPY[stage.status] || 'Не завершено'}</span></li>)}</ul> : null}
    </section>
  )
}
