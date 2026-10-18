import { expect, it, vi } from 'vitest'
import { pollRankingWorkflow, waitForRanking } from './rankingWorkflow'

it('backs off to 3000ms and only returns terminal diffs', async () => {
  const read = vi.fn()
  for (let i = 0; i < 8; i++) read.mockResolvedValueOnce({ rebuildStatus: 'pending' })
  read.mockResolvedValueOnce({ rebuildStatus: 'succeeded' })
  const wait = vi.fn().mockResolvedValue(undefined)
  await expect(pollRankingWorkflow({ rebuildJobId: 'job', previousRankingVersion: 4 }, new AbortController().signal, read, wait)).resolves.toEqual({ rebuildStatus: 'succeeded' })
  expect(wait.mock.calls.map(([delay]) => delay)).toEqual([300, 450, 675, 1013, 1520, 2280, 3000, 3000, 3000])
})

it('aborts its delay and never reads a cancelled workflow', async () => {
  const abort = new AbortController()
  const pending = waitForRanking(3000, abort.signal)
  abort.abort()
  await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
  const read = vi.fn()
  await expect(pollRankingWorkflow({ rebuildJobId: 'job', previousRankingVersion: 4 }, abort.signal, read)).rejects.toMatchObject({ name: 'AbortError' })
  expect(read).not.toHaveBeenCalled()
})
