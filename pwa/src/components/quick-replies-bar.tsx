import { ChevronDown, ChevronUp, Sparkles, Terminal } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { QUICK_REPLIES_DEFAULT } from '../hooks/use-settings'
import { FOCUS_RING } from './ui/button'

export interface HarnessShortcut {
  id: 'model' | 'effort' | 'compact' | 'tree' | 'resume'
  label: string
  command: string
  terminal?: boolean
}

export function getHarnessShortcuts(agentName?: string): HarnessShortcut[] {
  const isCodex = agentName?.toLowerCase() === 'codex'
  return [
    { id: 'model', label: 'Model', command: '/model', terminal: true },
    {
      id: 'effort',
      label: 'Effort',
      command: isCodex ? '/model' : '/effort',
      terminal: isCodex,
    },
    { id: 'compact', label: 'Compact', command: '/compact' },
    {
      id: 'tree',
      label: 'Tree',
      command: isCodex ? '/worktree' : '/branch',
      terminal: isCodex,
    },
    { id: 'resume', label: 'Resume', command: '/resume', terminal: true },
  ]
}

interface Props {
  replies?: string[]
  agentName?: string
  disabled?: boolean
  confirmBeforeSend?: boolean
  defaultCollapsed?: boolean
  onSendReply: (text: string) => void
  onRunShortcut?: (shortcut: HarnessShortcut) => void
}

const CONFIRM_WINDOW_MS = 2500

export function QuickRepliesBar({
  replies = QUICK_REPLIES_DEFAULT,
  agentName,
  disabled = false,
  confirmBeforeSend = false,
  defaultCollapsed = false,
  onSendReply,
  onRunShortcut,
}: Props) {
  const [collapsed, setCollapsed] = useState(defaultCollapsed)
  const [showHarness, setShowHarness] = useState(false)
  const [armedItem, setArmedItem] = useState<string | null>(null)
  const armedTimerRef = useRef<number | null>(null)

  useEffect(() => {
    return () => {
      if (armedTimerRef.current) clearTimeout(armedTimerRef.current)
    }
  }, [])

  const armOrRun = (key: string, action: () => void) => {
    if (disabled) return
    if (confirmBeforeSend && armedItem !== key) {
      if (armedTimerRef.current) clearTimeout(armedTimerRef.current)
      setArmedItem(key)
      armedTimerRef.current = window.setTimeout(() => {
        setArmedItem(null)
        armedTimerRef.current = null
      }, CONFIRM_WINDOW_MS)
      return
    }
    if (armedItem === key) {
      if (armedTimerRef.current) {
        clearTimeout(armedTimerRef.current)
        armedTimerRef.current = null
      }
      setArmedItem(null)
    }
    action()
  }

  const shortcuts = getHarnessShortcuts(agentName)

  return (
    <div
      data-testid="quick-replies-bar"
      className="flex flex-col border-b border-border/60 px-2 py-1"
    >
      <div className="flex items-center gap-1.5">
        <button
          type="button"
          data-testid="quick-replies-toggle"
          aria-label={collapsed ? 'Show quick replies' : 'Hide quick replies'}
          aria-expanded={!collapsed}
          onClick={() => setCollapsed((prev) => !prev)}
          className={`flex h-6 shrink-0 items-center gap-1 rounded-control px-1.5 text-[11px] font-medium text-fg-muted hover:bg-surface-hover hover:text-fg ${FOCUS_RING}`}
        >
          <Sparkles size={12} aria-hidden="true" />
          <span>Quick</span>
          {collapsed ? (
            <ChevronUp size={12} aria-hidden="true" />
          ) : (
            <ChevronDown size={12} aria-hidden="true" />
          )}
        </button>

        {!collapsed && (
          <fieldset
            aria-label="Quick replies"
            className="m-0 flex min-w-0 flex-1 items-center gap-1 overflow-x-auto border-0 p-0 py-0.5"
          >
            {replies.map((chip) => {
              const key = `reply:${chip}`
              const isArmed = armedItem === key
              return (
                <button
                  key={chip}
                  type="button"
                  disabled={disabled}
                  data-quick-reply={chip}
                  data-armed={isArmed ? 'true' : undefined}
                  onClick={() => armOrRun(key, () => onSendReply(chip))}
                  className={`h-6 shrink-0 rounded-full border px-2.5 text-[11px] font-medium transition-colors disabled:opacity-50 ${FOCUS_RING} ${
                    isArmed
                      ? 'border-accent bg-accent text-white'
                      : 'border-border bg-bg text-fg hover:border-border-strong hover:bg-surface-hover'
                  }`}
                >
                  {isArmed ? `Send "${chip}"?` : chip}
                </button>
              )
            })}
          </fieldset>
        )}

        {onRunShortcut && (
          <button
            type="button"
            data-testid="harness-belt-toggle"
            aria-label="Harness shortcuts"
            aria-expanded={showHarness}
            onClick={() => setShowHarness((prev) => !prev)}
            className={`ml-auto flex h-6 shrink-0 items-center gap-1 rounded-control border px-2 text-[11px] font-medium transition-colors ${FOCUS_RING} ${
              showHarness
                ? 'border-accent bg-accent-soft text-accent-fg'
                : 'border-border bg-bg text-fg-muted hover:text-fg'
            }`}
          >
            <Terminal size={11} aria-hidden="true" />
            <span>Harness</span>
          </button>
        )}
      </div>

      {showHarness && onRunShortcut && (
        <fieldset
          aria-label="Harness shortcut belt"
          data-testid="harness-shortcut-belt"
          className="m-0 mt-1 flex min-w-0 items-center gap-1 overflow-x-auto border-0 border-t border-border/40 p-0 pt-1"
        >
          {shortcuts.map((sc) => {
            const key = `sc:${sc.id}`
            const isArmed = armedItem === key
            return (
              <button
                key={sc.id}
                type="button"
                disabled={disabled}
                data-shortcut-id={sc.id}
                onClick={() => armOrRun(key, () => onRunShortcut(sc))}
                className={`flex h-6 shrink-0 items-center gap-1 rounded-control border px-2 font-term text-[11px] transition-colors disabled:opacity-50 ${FOCUS_RING} ${
                  isArmed
                    ? 'border-accent bg-accent text-white'
                    : 'border-border bg-bg text-fg-muted hover:text-fg'
                }`}
              >
                <span>{isArmed ? `Run ${sc.command}?` : sc.label}</span>
                <span className="text-[10px] opacity-70">{sc.command}</span>
              </button>
            )
          })}
        </fieldset>
      )}
    </div>
  )
}
