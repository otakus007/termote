import { AlertTriangle, LoaderCircle } from 'lucide-react'
import { useState } from 'react'
import { agentLabel } from '../chat-agents'
import type {
  LaunchersResponse,
  LaunchOptionChip,
  StartAgentKind,
  StartAgentOptionsPayload,
} from '../hooks/use-mux-api'
import type { StartRecord } from '../hooks/use-start-agent'
import { Button } from './ui/button'

export const DEFAULT_HARNESS_OPTIONS: Record<string, LaunchOptionChip[]> = {
  claude: [
    {
      id: 'skip-perms',
      label: 'Skip permissions',
      flag: '--dangerously-skip-permissions',
      noPrompts: true,
    },
    { id: 'continue', label: 'Continue', flag: '--continue' },
    { id: 'resume', label: 'Resume', flag: '--resume' },
  ],
  codex: [
    { id: 'full-auto', label: 'Full auto', flag: '--full-auto' },
    {
      id: 'yolo',
      label: 'Bypass approvals',
      flag: '--dangerously-bypass-approvals-and-sandbox',
      noPrompts: true,
    },
  ],
}

interface StartAgentPanelProps {
  // The agents the server can start (Codex only where it has a Chat view)
  kinds: StartAgentKind[]
  // This pane's start, if one was made from this page
  record?: StartRecord
  // Optional launchers, recipes, recent commands, and option chip overrides
  launchers?: LaunchersResponse
  optionsByHarness?: Record<string, LaunchOptionChip[]>
  onStart: (kind: StartAgentKind, options?: StartAgentOptionsPayload) => void
}

