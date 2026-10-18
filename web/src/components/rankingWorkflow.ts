import { getRankingDiff } from '../api/client'
import type { RankingDiff } from '../api/types'

export type RankingWorkflow = { rebuildJobId: string; previousRankingVersion: number }
function aborted() { return new DOMException('Aborted', 'AbortError') }
export function waitForRanking(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(aborted()); return }
    const timer = window.setTimeout(() => { signal.removeEventListener('abort', cancel); resolve() }, milliseconds)
    function cancel() { window.clearTimeout(timer); reject(aborted()) }
    signal.addEventListener('abort', cancel, { once: true })
  })
}
export async function pollRankingWorkflow(workflow: RankingWorkflow, signal: AbortSignal, readDiff = getRankingDiff, wait = waitForRanking): Promise<RankingDiff> {
  let interval = 300
  while (!signal.aborted) {
    await wait(interval, signal)
    if (signal.aborted) throw aborted()
    const diff = await readDiff(workflow.previousRankingVersion, workflow.rebuildJobId, signal)
    if (signal.aborted) throw aborted()
    if (diff.rebuildStatus === 'succeeded' || diff.rebuildStatus === 'dead') return diff
    interval = Math.min(Math.round(interval * 1.5), 3000)
  }
  throw aborted()
}
