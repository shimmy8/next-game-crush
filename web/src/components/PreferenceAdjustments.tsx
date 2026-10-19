import { useEffect, useRef, useState } from 'react'
import { disablePreferenceAdjustment, getRankingDiff } from '../api/client'
import type { DisableAdjustmentAccepted, PreferenceAdjustment, RankingDiff } from '../api/types'
import { pollRankingWorkflow, waitForRanking } from './rankingWorkflow'

type Props = {
  adjustments: PreferenceAdjustment[]; csrfToken: string; onRankingUpdated?: (diff: RankingDiff) => void
  disable?: typeof disablePreferenceAdjustment; readDiff?: typeof getRankingDiff
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>
  readOnly?: boolean
}

function AdjustmentRow({ adjustment, csrfToken, onRankingUpdated, disable = disablePreferenceAdjustment, readDiff = getRankingDiff, wait = waitForRanking, readOnly = false }: Omit<Props, 'adjustments'> & { adjustment: PreferenceAdjustment }) {
  const [phase, setPhase] = useState<'editing' | 'saving' | 'polling' | 'succeeded' | 'error'>('editing')
  const [error, setError] = useState('')
  const [accepted, setAccepted] = useState<DisableAdjustmentAccepted | null>(null)
  const saved = useRef<DisableAdjustmentAccepted | null>(null)
  const busy = useRef(false)
  const abort = useRef<AbortController | null>(null)
  const updated = useRef(onRankingUpdated)
  useEffect(() => { updated.current = onRankingUpdated }, [onRankingUpdated])
  useEffect(() => () => abort.current?.abort(), [])

  async function run() {
	if (readOnly) { setError('Демо-режим только для просмотра — корректировки доступны владельцу.'); return }
    if (busy.current || phase === 'succeeded') return
    busy.current = true; abort.current?.abort()
    const controller = new AbortController(); abort.current = controller
    setError('')
    let workflow = saved.current
    if (!workflow) {
      setPhase('saving')
      try { workflow = await disable(adjustment.id, csrfToken, controller.signal) }
      catch { if (!controller.signal.aborted) { setPhase('error'); setError('Не удалось отключить корректировку. Попробуйте ещё раз.'); busy.current = false } return }
      if (controller.signal.aborted) return
      saved.current = workflow; setAccepted(workflow)
    }
    setPhase('polling')
    try {
      const diff = await pollRankingWorkflow(workflow, controller.signal, readDiff, wait)
      if (diff.rebuildStatus === 'dead') { setPhase('error'); setError('Пересборка рекомендаций завершилась с ошибкой') }
      else { setPhase('succeeded'); updated.current?.(diff) }
    } catch { if (!controller.signal.aborted) { setPhase('error'); setError('Корректировка отключена, но статус пересборки пока недоступен.') } }
    finally { if (!controller.signal.aborted) busy.current = false }
  }

  return <li><strong>{adjustment.label}</strong><span>{adjustment.effect}</span>
    <button type="button" aria-label={`Не учитывать: ${adjustment.label}`} disabled={phase === 'saving' || !!accepted} onClick={() => { void run() }}>Не учитывать</button>
    <div aria-live="polite">{phase === 'saving' ? 'Сохраняем…' : phase === 'polling' ? 'Пересобираем рекомендации…' : phase === 'succeeded' ? 'Рекомендации обновлены' : ''}</div>
    {error && <><span role={readOnly ? 'status' : 'alert'}>{error}</span>{accepted && <button type="button" aria-label={`Повторить проверку: ${adjustment.label}`} onClick={() => { void run() }}>Повторить проверку</button>}</>}
  </li>
}

export default function PreferenceAdjustments({ adjustments = [], ...props }: Props) {
  const active = adjustments.filter((item) => item.active)
  if (!active.length) return null
  return <div className="preference-adjustments"><ul>{active.map((adjustment) => <AdjustmentRow key={adjustment.id} adjustment={adjustment} {...props} />)}</ul></div>
}
