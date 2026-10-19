import { ArrowDown, ArrowUp, Copy, Plus, Trash2 } from 'lucide-react'
import { useEffect, useId, useState } from 'react'
import {
  BUILTIN_KEYS_PRESETS,
  decodeKeysLayout,
  encodeKeysLayout,
  isValidStepToken,
  type KeyCell,
  type KeyColSpan,
  type KeyModifier,
  type KeyRowSpan,
  type KeysBoardLayout,
  type KeysPresetId,
  MAX_KEYS_COUNT,
  MAX_STEPS_PER_KEY,
} from '../utils/keys-layout'
import { Button, FOCUS_RING } from './ui/button'
import { Sheet } from './ui/sheet'
import { Switch } from './ui/switch'

interface Props {
  isOpen: boolean
  onClose: () => void
  layout: KeysBoardLayout
  onSave: (layout: KeysBoardLayout) => void
}

const PRESET_LIST: {
  id: Exclude<KeysPresetId, 'custom'>
  label: string
}[] = [
  { id: 'default', label: 'Default' },
  { id: 'claude-code', label: 'Claude Code' },
  { id: 'prefix', label: 'Prefix' },
  { id: 'vim', label: 'Vim' },
  { id: 'navigation', label: 'Navigation' },
]

const INPUT_CLASS = `h-9 rounded-control border border-border bg-bg px-2 text-[13px] text-fg ${FOCUS_RING}`

