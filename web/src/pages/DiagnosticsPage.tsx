import { useEffect, useState } from 'react'
import { deleteImportedData, getDiagnostics, getPreferenceAdjustments } from '../api/client'
import type { DiagnosticsData, PreferenceAdjustment } from '../api/types'
import { diagnosticErrorLabel, EXPLANATION_FAILURE_REASONS, explanationFailureLabel, jobLabel, statusLabel } from '../ui/labels'

type Props = {
  csrfToken: string
  onDeleted: () => void
  load?: (signal?: AbortSignal) => Promise<DiagnosticsData>
  remove?: (csrfToken: string) => Promise<void>
  loadAdjustments?: typeof getPreferenceAdjustments
  readOnly?: boolean
}

const componentNames: Record<string, string> = { database: 'База данных', steam: 'Steam', openai: 'OpenAI', smtp: 'Почта', jobs: 'Задания' }

function catalogStatusText(value: NonNullable<DiagnosticsData['catalogDiscovery']>): string {
  if (value.status === 'updating') return 'Каталог обновляется'
  if (value.status === 'stale' || value.status === 'degraded' || value.status === 'disabled' || value.backfillCompletedAt === null) return 'Каталог может быть неполным'
  return 'Каталог доступен'
}

export default function DiagnosticsPage({ csrfToken, onDeleted, load = getDiagnostics, remove = deleteImportedData, loadAdjustments = getPreferenceAdjustments, readOnly = false }: Props) {
  const [value, setValue] = useState<DiagnosticsData | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [adjustments, setAdjustments] = useState<PreferenceAdjustment[]>([])
  const [adjustmentError, setAdjustmentError] = useState('')
  const [historyVersion, setHistoryVersion] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    void loadAdjustments(true, controller.signal).then((items) => {
      if (!Array.isArray(items)) throw new Error('Invalid adjustment history')
      if (!controller.signal.aborted) { setAdjustments(items); setAdjustmentError('') }
    }).catch(() => { if (!controller.signal.aborted) setAdjustmentError('История корректировок временно недоступна') })
    return () => controller.abort()
  }, [loadAdjustments, historyVersion])
  useEffect(() => { const controller = new AbortController(); void load(controller.signal).then(setValue).catch(() => { if (!controller.signal.aborted) setError('Диагностика временно недоступна') }); return () => controller.abort() }, [load])
  async function deleteData() { if (readOnly) { setError('Демо-режим только для просмотра — удаление доступно владельцу.'); return } setBusy(true); setError(''); try { await remove(csrfToken); onDeleted() } catch { setError('Не удалось удалить данные') } finally { setBusy(false) } }
  return <section className="diagnostics-card" aria-labelledby="diagnostics-title">
    <div className="eyebrow">Состояние системы</div><h2 id="diagnostics-title">Диагностика и данные</h2>
    {error && <p role="alert">{error}</p>}
    {value?.catalogDiscovery && <section className="catalog-diagnostics" aria-labelledby="catalog-diagnostics-title">
      <h3 id="catalog-diagnostics-title">Покрытие каталога</h3>
      <p>{catalogStatusText(value.catalogDiscovery)}</p>
      <dl><div><dt>Найдено активных игр</dt><dd>{value.catalogDiscovery.activeDiscoveredGames}</dd></div><div><dt>Ожидают проверки</dt><dd>{value.catalogDiscovery.pendingQualifications}</dd></div></dl>
    </section>}
    {value && <><ul className="diagnostics-list">{Object.entries(value.components).map(([name, item]) => <li key={name}><strong>{componentNames[name] ?? 'Компонент'}</strong><span>{statusLabel(item.status)}</span>{item.freshAt && <time dateTime={item.freshAt}>{new Date(item.freshAt).toLocaleString('ru-RU')}</time>}</li>)}</ul>
      <p>{value.jobs.filter((job) => job.status === 'dead').length ? 'Есть задания, требующие внимания.' : 'Ошибок фоновых заданий нет.'}</p>
      {value.jobs.some((job) => job.error) && <ul className="diagnostic-errors">{value.jobs.filter((job) => job.error).map((job) => <li key={job.id}><strong>{jobLabel(job.stage, job.kind)}</strong><span>{diagnosticErrorLabel(job.error ?? '')}</span></li>)}</ul>}</>}
    {value?.jobs.filter((job) => job.kind === 'recommendations.rebuild' && job.explanations).map((job) => {
      const explanations = job.explanations
      if (!explanations) return null
      const failures = EXPLANATION_FAILURE_REASONS.filter((reason) => (explanations.failureReasons[reason] ?? 0) > 0)
      return <section className="explanation-diagnostics" aria-labelledby={`explanation-diagnostics-${job.id}`} key={job.id}>
        <h3 id={`explanation-diagnostics-${job.id}`}>Объяснения рекомендаций</h3>
        <dl><div><dt>AI-сводки</dt><dd>{explanations.aiGenerated}</dd></div><div><dt>По правилам</dt><dd>{explanations.deterministic}</dd></div><div><dt>Нейтральные</dt><dd>{explanations.none}</dd></div></dl>
        {failures.length > 0 && <><h4>Причины fallback</h4><dl>{failures.map((reason) => <div key={reason}><dt>{explanationFailureLabel(reason)}</dt><dd>{explanations.failureReasons[reason]}</dd></div>)}</dl></>}
      </section>
    })}
    <details className="adjustment-history"><summary>История корректировок</summary>{adjustmentError && <p role="status">{adjustmentError}</p>}<ul>{adjustments.map((item) => <li key={item.id}><strong>{item.label}</strong><span>{item.active ? 'Учитывается' : 'Не учитывается'}</span><time dateTime={item.createdAt}>{new Date(item.createdAt).toLocaleString('ru-RU')}</time></li>)}</ul><button type="button" onClick={() => setHistoryVersion((value) => value + 1)}>Обновить историю корректировок</button></details>
    {!confirming ? <button className="danger-button" type="button" onClick={() => { if (readOnly) { void deleteData() } else setConfirming(true) }}>Удалить импортированные данные</button> : <div className="delete-confirm"><p>Будут удалены библиотека, профиль, рекомендации, отзывы, задания и сессии. Общий каталог останется.</p><button className="danger-button" type="button" disabled={busy} onClick={() => { void deleteData() }}>{busy ? 'Удаляем…' : 'Подтвердить удаление'}</button><button type="button" onClick={() => setConfirming(false)}>Отмена</button></div>}
  </section>
}
