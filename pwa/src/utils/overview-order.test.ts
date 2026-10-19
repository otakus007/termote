import { describe, expect, it } from 'vitest'
import { overviewOrder } from './overview-order'

describe('overviewOrder', () => {
  it('puts the heaviest status first, then the oldest in it', () => {
    const cards = [
      { id: 'idle', status: 'idle' as const, since: '2026-10-13T09:00:00Z' },
      { id: 'new', status: 'working' as const, since: '2026-10-13T09:05:00Z' },
      { id: 'old', status: 'working' as const, since: '2026-10-13T09:01:00Z' },
      { id: 'none' },
      { id: 'blocked', status: 'blocked' as const },
      { id: 'done', status: 'done' as const, since: 'not a time' },
    ]
    expect(overviewOrder(cards).map((c) => c.id)).toEqual([
      'blocked',
      'old',
      'new',
      'done',
      'idle',
      'none',
    ])
  })
  it('keeps the given order on a tie', () => {
    const cards = [
      { id: 'a', status: 'done' as const },
      { id: 'b', status: 'done' as const },
    ]
    expect(overviewOrder(cards).map((c) => c.id)).toEqual(['a', 'b'])
  })
  it('sorts pinned panes before unpinned panes while keeping status rank inside each group', () => {
    const cards = [
      {
        id: 'idle-pinned',
        paneKey: 'g1:idle',
        status: 'idle' as const,
      },
      {
        id: 'blocked-unpinned',
        paneKey: 'g1:blocked',
        status: 'blocked' as const,
      },
      {
        id: 'working-pinned',
        paneKey: 'g1:working',
        status: 'working' as const,
      },
    ]
    expect(
      overviewOrder(cards, ['g1:idle', 'g1:working']).map((c) => c.id),
    ).toEqual(['working-pinned', 'idle-pinned', 'blocked-unpinned'])
  })
})
