export type KeyModifier = 'Ctrl' | 'Alt' | 'Shift'
export type KeyColSpan = 1 | 2 | 3
export type KeyRowSpan = 1 | 2

export type KeysPresetId =
  | 'default'
  | 'claude-code'
  | 'prefix'
  | 'vim'
  | 'navigation'
  | 'custom'

export interface KeyCell {
  id: string
  label: string
  colSpan: KeyColSpan
  rowSpan: KeyRowSpan
  // 1..4 step tokens executed in sequence (50ms inter-step delay)
  steps: string[]
  // Requires two-tap confirmation within 2.5s before sending
  danger?: boolean
  // Sticky modifier key ('Ctrl' | 'Alt' | 'Shift')
  modifier?: KeyModifier
}

export interface KeysBoardLayout {
  version: 1
  preset: KeysPresetId
  name: string
  keys: KeyCell[]
}

export const KEYS_LAYOUT_PREFIX = 'tmk1:'
export const MAX_SHARE_CODE_BYTES = 16 * 1024
export const MAX_KEYS_COUNT = 64
export const MAX_STEPS_PER_KEY = 4
export const STEP_DELAY_MS = 50
export const DANGER_CONFIRM_MS = 2500

const NAMED_STEP_TOKENS = new Set([
  'Escape',
  'Tab',
  'Shift+Tab',
  'Enter',
  'Backspace',
  'Delete',
  'Space',
  'ArrowUp',
  'ArrowDown',
  'ArrowLeft',
  'ArrowRight',
  'Home',
  'End',
  'PageUp',
  'PageDown',
  'Ctrl',
  'Alt',
  'Shift',
])