export function KeysBoardEditor({ isOpen, onClose, layout, onSave }: Props) {
  const [draft, setDraft] = useState<KeysBoardLayout>(layout)
  const [selectedIdx, setSelectedIdx] = useState(0)
  const [importCode, setImportCode] = useState('')
  const [importError, setImportError] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const importId = useId()

  useEffect(() => {
    if (isOpen) {
      setDraft(layout)
      setSelectedIdx(0)
      setImportCode('')
      setImportError(null)
      setCopied(false)
    }
  }, [isOpen, layout])

  const selectedKey: KeyCell | undefined = draft.keys[selectedIdx]
  const shareCode = encodeKeysLayout(draft)

  const selectPreset = (presetId: Exclude<KeysPresetId, 'custom'>) => {
    const presetLayout = BUILTIN_KEYS_PRESETS[presetId]
    setDraft({
      ...presetLayout,
      keys: presetLayout.keys.map((k) => ({ ...k, steps: [...k.steps] })),
    })
    setSelectedIdx(0)
    setImportError(null)
  }

  const updateKeyAt = (idx: number, patch: Partial<KeyCell>) => {
    setDraft((prev) => {
      const nextKeys = prev.keys.map((k, i) =>
        i === idx ? { ...k, ...patch } : k,
      )
      return {
        ...prev,
        preset: 'custom',
        name: 'Custom',
        keys: nextKeys,
      }
    })
  }

  const moveKey = (idx: number, delta: -1 | 1) => {
    const target = idx + delta
    if (target < 0 || target >= draft.keys.length) return
    setDraft((prev) => {
      const nextKeys = [...prev.keys]
      const [item] = nextKeys.splice(idx, 1)
      nextKeys.splice(target, 0, item)
      return { ...prev, preset: 'custom', name: 'Custom', keys: nextKeys }
    })
    setSelectedIdx(target)
  }

  const addKey = () => {
    if (draft.keys.length >= MAX_KEYS_COUNT) return
    const newKey: KeyCell = {
      id: `custom-${Date.now()}-${draft.keys.length}`,
      label: 'Key',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Enter'],
    }
    const nextIdx = draft.keys.length
    setDraft((prev) => ({
      ...prev,
      preset: 'custom',
      name: 'Custom',
      keys: [...prev.keys, newKey],
    }))
    setSelectedIdx(nextIdx)
  }

  const removeKey = (idx: number) => {
    if (draft.keys.length <= 1) return
    setDraft((prev) => {
      const nextKeys = prev.keys.filter((_, i) => i !== idx)
      return { ...prev, preset: 'custom', name: 'Custom', keys: nextKeys }
    })
    setSelectedIdx((prev) => Math.max(0, Math.min(prev, draft.keys.length - 2)))
  }

  const handleImport = () => {
    const decoded = decodeKeysLayout(importCode)
    if (!decoded) {
      setImportError('Invalid or unsupported tmk1: layout code')
      return
    }
    setDraft(decoded)
    setSelectedIdx(0)
    setImportCode('')
    setImportError(null)
  }

  const handleCopyCode = async () => {
    try {
      await navigator.clipboard?.writeText(shareCode)
      setCopied(true)
    } catch {
      setCopied(false)
    }
  }

  const handleSave = () => {
    onSave(draft)
    onClose()
  }

  return (
    <Sheet
      isOpen={isOpen}
      onClose={onClose}
      title="Customize Keys Board"
      className="max-md:h-(--app-height) max-md:max-h-(--app-height) max-md:pt-[env(safe-area-inset-top)] md:max-w-2xl"
    >
      <div className="flex flex-col gap-4 p-4">
        {/* Preset Selector */}
        <div className="flex flex-col gap-1.5">
          <span className="font-label text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
            Presets
          </span>
          <fieldset
            aria-label="Layout presets"
            className="m-0 flex min-w-0 flex-wrap gap-1.5 border-0 p-0"
          >
            {PRESET_LIST.map((p) => (
              <button
                key={p.id}
                type="button"
                aria-pressed={draft.preset === p.id}
                onClick={() => selectPreset(p.id)}
                className={`h-8 rounded-control border px-2.5 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                  draft.preset === p.id
                    ? 'border-accent bg-accent-soft text-accent-fg'
                    : 'border-border bg-surface text-fg-muted hover:text-fg'
                }`}
              >
                {p.label}
              </button>
            ))}
          </fieldset>
        </div>

        {/* 7-Column Live Grid Preview & Key Picker */}
        <div className="flex flex-col gap-1.5">
          <div className="flex items-center justify-between">
            <span className="font-label text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
              7-Column 2D Grid ({draft.keys.length}/{MAX_KEYS_COUNT} keys)
            </span>
            <Button
              size="sm"
              variant="ghost"
              disabled={draft.keys.length >= MAX_KEYS_COUNT}
              onClick={addKey}
            >
              <Plus size={14} aria-hidden="true" />
              Add key
            </Button>
          </div>
          <div
            data-testid="editor-keys-grid"
            className="grid grid-cols-7 gap-1 rounded-panel border border-border bg-bg p-2"
            style={{ gridTemplateColumns: 'repeat(7, minmax(0, 1fr))' }}
          >
            {draft.keys.map((cell, idx) => {
              const isSelected = idx === selectedIdx
              return (
                <button
                  key={cell.id}
                  type="button"
                  onClick={() => setSelectedIdx(idx)}
                  style={{
                    gridColumn: `span ${cell.colSpan} / span ${cell.colSpan}`,
                    gridRow: `span ${cell.rowSpan} / span ${cell.rowSpan}`,
                  }}
                  aria-pressed={isSelected}
                  className={`flex min-h-9 flex-col items-center justify-center rounded-control border px-1 py-1 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                    isSelected
                      ? 'border-accent bg-accent-soft text-accent-fg'
                      : cell.danger
                        ? 'border-danger/50 bg-danger/10 text-danger'
                        : 'border-border bg-surface text-fg'
                  }`}
                >
                  <span className="truncate max-w-full">{cell.label}</span>
                  <span className="text-[10px] text-fg-subtle">
                    {cell.colSpan}×{cell.rowSpan}
                    {cell.steps.length > 1 ? ` · ${cell.steps.length}s` : ''}
                  </span>
                </button>
              )
            })}
          </div>
        </div>

        {/* Selected Key Inspector */}
        {selectedKey && (
          <div
            data-testid="key-cell-inspector"
            className="flex flex-col gap-3 rounded-panel border border-border bg-surface p-3"
          >
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="font-label text-[12px] font-semibold text-fg">
                Edit Key #{selectedIdx + 1}: {selectedKey.label}
              </span>
              <div className="flex items-center gap-1">
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={selectedIdx === 0}
                  aria-label="Move key earlier"
                  onClick={() => moveKey(selectedIdx, -1)}
                >
                  <ArrowUp size={14} aria-hidden="true" />
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={selectedIdx === draft.keys.length - 1}
                  aria-label="Move key later"
                  onClick={() => moveKey(selectedIdx, 1)}
                >
                  <ArrowDown size={14} aria-hidden="true" />
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={draft.keys.length <= 1}
                  aria-label="Delete selected key"
                  onClick={() => removeKey(selectedIdx)}
                >
                  <Trash2 size={14} aria-hidden="true" />
                </Button>
              </div>
            </div>

            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <label className="flex flex-col gap-1 text-[12px] text-fg-muted">
                Key label
                <input
                  type="text"
                  aria-label="Key label"
                  maxLength={32}
                  value={selectedKey.label}
                  onChange={(e) =>
                    updateKeyAt(selectedIdx, {
                      label: e.target.value || 'Key',
                    })
                  }
                  className={INPUT_CLASS}
                />
              </label>

              <label className="flex flex-col gap-1 text-[12px] text-fg-muted">
                Width (colSpan 1–3)
                <select
                  aria-label="Key column span"
                  value={selectedKey.colSpan}
                  onChange={(e) =>
                    updateKeyAt(selectedIdx, {
                      colSpan: Number(e.target.value) as KeyColSpan,
                    })
                  }
                  className={INPUT_CLASS}
                >
                  <option value={1}>1 column</option>
                  <option value={2}>2 columns</option>
                  <option value={3}>3 columns</option>
                </select>
              </label>

              <label className="flex flex-col gap-1 text-[12px] text-fg-muted">
                Height (rowSpan 1–2)
                <select
                  aria-label="Key row span"
                  value={selectedKey.rowSpan}
                  onChange={(e) =>
                    updateKeyAt(selectedIdx, {
                      rowSpan: Number(e.target.value) as KeyRowSpan,
                    })
                  }
                  className={INPUT_CLASS}
                >
                  <option value={1}>1 row</option>
                  <option value={2}>2 rows</option>
                </select>
              </label>
            </div>

            <div className="flex flex-wrap items-center justify-between gap-3">
              <label className="flex items-center gap-2 text-[12px] text-fg-muted">
                Sticky modifier
                <select
                  aria-label="Sticky modifier role"
                  value={selectedKey.modifier ?? ''}
                  onChange={(e) => {
                    const mod = e.target.value as KeyModifier | ''
                    updateKeyAt(selectedIdx, {
                      modifier: mod ? mod : undefined,
                    })
                  }}
                  className={INPUT_CLASS}
                >
                  <option value="">None (regular / sequence)</option>
                  <option value="Ctrl">Ctrl (latch / lock)</option>
                  <option value="Alt">Alt (latch / lock)</option>
                  <option value="Shift">Shift (latch / lock)</option>
                </select>
              </label>

              <div className="flex items-center gap-2">
                <span className="text-[12px] text-fg-muted">
                  Two-tap danger confirm
                </span>
                <Switch
                  label="Two-tap danger confirm"
                  checked={selectedKey.danger === true}
                  onChange={(v) => updateKeyAt(selectedIdx, { danger: v })}
                />
              </div>
            </div>

            {/* 1..4 Sequence Steps */}
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center justify-between">
                <span className="text-[12px] text-fg-muted">
                  Sequence steps (1–{MAX_STEPS_PER_KEY}, 50ms interval)
                </span>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={selectedKey.steps.length >= MAX_STEPS_PER_KEY}
                  onClick={() =>
                    updateKeyAt(selectedIdx, {
                      steps: [...selectedKey.steps, 'Enter'],
                    })
                  }
                >
                  Add step
                </Button>
              </div>
              <div className="flex flex-wrap gap-2">
                {selectedKey.steps.map((step, stepIdx) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: ordered sequence of 1..4 steps
                  <div key={stepIdx} className="flex items-center gap-1">
                    <input
                      type="text"
                      aria-label={`Step ${stepIdx + 1}`}
                      value={step}
                      onChange={(e) => {
                        const val = e.target.value
                        if (!val || isValidStepToken(val)) {
                          const nextSteps = [...selectedKey.steps]
                          nextSteps[stepIdx] = val || 'Enter'
                          updateKeyAt(selectedIdx, { steps: nextSteps })
                        }
                      }}
                      className={`w-28 font-term ${INPUT_CLASS}`}
                    />
                    {selectedKey.steps.length > 1 && (
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={`Remove step ${stepIdx + 1}`}
                        onClick={() =>
                          updateKeyAt(selectedIdx, {
                            steps: selectedKey.steps.filter(
                              (_, i) => i !== stepIdx,
                            ),
                          })
                        }
                      >
                        ×
                      </Button>
                    )}
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}

        {/* Share Code Export & Import */}
        <div className="flex flex-col gap-2 rounded-panel border border-border bg-surface p-3">
          <span className="font-label text-[11px] font-semibold uppercase tracking-wider text-fg-subtle">
            Share Code (tmk1:)
          </span>
          <div className="flex items-center gap-2">
            <input
              type="text"
              readOnly
              aria-label="Exported layout code"
              data-testid="keys-share-code"
              value={shareCode}
              className={`min-w-0 flex-1 font-term text-[11px] ${INPUT_CLASS}`}
            />
            <Button size="sm" variant="ghost" onClick={handleCopyCode}>
              <Copy size={14} aria-hidden="true" />
              {copied ? 'Copied' : 'Copy code'}
            </Button>
          </div>
          <div className="flex items-center gap-2">
            <input
              id={importId}
              type="text"
              aria-label="Import layout code"
              placeholder="Paste tmk1:... code to import"
              value={importCode}
              onChange={(e) => {
                setImportCode(e.target.value)
                setImportError(null)
              }}
              className={`min-w-0 flex-1 font-term text-[12px] ${INPUT_CLASS}`}
            />
            <Button
              size="sm"
              disabled={!importCode.trim()}
              onClick={handleImport}
            >
              Import
            </Button>
          </div>
          {importError && (
            <p role="alert" className="m-0 text-[12px] text-danger">
              {importError}
            </p>
          )}
        </div>

        {/* Footer Actions */}
        <div className="flex items-center justify-end gap-2 pt-1">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button onClick={handleSave}>Save layout</Button>
        </div>
      </div>
    </Sheet>
  )
}
