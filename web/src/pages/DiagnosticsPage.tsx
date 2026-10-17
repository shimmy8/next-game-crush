import { useEffect, useState } from 'react'
import { deleteImportedData, getDiagnostics } from '../api/client'
import type { DiagnosticsData } from '../api/types'

type Props = {
  csrfToken: string
  onDeleted: () => void
  load?: (signal?: AbortSignal) => Promise<DiagnosticsData>
  remove?: (csrfToken: string) => Promise<void>
}

const componentNames: Record<string, string> = { database: 'База данных', steam: 'Steam', openai: 'OpenAI', smtp: 'Почта', jobs: 'Задания' }

export default function DiagnosticsPage({ csrfToken, onDeleted, load = getDiagnostics, remove = deleteImportedData }: Props) {
  const [value, setValue] = useState<DiagnosticsData | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => { const controller = new AbortController(); void load(controller.signal).then(setValue).catch(() => { if (!controller.signal.aborted) setError('Диагностика временно недоступна') }); return () => controller.abort() }, [load])
  async function deleteData() { setBusy(true); setError(''); try { await remove(csrfToken); onDeleted() } catch { setError('Не удалось удалить данные') } finally { setBusy(false) } }
  return <section className="diagnostics-card" aria-labelledby="diagnostics-title">
    <div className="eyebrow">Состояние системы</div><h2 id="diagnostics-title">Диагностика и данные</h2>
    {error && <p role="alert">{error}</p>}
    {value && <><ul className="diagnostics-list">{Object.entries(value.components).map(([name, item]) => <li key={name}><strong>{componentNames[name] ?? name}</strong><span>{item.status}</span>{item.freshAt && <time dateTime={item.freshAt}>{new Date(item.freshAt).toLocaleString('ru')}</time>}</li>)}</ul>
      <p>{value.jobs.filter((job) => job.status === 'dead').length ? 'Есть задания, требующие внимания.' : 'Ошибок фоновых заданий нет.'}</p>
      {value.jobs.some((job) => job.error) && <ul className="diagnostic-errors">{value.jobs.filter((job) => job.error).map((job) => <li key={job.id}><strong>{job.stage || job.kind}</strong><span>{job.error}</span></li>)}</ul>}</>}
    {!confirming ? <button className="danger-button" type="button" onClick={() => setConfirming(true)}>Удалить импортированные данные</button> : <div className="delete-confirm"><p>Будут удалены библиотека, профиль, рекомендации, отзывы, задания и сессии. Общий каталог останется.</p><button className="danger-button" type="button" disabled={busy} onClick={() => { void deleteData() }}>{busy ? 'Удаляем…' : 'Подтвердить удаление'}</button><button type="button" onClick={() => setConfirming(false)}>Отмена</button></div>}
  </section>
}
