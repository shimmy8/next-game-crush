import { useState } from 'react'
import type { Recommendation } from '../api/types'
import { useGameFeedback } from './FeedbackWorkflowContext'
import FeedbackDialog from './FeedbackDialog'
import { useCalendarFeedback } from './CalendarFeedbackContext'

export type FeedbackLauncherProps = { gameId?: string; recommendationId: string; rankingVersion: number; gameName: string; coverImageUrl?: string; item?: Recommendation; readOnly?: boolean }
export default function FeedbackLauncher(props: FeedbackLauncherProps) {
  const gameId = props.gameId ?? props.item?.gameId ?? props.recommendationId
  const coordinator = useCalendarFeedback(); const workflow = useGameFeedback(gameId)
  const [open, setOpen] = useState(false); const [opener, setOpener] = useState<HTMLElement | null>(null); const [notice, setNotice] = useState('')
  return <><button className="feedback-launcher" type="button" disabled={workflow.busy || workflow.tombstoned} onClick={(event) => { if (props.readOnly || coordinator?.readOnly) { setNotice('Хорошая попытка — реакции пока только для владельца.'); return }; if (coordinator && props.item) coordinator.open(props.item, event.currentTarget); else { setOpener(event.currentTarget); setOpen(true) } }}>Оценить</button>{notice && <span role="status" className="demo-action-notice">{notice}</span>}{!coordinator && <FeedbackDialog {...props} open={open} onClose={() => setOpen(false)} returnFocusTo={opener} />}</>
}