const CHORD_TOKEN_RE =
  /^(?:Ctrl\+)?(?:Alt\+)?(?:Shift\+)?(?:[A-Za-z0-9[\]\\;',$./`=-]|F[1-9]|F1[0-2]|Tab|Enter|Escape|Backspace|Delete|Space|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Home|End|PageUp|PageDown)$/

// Safe printable token (e.g. "/compact", ":wq", "[", "%", "d") with no raw
// ASCII control characters (0x00-0x1F, 0x7F).
function isPrintableToken(token: string): boolean {
  for (let i = 0; i < token.length; i++) {
    const code = token.charCodeAt(i)
    if (code <= 0x1f || code === 0x7f) return false
  }
  return true
}

export function isValidStepToken(token: unknown): token is string {
  if (typeof token !== 'string' || token.length === 0 || token.length > 64) {
    return false
  }
  if (NAMED_STEP_TOKENS.has(token)) return true
  if (CHORD_TOKEN_RE.test(token)) return true
  return isPrintableToken(token)
}

function hasForbiddenProtoKeys(obj: Record<string, unknown>): boolean {
  return (
    Object.hasOwn(obj, '__proto__') ||
    Object.hasOwn(obj, 'constructor') ||
    Object.hasOwn(obj, 'prototype')
  )
}

function isPlainRecord(val: unknown): val is Record<string, unknown> {
  if (typeof val !== 'object' || val === null || Array.isArray(val)) {
    return false
  }
  return !hasForbiddenProtoKeys(val as Record<string, unknown>)
}

const VALID_PRESETS = new Set<KeysPresetId>([
  'default',
  'claude-code',
  'prefix',
  'vim',
  'navigation',
  'custom',
])

const VALID_MODIFIERS = new Set<KeyModifier>(['Ctrl', 'Alt', 'Shift'])

export function validateKeyCell(raw: unknown, index = 0): KeyCell | null {
  if (!isPlainRecord(raw)) return null

  const id =
    typeof raw.id === 'string' &&
    raw.id.trim().length > 0 &&
    raw.id.length <= 64
      ? raw.id.trim()
      : `key-${index}`

  if (
    typeof raw.label !== 'string' ||
    raw.label.trim().length === 0 ||
    raw.label.length > 32
  ) {
    return null
  }

  const colSpan = raw.colSpan
  if (colSpan !== 1 && colSpan !== 2 && colSpan !== 3) {
    return null
  }

  const rowSpan = raw.rowSpan
  if (rowSpan !== 1 && rowSpan !== 2) {
    return null
  }

  if (
    !Array.isArray(raw.steps) ||
    raw.steps.length < 1 ||
    raw.steps.length > MAX_STEPS_PER_KEY
  ) {
    return null
  }

  const steps: string[] = []
  for (const step of raw.steps) {
    if (!isValidStepToken(step)) return null
    steps.push(step)
  }

  const cell: KeyCell = {
    id,
    label: raw.label,
    colSpan,
    rowSpan,
    steps,
  }

  if (raw.danger === true) {
    cell.danger = true
  }

  if (raw.modifier !== undefined) {
    if (
      typeof raw.modifier !== 'string' ||
      !VALID_MODIFIERS.has(raw.modifier as KeyModifier)
    ) {
      return null
    }
    cell.modifier = raw.modifier as KeyModifier
  }

  return cell
}

export function validateKeysLayout(raw: unknown): KeysBoardLayout | null {
  if (!isPlainRecord(raw)) return null
  if (raw.version !== undefined && raw.version !== 1) return null

  if (
    !Array.isArray(raw.keys) ||
    raw.keys.length < 1 ||
    raw.keys.length > MAX_KEYS_COUNT
  ) {
    return null
  }

  const preset: KeysPresetId =
    typeof raw.preset === 'string' &&
    VALID_PRESETS.has(raw.preset as KeysPresetId)
      ? (raw.preset as KeysPresetId)
      : 'custom'

  const name =
    typeof raw.name === 'string' &&
    raw.name.trim().length > 0 &&
    raw.name.length <= 48
      ? raw.name.trim()
      : 'Custom'

  const seenIds = new Set<string>()
  const keys: KeyCell[] = []
  for (let i = 0; i < raw.keys.length; i++) {
    const validated = validateKeyCell(raw.keys[i], i)
    if (!validated) return null
    let uniqueId = validated.id
    if (seenIds.has(uniqueId)) {
      uniqueId = `${validated.id}-${i}`
    }
    seenIds.add(uniqueId)
    keys.push({ ...validated, id: uniqueId })
  }

  return {
    version: 1,
    preset,
    name,
    keys,
  }
}

function toBase64Url(str: string): string {
  const bytes = new TextEncoder().encode(str)
  let binary = ''
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i])
  }
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function fromBase64Url(b64url: string): string | null {
  if (!/^[A-Za-z0-9_-]+$/.test(b64url)) return null
  const padded = b64url.replace(/-/g, '+').replace(/_/g, '/')
  const padLen = (4 - (padded.length % 4)) % 4
  try {
    const binary = atob(padded + '='.repeat(padLen))
    const bytes = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i++) {
      bytes[i] = binary.charCodeAt(i)
    }
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return null
  }
}

export function encodeKeysLayout(layout: KeysBoardLayout): string {
  const clean = validateKeysLayout(layout) ?? DEFAULT_KEYS_LAYOUT
  return KEYS_LAYOUT_PREFIX + toBase64Url(JSON.stringify(clean))
}

export function decodeKeysLayout(code: string): KeysBoardLayout | null {
  if (typeof code !== 'string') return null
  const trimmed = code.trim()
  if (
    trimmed.length === 0 ||
    trimmed.length > MAX_SHARE_CODE_BYTES ||
    !trimmed.startsWith(KEYS_LAYOUT_PREFIX)
  ) {
    return null
  }
  const payload = trimmed.slice(KEYS_LAYOUT_PREFIX.length)
  if (!payload) return null
  const jsonText = fromBase64Url(payload)
  if (!jsonText || jsonText.length > MAX_SHARE_CODE_BYTES) return null

  try {
    const parsed: unknown = JSON.parse(jsonText)
    return validateKeysLayout(parsed)
  } catch {
    return null
  }
}

