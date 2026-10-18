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
const searchFeedNames: Record<string, string> = { popular_upcoming: 'Популярные будущие', popular_new: 'Популярные новые', recent: 'Недавние релизы', coming_soon: 'Скоро выйдут' }
const throttleNames: Record<string, string> = { store: 'Steam Store', steam_api: 'Steam API', openai: 'OpenAI' }

function catalogStatusText(value: NonNullable<DiagnosticsData['catalogDiscovery']>): string {
  if (value.status === 'updating') return 'Каталог обновляется'
  if (value.status === 'stale' || value.status === 'degraded' || value.status === 'disabled' || value.backfillCompletedAt === null) return 'Каталог может быть неполным'
  return 'Каталог доступен'
}

function searchStatusText(status: NonNullable<DiagnosticsData['catalogSearch']>['status']): string {
  if (status === 'ready') return 'Поиск релизов обновлён'
  if (status === 'backfilling') return 'Идёт начальное наполнение'
  if (status === 'stale') return 'Данные поиска требуют обновления'
  if (status === 'disabled') return 'Поиск релизов отключён'
  return 'Поиск релизов временно недоступен'
}

function diagnosticTime(value: string | null): string {
  return value ? new Date(value).toLocaleString('ru-RU') : 'Нет данных'
}

function runKindText(kind: 'scheduled' | 'manual' | 'initial' | 'reconciliation'): string {
  if (kind === 'scheduled') return 'плановое обновление'
  if (kind === 'manual') return 'ручное обновление'
  if (kind === 'initial') return 'начальное наполнение'
  return 'сверка каталога'
}

function runText(run: NonNullable<NonNullable<DiagnosticsData['catalogSearch']>['lastCompleteRun']>): string {
  return `${runKindText(run.kind)} · запуск ${diagnosticTime(run.occurredAt)} · завершение ${diagnosticTime(run.completedAt)}`
}

function feedStatusText(status: string): string {
  if (status === 'complete') return 'завершена'
  if (status === 'partial') return 'завершена частично'
  if (status === 'failed') return 'ошибка'
  if (status === 'running') return 'обновляется'
  return 'ожидает запуска'
}

function safetyCapText(value?: string): string {
  if (value === 'page_limit') return 'достигнут лимит страниц'
  if (value === 'new_app_limit') return 'достигнут лимит новых игр'
  return ''
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
    {value?.catalogSearch && <section className="catalog-search-diagnostics" aria-labelledby="catalog-search-diagnostics-title">
      <h3 id="catalog-search-diagnostics-title">Поиск релизов Steam</h3>
      <p>{searchStatusText(value.catalogSearch.status)}</p>
      <dl>
        <div><dt>Последний запуск по расписанию</dt><dd>{diagnosticTime(value.catalogSearch.lastScheduledOccurrenceAt)}</dd></div>
        <div><dt>Последнее завершённое обновление</dt><dd>{value.catalogSearch.lastCompleteRun ? runText(value.catalogSearch.lastCompleteRun) : 'Нет данных'}</dd></div>
        <div><dt>Последнее частичное обновление</dt><dd>{value.catalogSearch.lastPartialRun ? runText(value.catalogSearch.lastPartialRun) : 'Нет данных'}</dd></div>
        <div><dt>Последнее неуспешное обновление</dt><dd>{value.catalogSearch.lastFailedRun ? runText(value.catalogSearch.lastFailedRun) : 'Нет данных'}</dd></div>
        <div><dt>Свежесть каталога</dt><dd>{diagnosticTime(value.catalogSearch.freshAt)}</dd></div>
        <div><dt>Последняя сверка со Steam</dt><dd>{diagnosticTime(value.catalogSearch.lastReconciliationAt)}</dd></div>
      </dl>
      <dl>
        <div><dt>Допущено будущих</dt><dd>{value.catalogSearch.admitted.upcoming}</dd></div>
        <div><dt>Допущено вышедших</dt><dd>{value.catalogSearch.admitted.released}</dd></div>
        <div><dt>Отклонено по отзывам</dt><dd>{value.catalogSearch.rejections.reviewRejected}</dd></div>
        <div><dt>Без отзывов в льготном окне</dt><dd>{value.catalogSearch.rejections.unratedGrace}</dd></div>
        <div><dt>Вне окна релизов</dt><dd>{value.catalogSearch.rejections.outsideWindow}</dd></div>
        <div><dt>Steam недоступен</dt><dd>{value.catalogSearch.rejections.unavailable}</dd></div>
        <div><dt>Ошибки обработки</dt><dd>{value.catalogSearch.rejections.failed}</dd></div>
        <div><dt>Метаданные в кэше</dt><dd>{value.catalogSearch.cache.metadataEntries}</dd></div>
        <div><dt>AI-признаки в кэше</dt><dd>{value.catalogSearch.cache.aiEntries}</dd></div>
        <div><dt>Ожидают срока проверки</dt><dd>{value.catalogSearch.due.count} · ближайшая {diagnosticTime(value.catalogSearch.due.nextAt)}</dd></div>
        <div><dt>Ревизия пересборки</dt><dd>{value.catalogSearch.rebuild.dirtyRevision}</dd></div>
        <div><dt>Ожидают пересборки</dt><dd>{value.catalogSearch.rebuild.pendingWakeups}</dd></div>
      </dl>
      <h4>Ленты</h4>
      <dl>{Object.entries(value.catalogSearch.feeds).map(([feed, item]) => <div key={feed}><dt>{searchFeedNames[feed] ?? 'Лента Steam'}</dt><dd>{feedStatusText(item.status)} · {item.pages} стр. · {item.discovered} найдено · {item.deduplicated} уникальных · {item.active} активных{item.boundaryStop ? ' · граница окна достигнута' : ''}{safetyCapText(item.safetyCap) ? ` · ${safetyCapText(item.safetyCap)}` : ''}</dd></div>)}</dl>
      <h4>Лимиты за час</h4>
      <dl><div><dt>Steam Store</dt><dd>{value.catalogSearch.budgets.store.used} из {value.catalogSearch.budgets.store.limit} · осталось {value.catalogSearch.budgets.store.remaining} · отложено {value.catalogSearch.budgets.store.deferred}</dd></div><div><dt>OpenAI</dt><dd>{value.catalogSearch.budgets.ai.used} из {value.catalogSearch.budgets.ai.limit} · осталось {value.catalogSearch.budgets.ai.remaining}</dd></div></dl>
      <h4>Ограничение запросов</h4>
      <dl>{Object.entries(value.catalogSearch.throttles).map(([name, item]) => <div key={name}><dt>{throttleNames[name] ?? 'Внешний сервис'}</dt><dd>{item.concurrency} одновременно · следующий запрос {diagnosticTime(item.nextAt)}</dd></div>)}</dl>
      <h4>Очереди</h4>
      <dl><div><dt>Интерактивная</dt><dd>{value.catalogSearch.lanes.interactive.queued} в очереди · {value.catalogSearch.lanes.interactive.running} выполняется · {value.catalogSearch.lanes.interactive.deferred} отложено · {value.catalogSearch.lanes.interactive.failed} ошибок</dd></div><div><dt>Каталог</dt><dd>{value.catalogSearch.lanes.catalog.queued} в очереди · {value.catalogSearch.lanes.catalog.running} выполняется · {value.catalogSearch.lanes.catalog.deferred} отложено · {value.catalogSearch.lanes.catalog.failed} ошибок</dd></div></dl>
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
