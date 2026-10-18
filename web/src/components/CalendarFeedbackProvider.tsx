import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { Recommendation } from '../api/types'
import FeedbackDialog from './FeedbackDialog'
import { CalendarFeedbackContext } from './CalendarFeedbackContext'
import { useGameFeedback } from './FeedbackWorkflowContext'

function SelectedDialog({ selected, open, setOpen, clear, opener, fallback, readOnly }: { selected: Recommendation; open: boolean; setOpen(value: boolean): void; clear(): void; opener: HTMLElement | null; fallback(): HTMLElement | null; readOnly: boolean }) {
  const workflow = useGameFeedback(selected.gameId)
  useEffect(() => { if (workflow.tombstoned) { setOpen(false); clear() } }, [clear, setOpen, workflow.tombstoned])
  return <FeedbackDialog item={selected} gameId={selected.gameId} recommendationId={selected.id} rankingVersion={selected.rankingVersion} gameName={selected.name} coverImageUrl={selected.coverImageUrl} presentationOrigin={{ surface: 'calendar', recommendationId: selected.id }} open={open && !workflow.tombstoned} onClose={() => setOpen(false)} returnFocusTo={opener} fallbackFocus={fallback} readOnly={readOnly} />
}
export default function CalendarFeedbackProvider({ visibleGameIds, children, readOnly = false }: { visibleGameIds: ReadonlySet<string>; children: ReactNode; readOnly?: boolean }) {
  const [selected, setSelected] = useState<Recommendation | null>(null); const [open, setOpen] = useState(false); const [opener, setOpener] = useState<HTMLElement | null>(null); const container = useRef<HTMLDivElement>(null)
  const fallback = useCallback(() => container.current?.querySelector<HTMLElement>('.view-switch button[aria-pressed="true"]') ?? null, []); const clear = useCallback(() => setSelected(null), [])
  const visibleSelection = selected && visibleGameIds.has(selected.gameId) ? selected : null
  return <CalendarFeedbackContext.Provider value={{ readOnly, open(item, button) { setOpener(button); setSelected(item); setOpen(true) } }}><div ref={container}>{children}</div>{visibleSelection && <SelectedDialog selected={visibleSelection} open={open} setOpen={setOpen} clear={clear} opener={opener} fallback={fallback} readOnly={readOnly} />}</CalendarFeedbackContext.Provider>
}