export const DEFAULT_KEYS_LAYOUT: KeysBoardLayout = {
  version: 1,
  preset: 'default',
  name: 'Default',
  keys: [
    { id: 'esc', label: 'Esc', colSpan: 1, rowSpan: 1, steps: ['Escape'] },
    { id: 'tab', label: 'Tab', colSpan: 1, rowSpan: 1, steps: ['Tab'] },
    {
      id: 'shift-tab',
      label: '⇧Tab',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Shift+Tab'],
    },
    {
      id: 'ctrl',
      label: 'Ctrl',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl'],
      modifier: 'Ctrl',
    },
    {
      id: 'alt',
      label: 'Alt',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Alt'],
      modifier: 'Alt',
    },
    {
      id: 'shift',
      label: 'Shift',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Shift'],
      modifier: 'Shift',
    },
    {
      id: 'ctrl-c',
      label: '^C',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+C'],
      danger: true,
    },
    { id: 'up', label: '↑', colSpan: 1, rowSpan: 1, steps: ['ArrowUp'] },
    { id: 'down', label: '↓', colSpan: 1, rowSpan: 1, steps: ['ArrowDown'] },
    { id: 'left', label: '←', colSpan: 1, rowSpan: 1, steps: ['ArrowLeft'] },
    { id: 'right', label: '→', colSpan: 1, rowSpan: 1, steps: ['ArrowRight'] },
    {
      id: 'ctrl-d',
      label: '^D',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+D'],
      danger: true,
    },
    { id: 'enter', label: '↵', colSpan: 2, rowSpan: 1, steps: ['Enter'] },
  ],
}

export const CLAUDE_CODE_KEYS_LAYOUT: KeysBoardLayout = {
  version: 1,
  preset: 'claude-code',
  name: 'Claude Code',
  keys: [
    { id: 'esc', label: 'Esc', colSpan: 1, rowSpan: 1, steps: ['Escape'] },
    {
      id: 'mode-cycle',
      label: '⇧Tab Mode',
      colSpan: 2,
      rowSpan: 1,
      steps: ['Shift+Tab'],
    },
    {
      id: 'compact',
      label: '/compact',
      colSpan: 2,
      rowSpan: 1,
      steps: ['/compact', 'Enter'],
    },
    {
      id: 'clear',
      label: '/clear',
      colSpan: 2,
      rowSpan: 1,
      steps: ['/clear', 'Enter'],
      danger: true,
    },
    { id: 'tab', label: 'Tab', colSpan: 1, rowSpan: 1, steps: ['Tab'] },
    { id: 'up', label: '↑', colSpan: 1, rowSpan: 1, steps: ['ArrowUp'] },
    { id: 'down', label: '↓', colSpan: 1, rowSpan: 1, steps: ['ArrowDown'] },
    {
      id: 'ctrl',
      label: 'Ctrl',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl'],
      modifier: 'Ctrl',
    },
    {
      id: 'ctrl-c',
      label: '^C',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+C'],
      danger: true,
    },
    { id: 'enter', label: '↵', colSpan: 2, rowSpan: 1, steps: ['Enter'] },
  ],
}

