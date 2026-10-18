import { createContext, useContext } from 'react'
import type { Recommendation } from '../api/types'

export const CalendarFeedbackContext = createContext<{ open(item: Recommendation, opener: HTMLElement): void; readOnly?: boolean } | null>(null)
export function useCalendarFeedback() { return useContext(CalendarFeedbackContext) }
