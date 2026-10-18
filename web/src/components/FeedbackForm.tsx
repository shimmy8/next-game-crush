import { useId, useState } from 'react'
import type { DirectFeedbackAction } from '../api/types'
import { taxonomyLabel } from '../ui/labels'
import { useGameFeedback } from './FeedbackWorkflowContext'
import { feedbackReasonOrder } from './feedbackWorkflowMachine'

export type FeedbackFormProps = { gameId?: string; recommendationId: string; rankingVersion: number; gameName?: string; initialAction?: DirectFeedbackAction | null; initialReasons?: string[] | null; readOnly?: boolean }
const reasonLabels: Record<string, string> = { genre: 'Жанр', setting: 'Сеттинг', multiplayer: 'Мультиплеер', 'single-player': 'Одиночная игра', difficulty: 'Сложность', length: 'Длина', 'early-access': 'Ранний доступ', 'already-known': 'Уже знаю', 'not-now': 'Не сейчас', other: 'Другое' }

export default function FeedbackForm(props: FeedbackFormProps) {
  const workflow = useGameFeedback(props.gameId ?? props.recommendationId)
  const [action, setAction] = useState<'like' | 'dislike' | null>(null)
  const [selectedReasons, setSelectedReasons] = useState<string[]>([])
  const [freeText, setFreeText] = useState('')
  const [validation, setValidation] = useState('')
  const [submittedAction, setSubmittedAction] = useState<'like' | 'dislike' | 'remind_me' | null>(null)
  const [readOnlyNotice, setReadOnlyNotice] = useState('')
  const id = useId(); const count = Array.from(freeText).length
  const submit = (nextAction: 'like' | 'dislike' | 'remind_me') => {
    if (props.readOnly) { setReadOnlyNotice('Хорошая попытка — реакции пока только для владельца.'); return }
    if (count > 500) { setValidation('Комментарий должен быть не длиннее 500 символов'); return }
    setValidation(''); setSubmittedAction(nextAction)
    void workflow.submit({ recommendationId: props.recommendationId, rankingVersion: props.rankingVersion, gameName: props.gameName ?? props.recommendationId, action: nextAction, ...(nextAction === 'remind_me' ? {} : { reasons: selectedReasons.length ? selectedReasons : undefined, freeText: freeText.trim() || undefined }) })
  }
  const choose = (next: 'like' | 'dislike') => props.readOnly ? setReadOnlyNotice('Хорошая попытка — реакции пока только для владельца.') : setAction(next)
  if (workflow.accepted) return <div className="feedback-status" aria-live="polite"><strong>{submittedAction === 'remind_me' ? 'Напоминание сохранено' : 'Отзыв сохранён'}</strong>
    {workflow.phase === 'polling' && <span>{submittedAction === 'remind_me' ? 'Подтверждаем напоминание…' : 'Пересобираем рекомендации…'}</span>}
    {workflow.diff && submittedAction !== 'remind_me' && <><span>Добавлено: {workflow.diff.added.length} · Убрано: {workflow.diff.removed.length} · Переставлено: {workflow.diff.reordered.length}</span>{workflow.diff.classification && <span>Сигнал: {taxonomyLabel(workflow.diff.classification.taxonomy)}, уверенность {Math.round(workflow.diff.classification.confidence * 100)}%</span>}</>}
    {workflow.error && <><span role="alert">{workflow.error}</span>{workflow.canRetry && <button type="button" onClick={workflow.retry}>Повторить проверку</button>}</>}
  </div>
  return <div className="feedback-form">{props.initialAction && <div className="feedback-status"><strong>{`Ранее выбрано: ${props.initialAction === 'like' ? 'Нравится' : 'Не моё'}`}</strong>{!!props.initialReasons?.length && <span>Причины: {props.initialReasons.map((value) => reasonLabels[value] ?? 'Другое').join(', ')}</span>}</div>}<div className="reaction-row" aria-label="Действия с рекомендацией">
    <button type="button" disabled={workflow.busy} aria-pressed={action === 'like'} onClick={() => choose('like')}>Нравится</button><button type="button" disabled={workflow.busy} aria-pressed={action === 'dislike'} onClick={() => choose('dislike')}>Не моё</button><button type="button" disabled={workflow.busy} onClick={() => submit('remind_me')}>Напомнить</button></div>
    {action && <div className="feedback-fields"><fieldset disabled={workflow.busy} className="feedback-reasons"><legend>Причины</legend>{feedbackReasonOrder.map((value) => <label key={value}><input type="checkbox" checked={selectedReasons.includes(value)} onChange={() => setSelectedReasons((current) => current.includes(value) ? current.filter((reason) => reason !== value) : [...current, value])} />{reasonLabels[value]}</label>)}</fieldset><label htmlFor={`${id}-text`}>Комментарий</label><textarea id={`${id}-text`} value={freeText} disabled={workflow.busy} aria-invalid={count > 500 || undefined} onChange={(event) => { setFreeText(event.target.value); setValidation('') }} /><span>{count} из 500 символов</span><button className="feedback-submit" type="button" disabled={workflow.busy} onClick={() => action && submit(action)}>{workflow.phase === 'submitting' ? 'Сохраняем…' : 'Сохранить отзыв'}</button></div>}
    {readOnlyNotice && <p role="status" className="demo-action-notice">{readOnlyNotice}</p>}{(validation || workflow.error) && <p role="alert" className="feedback-error">{validation || workflow.error}</p>}
  </div>
}
