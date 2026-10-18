import { useEffect, useId, useRef } from 'react'
import { createPortal } from 'react-dom'
import type { FeedbackLauncherProps } from './FeedbackLauncher'
import FeedbackForm from './FeedbackForm'
import { GamePoster } from './GamePoster'

export type FeedbackDialogProps = FeedbackLauncherProps & { open: boolean; onClose(): void; returnFocusTo?: HTMLElement | null; fallbackFocus?: () => HTMLElement | null }
export default function FeedbackDialog({ open, onClose, returnFocusTo, fallbackFocus, ...props }: FeedbackDialogProps) {
  const dialog = useRef<HTMLDialogElement>(null); const close = useRef<HTMLButtonElement>(null); const titleId = useId(); const onCloseRef = useRef(onClose); const fallbackRef = useRef(fallbackFocus)
  useEffect(() => { onCloseRef.current = onClose; fallbackRef.current = fallbackFocus }, [onClose, fallbackFocus])
  useEffect(() => {
    if (!open || !dialog.current) return
    const element = dialog.current; if (typeof element.showModal === 'function') element.showModal(); else element.setAttribute('open', '')
    const background = Array.from(document.body.children).filter((node) => node !== element && !node.contains(element)); const previous = background.map((node) => node.hasAttribute('inert')); background.forEach((node) => node.setAttribute('inert', '')); close.current?.focus()
    const focusables = () => Array.from(element.querySelectorAll<HTMLElement>('button:not(:disabled),a[href],input:not(:disabled),textarea:not(:disabled),[tabindex="0"]'))
    const keydown = (event: KeyboardEvent) => { if (event.key === 'Escape') { event.preventDefault(); onCloseRef.current(); return }; if (event.key !== 'Tab') return; const nodes = focusables(); const first = nodes[0]; const last = nodes.at(-1); if (event.shiftKey && (document.activeElement === first || !element.contains(document.activeElement))) { event.preventDefault(); last?.focus() } else if (!event.shiftKey && (document.activeElement === last || !element.contains(document.activeElement))) { event.preventDefault(); first?.focus() } }
    const keepFocus = (event: FocusEvent) => { if (!element.contains(event.target as Node)) close.current?.focus() }
    element.addEventListener('keydown', keydown); document.addEventListener('focusin', keepFocus)
    return () => { element.removeEventListener('keydown', keydown); document.removeEventListener('focusin', keepFocus); if (typeof element.close === 'function') element.close(); else element.removeAttribute('open'); background.forEach((node, index) => { if (!previous[index]) node.removeAttribute('inert') }); const target = returnFocusTo?.isConnected ? returnFocusTo : fallbackRef.current?.(); target?.focus() }
  }, [open, returnFocusTo])
  return createPortal(<dialog ref={dialog} className="feedback-dialog" aria-modal="true" aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); onClose() }}><button ref={close} type="button" className="feedback-dialog__close" aria-label="Закрыть" onClick={onClose}>×</button><div className="feedback-dialog__identity"><GamePoster name={props.gameName} coverImageUrl={props.coverImageUrl} size="list" decorative /><h2 id={titleId}>Оценить {props.gameName}</h2></div><FeedbackForm gameId={props.gameId ?? props.item?.gameId ?? props.recommendationId} recommendationId={props.recommendationId} rankingVersion={props.rankingVersion} gameName={props.gameName} initialAction={props.item?.latestFeedbackAction} initialReasons={props.item?.latestFeedbackReasons} readOnly={props.readOnly} /></dialog>, document.body)
}
