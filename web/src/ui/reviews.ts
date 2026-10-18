import type { ReviewSummary } from '../api/types'

const reviewCount = new Intl.NumberFormat('ru-RU')
const reviewPlural = new Intl.PluralRules('ru-RU')
const reviewNouns: Record<string, string> = { one: 'отзыв', few: 'отзыва', many: 'отзывов', other: 'отзыва' }

export function reviewLabel(reviews: ReviewSummary | undefined): string | undefined {
  if (!reviews) return undefined
  if (reviews.band === 'unrated') return 'Пока нет отзывов'
  return `${reviews.positivePercent}% положительных · ${reviewCount.format(reviews.total)} ${reviewNouns[reviewPlural.select(reviews.total)]}`
}
