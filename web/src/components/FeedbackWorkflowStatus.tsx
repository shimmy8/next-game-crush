import { useFeedbackWorkflows } from './FeedbackWorkflowContext'

export function feedbackErrorCopy(code: string) {
  if (code === 'polling_unavailable') return 'Отзыв сохранён, но статус пересборки пока недоступен.'
  if (code === 'rebuild_dead') return 'Пересборка рекомендаций завершилась с ошибкой.'
  if (code === 'stale_unconfirmed') return 'Данные пока не подтверждены — повторить обновление.'
  return 'Не удалось сохранить отзыв. Попробуйте ещё раз.'
}

export default function FeedbackWorkflowStatus({ gameIds }: { gameIds: readonly string[] }) {
  const workflows = useFeedbackWorkflows()
  return <div className="feedback-workflow-statuses" aria-live="polite">{gameIds.map((gameId) => {
    const state = workflows.stateFor(gameId)
    if (!state.error) return null
    return <div className="feedback-status" key={gameId}><span role="alert">{state.error}</span>{state.canRetry && <button type="button" onClick={() => workflows.retry(gameId)}>Повторить проверку</button>}</div>
  })}</div>
}
