import type { SyncStatus } from '../api/types'
import { diagnosticErrorLabel } from '../ui/labels'
import { deriveSyncPresentation, type StageCounts } from './syncProgressModel'

function countsLabel(counts: StageCounts) {
  const parts = [
    `Всего ${counts.total}`,
    `Завершено ${counts.completed}`,
    `Ожидают ${counts.pending}`,
    `С ошибкой ${counts.failed}`,
  ]
  if (counts.unknown > 0) parts.push(`Статус уточняется: ${counts.unknown}`)
  return parts.join(' · ')
}

export default function SyncProgress({ value, requestError }: { value: SyncStatus | null; requestError?: string }) {
  if (!value && !requestError) return null

  const presentation = deriveSyncPresentation(value, requestError)
  const progress = value ? Math.max(0, Math.min(100, value.progress)) : null
  const indeterminate = presentation.state === 'active' && progress === 0

  return (
    <section className={`sync-progress sync-progress--${presentation.state}`} aria-labelledby="sync-progress-title">
      <div className="sync-progress__head">
        <div className="sync-progress__summary" role="status" aria-live="polite" aria-atomic="true">
          <strong id="sync-progress-title">{presentation.headline}</strong>
        </div>
        {progress !== null && !indeterminate && <span aria-hidden="true">{progress}%</span>}
      </div>
      {presentation.warning && <p className="sync-progress__warning">{presentation.warning}</p>}
      {presentation.state === 'failed' && value?.error && (
        <p className="sync-progress__error">{diagnosticErrorLabel(value.error)}</p>
      )}
      {progress !== null && <progress aria-label="Прогресс синхронизации" max="100" value={indeterminate ? undefined : progress} />}
      {presentation.rows.length > 0 && (
        <div className="sync-progress__details" role="region" aria-label="Этапы синхронизации" tabIndex={0}>
          <ul>
            {presentation.rows.map((row) => (
              <li key={row.key}>
                <span>{row.label}</span>
                <span className="sync-progress__stage-status">
                  {row.counts && <span>{countsLabel(row.counts)}</span>}
                  {row.statusLabel && <span>{row.statusLabel}</span>}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}
