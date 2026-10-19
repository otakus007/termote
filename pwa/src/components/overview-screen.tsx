import { History, X } from 'lucide-react'
import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react'
import { useActivityLog } from '../hooks/use-activity-log'
import { useAgentOverview } from '../hooks/use-agent-overview'
import { useMediaQuery } from '../hooks/use-media-query'
import {
  ackAgent,
  type CrewMemberStatus,
  fetchMachineStatus,
  type MachineStatusResponse,
} from '../hooks/use-mux-api'
import type { Session, SessionGroup, SessionPane } from '../types/session'
import { stuckFor } from '../utils/agent-stuck'
import { overviewOrder } from '../utils/overview-order'
import { sessionPaneKey } from '../utils/pane-remap'
import {
  matchesPaneFilter,
  type SidebarFilter,
  summarizePanes,
} from '../utils/session-filter'
import { matchesPaneQuery, searchTerms } from '../utils/session-search'
import { AccountUsageStrip } from './account-usage-strip'
import { ActivityLog } from './activity-log'
import { AgentCard } from './agent-card'
import { AgentFilterBar } from './agent-filter-bar'
import { HostResourcePill, MachinesView } from './machines-view'
import { NeedsMeQueue } from './needs-me-queue'
import { SearchToggle, SessionSearchField } from './session-search-field'
import { Button, FOCUS_RING, IconButton } from './ui/button'
import { Sheet } from './ui/sheet'

// The card's time is a coarse "3 min ago": a tick every 30 s keeps it right
// without rendering the list every second.
export const OVERVIEW_TICK = 30_000

export interface OverviewCard {
  session: Session
  pane: SessionPane
  status?: SessionPane['agentStatus']
  since?: string
}

// Every pane running an agent, with its tab, in the sidebar's order.
export function agentCards(sessions: Session[]): OverviewCard[] {
  return sessions.flatMap((session) =>
    (session.panes ?? [])
      .filter((p) => p.hasAgent)
      .map((pane) => ({
        session,
        pane,
        status: pane.agentStatus,
        since: pane.agentSince,
      })),
  )
}

interface Props {
  sessions: Session[]
  groups: SessionGroup[]
  // Multi-machine crew members when operating in crew federation mode
  crewMembers?: readonly CrewMemberStatus[]
  // What a group is called here ("workspace", "tmux session")
  groupNoun: string
  // The first snapshot is in: until then nothing says "no agent"
  ready: boolean
  // Milliseconds between reads (the session list's period)
  pollInterval: number
  // Server time minus this device's (ms)
  clockOffset: number
  // tmux and psmux track only the active pane of each window
  activePaneOnly: boolean
  // Whether a card opens the Chat view of an agent (else its terminal)
  opensChat: (pane: SessionPane) => boolean
  // Whether the server answers the agent's dialogs (else the terminal)
  canAnswer: (pane: SessionPane) => boolean
  // View-only: dialogs are shown, never answered
  readOnly: boolean
  // A working agent quiet this many minutes is marked stuck
  stuckMinutes: number
  onOpen: (
    session: Session,
    pane: SessionPane,
    view: 'chat' | 'terminal',
  ) => void
  onClose: () => void
  // Unset (view-only, or nowhere to start one): no button when empty
  onStartAgent?: () => void
  // The pane whose card takes focus (the header's usage chip); a new
  // object asks again
  focusPane?: { paneId: string }
  // Pinned pane keys (sessionPaneKey or pane.id) sorted to the top
  pinnedPanes?: readonly string[]
  onTogglePin?: (paneKey: string) => void
  // Host resource telemetry (/api/mux/machine) is available on the server
  machineEnabled?: boolean
}