export const PREFIX_KEYS_LAYOUT: KeysBoardLayout = {
  version: 1,
  preset: 'prefix',
  name: 'Prefix',
  keys: [
    {
      id: 'copy-mode',
      label: '^B [',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', '['],
    },
    {
      id: 'new-win',
      label: '^B c',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', 'c'],
    },
    {
      id: 'next-win',
      label: '^B n',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', 'n'],
    },
    {
      id: 'prev-win',
      label: '^B p',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', 'p'],
    },
    {
      id: 'split-h',
      label: '^B %',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', '%'],
    },
    {
      id: 'zoom',
      label: '^B z',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', 'z'],
    },
    {
      id: 'detach',
      label: '^B d',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Ctrl+B', 'd'],
      danger: true,
    },
    { id: 'esc', label: 'Esc', colSpan: 1, rowSpan: 1, steps: ['Escape'] },
    { id: 'up', label: '↑', colSpan: 1, rowSpan: 1, steps: ['ArrowUp'] },
    { id: 'down', label: '↓', colSpan: 1, rowSpan: 1, steps: ['ArrowDown'] },
    { id: 'left', label: '←', colSpan: 1, rowSpan: 1, steps: ['ArrowLeft'] },
    { id: 'right', label: '→', colSpan: 1, rowSpan: 1, steps: ['ArrowRight'] },
    { id: 'enter', label: '↵', colSpan: 2, rowSpan: 1, steps: ['Enter'] },
  ],
}

export const VIM_KEYS_LAYOUT: KeysBoardLayout = {
  version: 1,
  preset: 'vim',
  name: 'Vim',
  keys: [
    { id: 'esc', label: 'Esc', colSpan: 2, rowSpan: 1, steps: ['Escape'] },
    {
      id: 'vim-w',
      label: ':w',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Escape', ':w', 'Enter'],
    },
    {
      id: 'vim-wq',
      label: ':wq',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Escape', ':wq', 'Enter'],
    },
    {
      id: 'vim-q',
      label: ':q!',
      colSpan: 1,
      rowSpan: 1,
      steps: ['Escape', ':q!', 'Enter'],
      danger: true,
    },
    { id: 'vim-u', label: 'u', colSpan: 1, rowSpan: 1, steps: ['u'] },
    { id: 'vim-redo', label: '^R', colSpan: 1, rowSpan: 1, steps: ['Ctrl+R'] },
    { id: 'vim-h', label: 'h', colSpan: 1, rowSpan: 1, steps: ['h'] },
    { id: 'vim-j', label: 'j', colSpan: 1, rowSpan: 1, steps: ['j'] },
    { id: 'vim-k', label: 'k', colSpan: 1, rowSpan: 1, steps: ['k'] },
    { id: 'vim-l', label: 'l', colSpan: 1, rowSpan: 1, steps: ['l'] },
    { id: 'vim-gg', label: 'gg', colSpan: 1, rowSpan: 1, steps: ['g', 'g'] },
    { id: 'vim-G', label: 'G', colSpan: 1, rowSpan: 1, steps: ['G'] },
    { id: 'vim-search', label: '/', colSpan: 1, rowSpan: 1, steps: ['/'] },
  ],
}

export const NAVIGATION_KEYS_LAYOUT: KeysBoardLayout = {
  version: 1,
  preset: 'navigation',
  name: 'Navigation',
  keys: [
    { id: 'home', label: 'Home', colSpan: 1, rowSpan: 1, steps: ['Home'] },
    { id: 'end', label: 'End', colSpan: 1, rowSpan: 1, steps: ['End'] },
    { id: 'pgup', label: 'PgUp', colSpan: 1, rowSpan: 1, steps: ['PageUp'] },
    { id: 'pgdn', label: 'PgDn', colSpan: 1, rowSpan: 1, steps: ['PageDown'] },
    { id: 'ctrl-a', label: '^A', colSpan: 1, rowSpan: 1, steps: ['Ctrl+A'] },
    { id: 'ctrl-e', label: '^E', colSpan: 1, rowSpan: 1, steps: ['Ctrl+E'] },
    { id: 'ctrl-u', label: '^U', colSpan: 1, rowSpan: 1, steps: ['Ctrl+U'] },
    { id: 'up', label: '↑', colSpan: 1, rowSpan: 1, steps: ['ArrowUp'] },
    { id: 'down', label: '↓', colSpan: 1, rowSpan: 1, steps: ['ArrowDown'] },
    { id: 'left', label: '←', colSpan: 1, rowSpan: 1, steps: ['ArrowLeft'] },
    { id: 'right', label: '→', colSpan: 1, rowSpan: 1, steps: ['ArrowRight'] },
    { id: 'tab', label: 'Tab', colSpan: 1, rowSpan: 1, steps: ['Tab'] },
    { id: 'enter', label: '↵', colSpan: 2, rowSpan: 1, steps: ['Enter'] },
  ],
}

export const BUILTIN_KEYS_PRESETS: Record<
  Exclude<KeysPresetId, 'custom'>,
  KeysBoardLayout
> = {
  default: DEFAULT_KEYS_LAYOUT,
  'claude-code': CLAUDE_CODE_KEYS_LAYOUT,
  prefix: PREFIX_KEYS_LAYOUT,
  vim: VIM_KEYS_LAYOUT,
  navigation: NAVIGATION_KEYS_LAYOUT,
}

export interface StickyModifiersState {
  Ctrl: 'off' | 'latched' | 'locked'
  Alt: 'off' | 'latched' | 'locked'
  Shift: 'off' | 'latched' | 'locked'
}

export const INITIAL_STICKY_MODIFIERS: StickyModifiersState = {
  Ctrl: 'off',
  Alt: 'off',
  Shift: 'off',
}

// Single tap: off -> latched; latched -> off; locked -> off.
// Double tap (within 320ms): -> locked.
export function nextStickyModifierState(
  current: StickyModifiersState,
  mod: KeyModifier,
  isDoubleTap: boolean,
): StickyModifiersState {
  const prev = current[mod]
  if (isDoubleTap) {
    return { ...current, [mod]: prev === 'locked' ? 'off' : 'locked' }
  }
  const next = prev === 'off' ? 'latched' : 'off'
  return { ...current, [mod]: next }
}

// Clears 'latched' modifiers after a non-modifier key is pressed while keeping 'locked'.
export function consumeLatchedModifiers(
  current: StickyModifiersState,
): StickyModifiersState {
  if (
    current.Ctrl !== 'latched' &&
    current.Alt !== 'latched' &&
    current.Shift !== 'latched'
  ) {
    return current
  }
  return {
    Ctrl: current.Ctrl === 'latched' ? 'off' : current.Ctrl,
    Alt: current.Alt === 'latched' ? 'off' : current.Alt,
    Shift: current.Shift === 'latched' ? 'off' : current.Shift,
  }
}

export interface KeyStepCallbacks {
  onKeyPress: (key: string) => void
  onCtrlKey: (key: string) => void
  onCtrlShiftKey?: (key: string) => void
  onSendText?: (text: string) => void
}

// Executes a single step token with any active sticky modifiers applied.
export function dispatchStepToken(
  token: string,
  mods: StickyModifiersState,
  cb: KeyStepCallbacks,
): void {
  const ctrlActive = mods.Ctrl !== 'off'
  const altActive = mods.Alt !== 'off'
  const shiftActive = mods.Shift !== 'off'

  // Check if token is already a chord like Ctrl+C or Ctrl+Shift+C
  if (token.startsWith('Ctrl+Shift+') && token.length === 12) {
    const ch = token.slice(11).toUpperCase()
    if (cb.onCtrlShiftKey) cb.onCtrlShiftKey(ch)
    else cb.onCtrlKey(ch)
    return
  }
  if (token.startsWith('Ctrl+') && token.length === 6) {
    const ch = token.slice(5).toUpperCase()
    if (shiftActive && cb.onCtrlShiftKey) {
      cb.onCtrlShiftKey(ch)
    } else {
      cb.onCtrlKey(ch)
    }
    return
  }

  // Named navigation/editing keys
  if (token === 'Tab' && shiftActive) {
    cb.onKeyPress('Shift+Tab')
    return
  }
  if (
    token === 'Escape' ||
    token === 'Tab' ||
    token === 'Shift+Tab' ||
    token === 'Enter' ||
    token === 'Backspace' ||
    token === 'Delete' ||
    token === 'ArrowUp' ||
    token === 'ArrowDown' ||
    token === 'ArrowLeft' ||
    token === 'ArrowRight' ||
    token === 'Home' ||
    token === 'End' ||
    token === 'PageUp' ||
    token === 'PageDown'
  ) {
    cb.onKeyPress(token)
    return
  }
  if (token === 'Space') {
    if (ctrlActive) cb.onCtrlKey(' ')
    else if (cb.onSendText) cb.onSendText(altActive ? '\x1b ' : ' ')
    else cb.onKeyPress(' ')
    return
  }

  // Single character with sticky modifiers
  if (token.length === 1) {
    const ch = shiftActive ? token.toUpperCase() : token
    if (ctrlActive && shiftActive && cb.onCtrlShiftKey) {
      cb.onCtrlShiftKey(ch.toUpperCase())
      return
    }
    if (ctrlActive) {
      cb.onCtrlKey(ch.toUpperCase())
      return
    }
    if (altActive) {
      if (cb.onSendText) cb.onSendText(`\x1b${ch}`)
      else cb.onKeyPress(`\x1b${ch}`)
      return
    }
    if (cb.onSendText) cb.onSendText(ch)
    else cb.onKeyPress(ch)
    return
  }

  // Multi-character text / slash command step
  if (cb.onSendText) {
    cb.onSendText(token)
  } else {
    cb.onKeyPress(token)
  }
}
