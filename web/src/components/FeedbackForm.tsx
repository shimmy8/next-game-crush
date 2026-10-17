import { useEffect, useRef, useState } from 'react'
import { getRankingDiff, submitFeedback } from '../api/client'
import type { FeedbackAccepted, FeedbackInput, RankingDiff } from '../api/types'

type Props = {
  recommendationId: string
  rankingVersion: number
  csrfToken: string
  onRankingUpdated?: (diff: RankingDiff) => void
  submit?: typeof submitFeedback
  readDiff?: typeof getRankingDiff
  wait?: (milliseconds: number, signal: AbortSignal) => Promise<void>
}

const reasons = [
  ['genre', 'Жанр'], ['setting', 'Сеттинг'], ['multiplayer', 'Мультиплеер'], ['single-player', 'Одиночная игра'],
  ['difficulty', 'Сложность'], ['length', 'Длина'], ['early-access', 'Ранний доступ'], ['already-known', 'Уже знаю'], ['not-now', 'Не сейчас'], ['other', 'Другое'],
]

function delay(milliseconds: number, signal: AbortSignal) {
  return new Promise<void>((resolve) => {
    const timer = window.setTimeout(done, milliseconds)
    function done() { window.clearTimeout(timer); signal.removeEventListener('abort', done); resolve() }
    signal.addEventListener('abort', done, { once: true })
  })
}

function eventKey() {
  return globalThis.crypto?.randomUUID?.() ?? `feedback-${Date.now()}-${Math.random().toString(16).slice(2)}`
}

export default function FeedbackForm({ recommendationId, rankingVersion, csrfToken, onRankingUpdated, submit = submitFeedback, readDiff = getRankingDiff, wait = delay }: Props) {
  const [action, setAction] = useState<'like' | 'dislike' | null>(null)
  const [reason, setReason] = useState('')
  const [freeText, setFreeText] = useState('')
  const [saved, setSaved] = useState(false)
	const [savedAction, setSavedAction] = useState<FeedbackInput['action'] | null>(null)
  const [pending, setPending] = useState(false)
  const [diff, setDiff] = useState<RankingDiff | null>(null)
	const [workflow, setWorkflow] = useState<FeedbackAccepted | null>(null)
  const [error, setError] = useState('')
	const [submitting, setSubmitting] = useState(false)
  const abort = useRef<AbortController | null>(null)
	const idempotencyKey = useRef('')
  useEffect(() => () => abort.current?.abort(), [])

  async function poll(previousVersion: number, jobId: string, signal: AbortSignal) {
    let interval = 300
    while (!signal.aborted) {
      await wait(interval, signal)
      if (signal.aborted) return
		const value = await readDiff(previousVersion, jobId, signal)
      if (['succeeded', 'dead'].includes(value.rebuildStatus)) {
        setPending(false); setDiff(value)
        if (value.rebuildStatus === 'succeeded') onRankingUpdated?.(value)
        else setError('Пересборка рекомендаций завершилась с ошибкой')
        return
      }
      interval = Math.min(Math.round(interval * 1.5), 3000)
    }
  }

  async function send(input: FeedbackInput) {
		if (submitting) return
    abort.current?.abort()
    const controller = new AbortController(); abort.current = controller
		setSubmitting(true); setError(''); setDiff(null)
		let accepted: FeedbackAccepted
		try {
			accepted = await submit(recommendationId, csrfToken, input, controller.signal)
		} catch {
			if (!controller.signal.aborted) { setSubmitting(false); setError('Не удалось сохранить отзыв. Попробуйте ещё раз.') }
			return
		}
		setSaved(true); setPending(true); setSubmitting(false)
		setSavedAction(input.action)
		setWorkflow(accepted)
		try {
			await poll(accepted.previousRankingVersion || rankingVersion, accepted.rebuildJobId, controller.signal)
		} catch {
			if (!controller.signal.aborted) { setPending(false); setError('Отзыв сохранён, но статус пересборки пока недоступен.') }
		}
  }

	function retryStatus() {
		if (!workflow) return
		abort.current?.abort()
		const controller = new AbortController(); abort.current = controller
		setError(''); setPending(true)
		void poll(workflow.previousRankingVersion || rankingVersion, workflow.rebuildJobId, controller.signal).catch(() => {
			if (!controller.signal.aborted) { setPending(false); setError('Отзыв сохранён, но статус пересборки пока недоступен.') }
		})
	}

  function save() {
    if (!action) return
    if (Array.from(freeText).length > 500) { setError('Комментарий должен быть не длиннее 500 символов'); return }
		if (!idempotencyKey.current) idempotencyKey.current = eventKey()
    void send({ action, reason: reason || undefined, freeText: freeText.trim() || undefined, idempotencyKey: idempotencyKey.current })
  }

	if (saved) return <div className="feedback-status" aria-live="polite"><strong>{savedAction === 'remind_me' ? 'Напоминание сохранено' : 'Отзыв сохранён'}</strong>{pending && <span>{savedAction === 'remind_me' ? 'Подтверждаем напоминание…' : 'Пересобираем рекомендации…'}</span>}{diff && savedAction !== 'remind_me' && <><span>Добавлено: {diff.added.length} · Убрано: {diff.removed.length} · Переставлено: {diff.reordered.length}</span>{diff.classification && <span>Сигнал: {diff.classification.taxonomy}, уверенность {Math.round(diff.classification.confidence * 100)}%{diff.classification.actionable ? ' · учтён' : ' · сохранён без влияния на профиль'}</span>}</>}{error && <><span role="alert">{error}</span><button type="button" onClick={retryStatus}>Повторить проверку</button></>}</div>

  return <div className="feedback-form">
    <div className="reaction-row" aria-label="Действия с рекомендацией">
		<button type="button" disabled={submitting} aria-pressed={action === 'like'} onClick={() => { idempotencyKey.current = ''; setAction('like') }}>Нравится</button>
		<button type="button" disabled={submitting} aria-pressed={action === 'dislike'} onClick={() => { idempotencyKey.current = ''; setAction('dislike') }}>Не моё</button>
		<button type="button" disabled={submitting} onClick={() => { if (!idempotencyKey.current) idempotencyKey.current = eventKey(); void send({ action: 'remind_me', idempotencyKey: idempotencyKey.current }) }}>Напомнить</button>
    </div>
    {action && <div className="feedback-fields">
      <label>Причина<select value={reason} onChange={(event) => setReason(event.target.value)}><option value="">Не выбрана</option>{reasons.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
      <label>Комментарий<textarea value={freeText} maxLength={500} aria-describedby="feedback-character-count" onChange={(event) => setFreeText(event.target.value)} /></label>
      <span id="feedback-character-count">{Array.from(freeText).length} из 500 символов</span>
		<button className="feedback-submit" type="button" disabled={submitting} onClick={save}>{submitting ? 'Сохраняем…' : 'Сохранить отзыв'}</button>
    </div>}
    {error && <p role="alert" className="feedback-error">{error}</p>}
  </div>
}