// Every agent the server sees, in every group: those waiting on the user
// first, then working, done and idle, the longest in its state first. A card
// opens its pane; the filter is the overview's own, not the sidebar's.
export function OverviewScreen({
  sessions,
  groups,
  crewMembers,
  groupNoun,
  ready,
  pollInterval,
  clockOffset,
  activePaneOnly,
  opensChat,
  canAnswer,
  readOnly,
  stuckMinutes,
  onOpen,
  onClose,
  onStartAgent,
  focusPane,
  pinnedPanes,
  onTogglePin,
  machineEnabled = false,
}: Props) {
  const overview = useAgentOverview(pollInterval)
  const activity = useActivityLog(pollInterval)
  // The activity log: a column beside the cards on a wide screen, a sheet
  // anywhere else.
  const [logOpen, setLogOpen] = useState(false)
  const [machinesOpen, setMachinesOpen] = useState(false)
  const [machineStatus, setMachineStatus] =
    useState<MachineStatusResponse | null>(null)
  useEffect(() => {
    if (!machineEnabled) return
    const ctrl = new AbortController()
    const poll = () => {
      void fetchMachineStatus('1h', ctrl.signal)
        .then((res) => setMachineStatus(res))
        .catch(() => {})
    }
    poll()
    const id = setInterval(poll, 60_000)
    return () => {
      ctrl.abort()
      clearInterval(id)
    }
  }, [machineEnabled])
  const wide = useMediaQuery('(min-width: 1280px)')
  // Focus moves into the overview and back to where it was when it closes.
  const titleRef = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    const before = document.activeElement as HTMLElement | null
    titleRef.current?.focus()
    return () => {
      if (before?.isConnected) before.focus()
    }
  }, [])
  const [filter, setFilter] = useState<SidebarFilter>('all')
  const [machineFilter, setMachineFilter] = useState<string>('all')
  // The search box: the cards only, not the queue or the activity log
  const [searchOpen, setSearchOpen] = useState(false)
  const [query, setQuery] = useState('')
  const searchId = useId()
  const listRef = useRef<HTMLUListElement>(null)
  const closeSearch = () => {
    setQuery('')
    setSearchOpen(false)
  }
  const [now, setNow] = useState(() => Date.now() + clockOffset)
  useEffect(() => {
    setNow(Date.now() + clockOffset)
    const id = setInterval(
      () => setNow(Date.now() + clockOffset),
      OVERVIEW_TICK,
    )
    return () => clearInterval(id)
  }, [clockOffset])

  const groupOf = useMemo(() => new Map(groups.map((g) => [g.id, g])), [groups])

  const machineChips = useMemo<CrewMemberStatus[]>(() => {
    const seen = new Map<string, CrewMemberStatus>()
    for (const g of groups) {
      if (!g.machineId) continue
      if (!seen.has(g.machineId)) {
        seen.set(g.machineId, {
          id: g.machineId,
          name: g.machineName || g.machineId,
          role: g.machineId === 'local' ? 'lead' : 'member',
          status: 'online',
          latencyMs: 0,
        })
      }
    }
    if (crewMembers && crewMembers.length > 0) {
      for (const m of crewMembers) {
        seen.set(m.id, m)
      }
      return [...seen.values()]
    }
    return seen.size > 1 ? [...seen.values()] : []
  }, [crewMembers, groups])
  const isMultiMachine = machineChips.length > 0

  const [ackedPanes, setAckedPanes] = useState<ReadonlySet<string>>(
    () => new Set(),
  )
  const handleAck = useCallback((paneId: string) => {
    setAckedPanes((prev) => new Set(prev).add(paneId))
    void ackAgent(paneId).catch(() => {})
  }, [])
  const pinnedSet = useMemo(() => new Set(pinnedPanes ?? []), [pinnedPanes])
  const cards = useMemo(
    () =>
      overviewOrder(
        agentCards(sessions).map((c) => {
          const paneKey = sessionPaneKey(c.session, c.pane.id)
          const pinned = pinnedSet.has(paneKey) || pinnedSet.has(c.pane.id)
          if (c.pane.agentStatus === 'done' && ackedPanes.has(c.pane.id)) {
            const pane: SessionPane = { ...c.pane, agentStatus: 'idle' }
            return { ...c, pane, paneKey, pinned, status: 'idle' as const }
          }
          return { ...c, paneKey, pinned }
        }),
        pinnedSet,
      ),
    [sessions, ackedPanes, pinnedSet],
  )
  const matchesMachine = useCallback(
    (c: { session: Session; pane: SessionPane }) => {
      if (!isMultiMachine || machineFilter === 'all') return true
      const group = c.session.groupId
        ? groupOf.get(c.session.groupId)
        : undefined
      const mid =
        group?.machineId ?? overview.panes.get(c.pane.id)?.machineId ?? ''
      if (mid) return mid === machineFilter
      if (machineFilter === 'local') return !c.pane.id.startsWith('c:')
      return c.pane.id.startsWith(`c:${machineFilter}/`)
    },
    [isMultiMachine, machineFilter, groupOf, overview.panes],
  )
  const machineFilteredCards = useMemo(
    () => cards.filter(matchesMachine),
    [cards, matchesMachine],
  )
  const summary = useMemo(
    () => summarizePanes(machineFilteredCards.map((c) => c.pane)),
    [machineFilteredCards],
  )
  const terms = searchTerms(query)
  const shown = machineFilteredCards.filter(
    (c) =>
      matchesPaneFilter(c.pane, filter) &&
      matchesPaneQuery(
        c.session,
        c.pane,
        terms,
        groupOf.get(c.session.groupId ?? '')?.name,
      ),
  )
  // The card asked for takes focus once it is shown (the cards are drawn
  // only when ready), over the title, and keeps an accent border until focus
  // leaves it. A card shown after the user moved on takes nothing.
  const targetRef = useRef<HTMLButtonElement>(null)
  const focused = useRef<object>(undefined)
  const [highlighted, setHighlighted] = useState<string>()
  const target =
    ready && focusPane && shown.some((c) => c.pane.id === focusPane.paneId)
      ? focusPane
      : undefined
  useEffect(() => {
    if (!target || focused.current === target) return
    focused.current = target
    const at = document.activeElement
    if (at !== titleRef.current && at !== document.body) return
    // The target is shown, so its card holds the ref
    targetRef.current!.focus()
    setHighlighted(target.paneId)
  }, [target])
  const showGroups = groups.length > 1
  const placeOf = useCallback(
    (session: Session) => {
      const group = session.groupId ? groupOf.get(session.groupId) : undefined
      return showGroups && group
        ? `${group.name} / ${session.name}`
        : session.name
    },
    [groupOf, showGroups],
  )
  const queueCards = useMemo(
    () =>
      machineFilteredCards.map((c) => ({ ...c, where: placeOf(c.session) })),
    [machineFilteredCards, placeOf],
  )

  // Opening a pane from the log leaves the overview, as a card does.
  const log = (
    <ActivityLog
      events={activity.events}
      sessions={sessions}
      groups={groups}
      clockOffset={clockOffset}
      loaded={activity.loaded}
      error={activity.error}
      onOpen={(session, pane) => {
        setLogOpen(false)
        onOpen(session, pane, opensChat(pane) ? 'chat' : 'terminal')
      }}
    />
  )

  return (
    <section
      aria-labelledby="overview-title"
      className="flex h-full flex-col bg-bg"
    >
      <div className="flex shrink-0 items-center gap-2 border-b border-border px-3 py-2">
        <h2
          id="overview-title"
          ref={titleRef}
          tabIndex={-1}
          className="flex-1 text-[15px] font-semibold outline-none"
        >
          Agents
        </h2>
        {cards.length > 0 && (
          <SearchToggle
            controls={searchId}
            noun="agents"
            open={searchOpen}
            onToggle={() => (searchOpen ? closeSearch() : setSearchOpen(true))}
          />
        )}
        <HostResourcePill
          status={machineStatus}
          active={machinesOpen}
          onClick={() => setMachinesOpen((v) => !v)}
        />
        <IconButton
          onClick={() => setLogOpen((v) => !v)}
          aria-label="Activity"
          aria-pressed={logOpen}
          title="Status changes of every agent"
        >
          <History size={18} aria-hidden="true" />
        </IconButton>
        <IconButton
          onClick={onClose}
          aria-label="Close the agent overview"
          title="Close (Esc)"
        >
          <X size={18} aria-hidden="true" />
        </IconButton>
      </div>
      <div className="flex min-h-0 flex-1">
        <div className="min-h-0 flex-1 overflow-y-auto px-3 py-3">
          {machinesOpen && (
            <div className="mb-3">
              <MachinesView
                readOnly={readOnly}
                onClose={() => setMachinesOpen(false)}
                onStatusLoaded={setMachineStatus}
              />
            </div>
          )}
          {overview.error && (
            <p role="status" className="mb-3 text-[12px] text-fg-muted">
              Could not read the agents' details; retrying.
            </p>
          )}
          {!ready ? null : cards.length === 0 ? (
            <div className="flex flex-col items-center gap-3 py-10 text-center text-[13px] text-fg-muted">
              <p>No agent is running in any session.</p>
              {onStartAgent && (
                <Button onClick={onStartAgent}>
                  Start an agent in this pane
                </Button>
              )}
            </div>
          ) : (
            <>
              <AccountUsageStrip accounts={overview.accounts} now={now} />
              <NeedsMeQueue
                cards={queueCards}
                readOnly={readOnly}
                canAnswer={canAnswer}
                onOpen={onOpen}
              />
              <div className="mb-3 flex max-w-md flex-col gap-2">
                {searchOpen && (
                  <SessionSearchField
                    id={searchId}
                    label="Find an agent"
                    value={query}
                    onChange={setQuery}
                    onClose={closeSearch}
                    onSubmit={() =>
                      shown[0] &&
                      onOpen(
                        shown[0].session,
                        shown[0].pane,
                        opensChat(shown[0].pane) ? 'chat' : 'terminal',
                      )
                    }
                    // A card's first button opens it
                    onArrowDown={() =>
                      listRef.current?.querySelector('button')?.focus()
                    }
                  />
                )}
                <AgentFilterBar
                  summary={summary}
                  filter={filter}
                  onChange={setFilter}
                  unit="agent"
                />
              </div>
              {isMultiMachine && (
                <fieldset
                  aria-label="Filter by machine"
                  className="mb-3 flex flex-wrap items-center gap-1.5"
                >
                  <button
                    type="button"
                    data-testid="machine-chip-all"
                    aria-pressed={machineFilter === 'all'}
                    onClick={() => setMachineFilter('all')}
                    className={`inline-flex h-7 items-center gap-1.5 rounded-full border px-2.5 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                      machineFilter === 'all'
                        ? 'border-accent bg-accent-soft text-fg'
                        : 'border-border bg-surface text-fg-muted hover:bg-surface-raised hover:text-fg'
                    }`}
                  >
                    All machines
                  </button>
                  {machineChips.map((m) => (
                    <button
                      key={m.id}
                      type="button"
                      data-testid={`machine-chip-${m.id}`}
                      aria-pressed={machineFilter === m.id}
                      onClick={() => setMachineFilter(m.id)}
                      className={`inline-flex h-7 items-center gap-1.5 rounded-full border px-2.5 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                        machineFilter === m.id
                          ? 'border-accent bg-accent-soft text-fg'
                          : 'border-border bg-surface text-fg-muted hover:bg-surface-raised hover:text-fg'
                      }`}
                    >
                      <span
                        aria-hidden="true"
                        className={`size-2 rounded-full ${
                          m.status === 'online' ? 'bg-success' : 'bg-fg-subtle'
                        }`}
                      />
                      <span>{m.name}</span>
                    </button>
                  ))}
                </fieldset>
              )}
              {shown.length === 0 ? (
                <div className="flex flex-col items-center gap-2 py-6 text-center text-[13px] text-fg-muted">
                  No agents match
                  <Button
                    size="sm"
                    onClick={() => {
                      setFilter('all')
                      setMachineFilter('all')
                      setQuery('')
                    }}
                  >
                    Show all
                  </Button>
                </div>
              ) : (
                <ul
                  ref={listRef}
                  className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3"
                >
                  {shown.map(({ session, pane, paneKey, pinned }) => {
                    const group = session.groupId
                      ? groupOf.get(session.groupId)
                      : undefined
                    const named = showGroups && group
                    const where = named
                      ? `${group.name} / ${session.name}`
                      : session.name
                    const hostBadge = isMultiMachine
                      ? (group?.machineName ??
                        overview.panes.get(pane.id)?.machineName)
                      : undefined
                    return (
                      <li
                        key={pane.id}
                        onBlur={(e) => {
                          if (!e.currentTarget.contains(e.relatedTarget)) {
                            setHighlighted((h) =>
                              h === pane.id ? undefined : h,
                            )
                          }
                        }}
                      >
                        <AgentCard
                          session={session}
                          pane={pane}
                          where={where}
                          groupTitle={
                            named ? `${groupNoun} ${group.name}` : undefined
                          }
                          worktree={group?.worktree?.linked}
                          machineName={hostBadge}
                          overview={overview.panes.get(pane.id)}
                          overviewFailed={overview.error}
                          now={now}
                          paneKey={paneKey}
                          events={activity.events}
                          stuckFor={stuckFor(
                            pane.agentStatus,
                            overview.panes.get(pane.id)?.lastTs,
                            now,
                            stuckMinutes,
                          )}
                          opensChat={opensChat(pane)}
                          onOpen={onOpen}
                          openRef={
                            pane.id === target?.paneId ? targetRef : undefined
                          }
                          highlighted={pane.id === highlighted}
                          onAck={readOnly ? undefined : handleAck}
                          pinned={pinned}
                          onTogglePin={onTogglePin}
                        />
                      </li>
                    )
                  })}
                </ul>
              )}
              {activePaneOnly && (
                <p className="mt-4 text-[12px] text-fg-subtle">
                  Only the active pane of each window is tracked.
                </p>
              )}
            </>
          )}
        </div>
        {logOpen && wide && (
          <aside
            aria-labelledby="overview-activity-title"
            className="flex w-80 shrink-0 flex-col gap-2 overflow-y-auto border-l border-border px-3 py-3"
          >
            <h3
              id="overview-activity-title"
              className="text-[13px] font-semibold"
            >
              Activity
            </h3>
            {log}
          </aside>
        )}
      </div>
      <Sheet
        isOpen={logOpen && !wide}
        onClose={() => setLogOpen(false)}
        title="Activity"
        closeLabel="Close the activity"
      >
        <div className="overflow-y-auto px-4 pb-4">{log}</div>
      </Sheet>
    </section>
  )
}
