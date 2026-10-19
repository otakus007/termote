import { useEffect, useRef } from 'react'
import type { AgentStatus, Session, SessionGroup } from '../types/session'
import {
  type AgentEvent,
  notificationContent,
  type PendingNotify,
  shouldNotify,
  stepSettleTracker,
} from '../utils/agent-notify'
import { parseDeepLink } from '../utils/deep-link'
import { notificationSupport } from '../utils/notify-permission'
import { onPaneRemap, remapEntries } from '../utils/pane-remap'

interface Options {
  sessions: Session[]
  groups: SessionGroup[]
  // Pane on screen; it never notifies while the page is visible and focused
  activePaneId?: string
  enabled: boolean
  // The server confirmed this device's Web Push subscription: it notifies
  // instead, and the page stays quiet so nothing shows twice.
  pushActive: boolean
  blockedSettleSec?: number
  doneSettleSec?: number
}

// Raises a notification when an agent becomes blocked or ends a turn, from
// each new snapshot this page polls. Shown through the service worker, whose
// click handler (public/notify-sw.js) sends the pane's link back here.
export function useAgentNotifications(opts: Options) {
  const optsRef = useRef(opts)
  optsRef.current = opts
  const prevRef = useRef(new Map<string, AgentStatus>())
  const pendingRef = useRef(new Map<string, PendingNotify>())

  // A status follows its pane when a tmux window move shifts the ids, so
  // one agent's last status is never paired with another's (the server's
  // push watcher keys by pane too).
  useEffect(
    () =>
      onPaneRemap((shift) => {
        remapEntries(prevRef.current, shift)
        remapEntries(pendingRef.current, shift)
      }),
    [],
  )

  useEffect(() => {
    const dispatchEvents = (events: AgentEvent[]) => {
      if (events.length === 0) return
      const o = optsRef.current
      const ctx = {
        enabled: o.enabled,
        permission: notificationSupport(),
        pushActive: o.pushActive,
        visible: document.visibilityState === 'visible',
        focused: document.hasFocus(),
        activePaneId: o.activePaneId,
      }
      const shown = events.filter((e) => shouldNotify(e, ctx))
      if (shown.length === 0) return
      navigator.serviceWorker.ready
        .then((reg) => {
          for (const e of shown) {
            const { title, options } = notificationContent(
              e,
              o.sessions,
              o.groups,
            )
            reg.showNotification(title, options).catch(() => {})
          }
        })
        .catch(() => {})
    }

    const blockedSettleMs = (optsRef.current.blockedSettleSec ?? 0) * 1000
    const doneSettleMs = (optsRef.current.doneSettleSec ?? 0) * 1000
    const now = Date.now()
    const { nextStatuses, nextPending, readyEvents } = stepSettleTracker(
      prevRef.current,
      pendingRef.current,
      opts.sessions,
      now,
      { blockedSettleMs, doneSettleMs },
    )
    prevRef.current = nextStatuses
    pendingRef.current = nextPending
    dispatchEvents(readyEvents)

    if (nextPending.size === 0) return
    let minWait = Number.POSITIVE_INFINITY
    for (const pend of nextPending.values()) {
      const settleMs =
        pend.event.kind === 'blocked' ? blockedSettleMs : doneSettleMs
      const rem = Math.max(0, settleMs - (now - pend.since))
      if (rem < minWait) minWait = rem
    }
    const timer = setTimeout(() => {
      const firedAt = Date.now()
      const res = stepSettleTracker(
        prevRef.current,
        pendingRef.current,
        optsRef.current.sessions,
        firedAt,
        {
          blockedSettleMs: (optsRef.current.blockedSettleSec ?? 0) * 1000,
          doneSettleMs: (optsRef.current.doneSettleSec ?? 0) * 1000,
        },
      )
      prevRef.current = res.nextStatuses
      pendingRef.current = res.nextPending
      dispatchEvents(res.readyEvents)
    }, minWait)
    return () => clearTimeout(timer)
  }, [opts.sessions])

  // A click on a notification while this page is open opens its pane here.
  useEffect(() => {
    if (!('serviceWorker' in navigator)) return
    const sw = navigator.serviceWorker
    const onMessage = (e: MessageEvent) => {
      const hash = e.data?.type === 'termote-open' ? e.data.hash : undefined
      if (typeof hash === 'string' && parseDeepLink(hash)) {
        window.location.hash = hash
      }
    }
    sw.addEventListener('message', onMessage)
    return () => sw.removeEventListener('message', onMessage)
  }, [])
}