// The Chat view of a pane that shows only its shell: starts Claude Code,
// Codex, or a configured harness/recipe there, and says why a start was refused.
export function StartAgentPanel({
  kinds,
  record,
  launchers,
  optionsByHarness,
  onStart,
}: StartAgentPanelProps) {
  const busy = record?.phase === 'sending' || record?.phase === 'starting'
  const [selectedFlags, setSelectedFlags] = useState<string[]>([])
  const [confirmKind, setConfirmKind] = useState<StartAgentKind | null>(null)

  const chipCatalog =
    optionsByHarness ?? (launchers ? DEFAULT_HARNESS_OPTIONS : undefined)
  const availableChips: LaunchOptionChip[] = chipCatalog
    ? kinds.flatMap((k) => chipCatalog[k] ?? [])
    : []

  const toggleFlag = (flag: string) => {
    setSelectedFlags((prev) =>
      prev.includes(flag) ? prev.filter((f) => f !== flag) : [...prev, flag],
    )
  }

  const hasNoPromptsSelected = (kind: StartAgentKind) => {
    const chips = chipCatalog?.[kind] ?? []
    return chips.some((c) => c.noPrompts && selectedFlags.includes(c.flag))
  }

  const triggerStart = (kind: StartAgentKind) => {
    const kindChips = chipCatalog?.[kind] ?? []
    const validFlags = selectedFlags.filter((f) =>
      kindChips.some((c) => c.flag === f),
    )
    if (validFlags.length === 0) {
      onStart(kind)
      return
    }
    if (hasNoPromptsSelected(kind)) {
      setConfirmKind(kind)
      return
    }
    onStart(kind, { harness: kind, options: validFlags })
  }

  const confirmNoPromptsStart = () => {
    if (!confirmKind) return
    const kind = confirmKind
    const kindChips = chipCatalog?.[kind] ?? []
    const validFlags = selectedFlags.filter((f) =>
      kindChips.some((c) => c.flag === f),
    )
    setConfirmKind(null)
    onStart(kind, { harness: kind, options: validFlags })
  }

  return (
    <>
      <p>Start an agent in this pane</p>
      <div className="flex flex-wrap justify-center gap-2">
        {kinds.map((kind) => (
          <Button
            key={kind}
            variant="secondary"
            disabled={busy}
            onClick={() => triggerStart(kind)}
          >
            {agentLabel(kind)}
          </Button>
        ))}
      </div>
      {availableChips.length > 0 && (
        <fieldset
          className="flex flex-wrap justify-center gap-1.5 border-0 p-0"
          aria-label="Launch options"
        >
          {availableChips.map((chip) => {
            const active = selectedFlags.includes(chip.flag)
            return (
              <button
                key={chip.flag}
                type="button"
                disabled={busy}
                aria-pressed={active}
                onClick={() => toggleFlag(chip.flag)}
                className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs transition-colors ${
                  active
                    ? 'border-accent bg-accent/15 text-fg font-medium'
                    : 'border-border bg-surface text-fg-muted hover:text-fg'
                }`}
              >
                <span>{chip.label}</span>
                {chip.noPrompts && (
                  <span className="inline-flex items-center gap-0.5 rounded bg-danger/15 px-1.5 py-0.5 text-[10px] font-semibold text-danger">
                    <AlertTriangle size={10} aria-hidden="true" />
                    No prompts
                  </span>
                )}
              </button>
            )
          })}
        </fieldset>
      )}
      {confirmKind && (
        <div
          role="alertdialog"
          aria-label="Confirm unprompted launch"
          className="flex max-w-sm flex-col gap-2 rounded-card border border-danger/40 bg-danger/10 p-3 text-left text-xs text-fg"
        >
          <p className="font-medium text-danger">
            Start {agentLabel(confirmKind)} without permission prompts?
          </p>
          <p className="text-fg-muted">
            The selected option skips interactive approval prompts in this pane.
          </p>
          <div className="flex justify-end gap-2">
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setConfirmKind(null)}
            >
              Cancel
            </Button>
            <Button size="sm" variant="danger" onClick={confirmNoPromptsStart}>
              Start anyway
            </Button>
          </div>
        </div>
      )}
      {(() => {
        const recipes = launchers?.recipes ?? launchers?.launchers ?? []
        if (recipes.length === 0) return null
        return (
          <div className="flex flex-col items-center gap-1.5">
            <span className="text-xs text-fg-subtle">Recipes</span>
            <div className="flex flex-wrap justify-center gap-1.5">
              {recipes.map((recipe) => {
                const targetKind = (recipe.harness ||
                  kinds[0] ||
                  'claude') as StartAgentKind
                return (
                  <Button
                    key={recipe.id}
                    size="sm"
                    variant="ghost"
                    disabled={busy}
                    onClick={() =>
                      onStart(targetKind, {
                        recipeId: recipe.id,
                        harness: recipe.harness,
                        options: recipe.options,
                        command: recipe.command,
                      })
                    }
                  >
                    {recipe.name}
                  </Button>
                )
              })}
            </div>
          </div>
        )
      })()}
      {launchers && launchers.recentCommands.length > 0 && (
        <div className="flex flex-col items-center gap-1.5">
          <span className="text-xs text-fg-subtle">Recent commands</span>
          <div className="flex flex-wrap justify-center gap-1.5">
            {launchers.recentCommands.map((entry) => {
              const cmd = typeof entry === 'string' ? entry : entry.command
              return (
                <Button
                  key={cmd}
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  className="font-term text-xs"
                  onClick={() =>
                    onStart((kinds[0] || 'claude') as StartAgentKind, {
                      command: cmd,
                    })
                  }
                >
                  {cmd}
                </Button>
              )
            })}
          </div>
        </div>
      )}
      {busy && (
        <p className="flex items-center gap-2" role="status">
          <LoaderCircle
            size={16}
            aria-hidden="true"
            className="motion-safe:animate-spin"
          />
          Starting {agentLabel(record.kind)}…
        </p>
      )}
      {record?.phase === 'failed' && (
        <p role="alert" className="text-danger">
          {record.error}
        </p>
      )}
    </>
  )
}
