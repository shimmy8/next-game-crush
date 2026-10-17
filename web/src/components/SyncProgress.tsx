import type { SyncStatus } from '../api/types'

const STATUS_COPY: Record<string, string> = {
  queued: 'В очереди', running: 'Обновляем', waiting: 'Ждём этапы', retryable: 'Повторяем', succeeded: 'Обновлено', dead: 'Ошибка',
}

export default function SyncProgress({ value, error }: { value: SyncStatus | null; error?: string }) {
  if (!value && !error) return null
  return (
    <section className="sync-progress" aria-live="polite">
      <div className="sync-progress__head">
        <strong>{error || STATUS_COPY[value?.status ?? ''] || 'Синхронизация'}</strong>
        {value && <span>{Math.max(0, Math.min(100, value.progress))}%</span>}
      </div>
      {value && <progress max="100" value={value.progress}>{value.progress}%</progress>}
      {value?.stages?.length ? <ul>{value.stages.map((stage) => <li key={`${stage.kind}-${stage.stage}`}><span>{stage.stage}</span><span>{stage.error ? 'Ошибка' : stage.availability === 'unavailable' ? 'Недоступно' : STATUS_COPY[stage.status] || stage.status}</span></li>)}</ul> : null}
    </section>
  )
}
