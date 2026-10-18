const STATUS_LABELS: Record<string, string> = {
  active: 'В работе',
  ambiguous: 'Статус доставки неизвестен',
  available: 'Доступно',
  configured: 'Настроено',
  dead: 'Требует внимания',
  degraded: 'Есть проблемы',
  disabled: 'Отключено',
  error: 'Ошибка',
  failed: 'Ошибка',
  fresh: 'Актуально',
  incomplete: 'Не завершено',
  not_modified: 'Актуально',
  ok: 'Работает',
  pending: 'В очереди',
  queued: 'В очереди',
  ready: 'Готово',
  request_failed: 'Ошибка запроса',
  retryable: 'Ожидает повтора',
  running: 'В работе',
  sending: 'Отправляется',
  sent: 'Отправлено',
  stale: 'Требует обновления',
  succeeded: 'Завершено',
  unavailable: 'Недоступно',
  unknown: 'Нет данных',
  waiting: 'Ожидает этапы',
}

const STAGE_LABELS: Record<string, string> = {
  achievements: 'Достижения',
  ai_embedding: 'AI-вектор',
  ai_features: 'AI-признаки',
  catalog: 'Каталог Steam',
  digest: 'Дайджест',
  feedback: 'Обработка отзыва',
  owned_games: 'Библиотека Steam',
  profile: 'Профиль',
  profile_summary: 'Краткое описание профиля',
  recently_played: 'Недавняя активность',
  recommendations: 'Рекомендации',
  steam: 'Steam',
  wishlist: 'Список желаемого',
}

const JOB_LABELS: Record<string, string> = {
  'achievements.sync': 'Синхронизация достижений',
  'ai.embedding': 'Создание AI-вектора',
  'ai.features': 'Извлечение AI-признаков',
  'ai.profile_summary': 'Краткое описание профиля',
  'catalog.refresh': 'Обновление каталога Steam',
  'catalog.refresh_game': 'Обновление игры в каталоге',
  'catalog.rebuild_wakeup': 'Проверка обновления рекомендаций',
  'feedback.classify': 'Обработка отзыва',
  'notifications.digest.send': 'Отправка дайджеста',
  'profile.rebuild': 'Пересборка профиля',
  'recommendations.rebuild': 'Пересборка рекомендаций',
  'steam.catalog': 'Каталог Steam',
  'steam.source': 'Данные Steam',
  'steam.sync': 'Синхронизация Steam',
}

export const EXPLANATION_FAILURE_REASONS = [
  'disabled',
  'rate_limited',
  'timeout',
  'provider_unavailable',
  'refusal',
  'invalid_output',
  'invalid_citation',
  'unsafe_text',
  'unknown',
] as const

const EXPLANATION_FAILURE_LABELS: Record<(typeof EXPLANATION_FAILURE_REASONS)[number], string> = {
  disabled: 'OpenAI отключён',
  rate_limited: 'Лимит запросов',
  timeout: 'Тайм-аут',
  provider_unavailable: 'Сервис недоступен',
  refusal: 'Ответ отклонён',
  invalid_output: 'Некорректный ответ',
  invalid_citation: 'Некорректные ссылки на основания',
  unsafe_text: 'Небезопасный текст',
  unknown: 'Неизвестная ошибка AI',
}

const TAXONOMY_LABELS: Record<string, string> = {
  'already-known': 'Уже знаю',
  difficulty: 'Сложность',
  'early-access': 'Ранний доступ',
  genre: 'Жанр',
  length: 'Длина',
  multiplayer: 'Мультиплеер',
  'not-now': 'Не сейчас',
  other: 'Другое',
  setting: 'Сеттинг',
  'single-player': 'Одиночная игра',
}

export function statusLabel(value: string) {
  return STATUS_LABELS[value] ?? 'Неизвестно'
}

export function stageLabel(value: string) {
  const separator = value.indexOf(':')
  const key = separator === -1 ? value : value.slice(0, separator)
  const detail = separator === -1 ? '' : value.slice(separator + 1)
  const label = STAGE_LABELS[key] ?? JOB_LABELS[key] ?? 'Этап синхронизации'
  return detail ? `${label} · приложение ${detail}` : label
}

export function jobLabel(stage: string, kind: string) {
  if (stage) return stageLabel(stage)
  return JOB_LABELS[kind] ?? 'Фоновое задание'
}

export function explanationFailureLabel(value: (typeof EXPLANATION_FAILURE_REASONS)[number]) {
  return EXPLANATION_FAILURE_LABELS[value]
}

export function taxonomyLabel(value: string) {
  return TAXONOMY_LABELS[value] ?? 'Другое'
}

export function diagnosticErrorLabel(value: string) {
  if (value === 'synchronization stage failed') return 'Этап синхронизации завершился с ошибкой'
  if (value === 'upstream request failed') return 'Ошибка внешнего сервиса'
  return 'Операция завершилась с ошибкой'
}
