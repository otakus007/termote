import { AGENT_STATUS_ORDER, type AgentStatus } from '../types/session'

interface Ordered {
  id?: string
  paneKey?: string
  pinned?: boolean
  status?: AgentStatus
  // When the status began (RFC 3339); absent from an older server
  since?: string
}

const rank = (s?: AgentStatus) => {
  const i = s ? AGENT_STATUS_ORDER.indexOf(s) : -1
  return i === -1 ? AGENT_STATUS_ORDER.length : i
}

const startedAt = (since?: string) => {
  const t = since ? Date.parse(since) : Number.NaN
  return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY
}

// The overview's order: pinned panes first, then the heaviest status first
// (blocked, working, done, idle), then within one status the one in it
// longest; ties keep their order (the sidebar's).
export function overviewOrder<T extends Ordered>(
  cards: T[],
  pinnedKeys?: Iterable<string>,
): T[] {
  const pinnedSet = pinnedKeys ? new Set(pinnedKeys) : undefined
  const isPinned = (card: T) =>
    Boolean(
      card.pinned ||
        (pinnedSet &&
          ((card.paneKey && pinnedSet.has(card.paneKey)) ||
            (card.id && pinnedSet.has(card.id)))),
    )
  return cards
    .map((card, i) => ({ card, i, pinned: isPinned(card) }))
    .sort(
      (a, b) =>
        Number(b.pinned) - Number(a.pinned) ||
        rank(a.card.status) - rank(b.card.status) ||
        startedAt(a.card.since) - startedAt(b.card.since) ||
        a.i - b.i,
    )
    .map(({ card }) => card)
}
