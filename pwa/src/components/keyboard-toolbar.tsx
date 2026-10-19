import {
  ArrowDown,
  ArrowLeft,
  ArrowRight,
  ArrowUp,
  ChevronFirst,
  ChevronLast,
  ChevronsDown,
  ChevronsUp,
  Clipboard,
  Clock,
  CornerDownLeft,
  Delete,
  Ellipsis,
  Grid2x2,
  History,
  ImagePlus,
  Keyboard,
  Languages,
  Send,
  SlidersHorizontal,
  TextSelect,
  X,
} from 'lucide-react'
import {
  type ComponentProps,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import { useHaptic } from '../hooks/use-haptic'
import { scrollEdgeMask, useScrollEdges } from '../hooks/use-scroll-edges'
import {
  BUILTIN_KEYS_PRESETS,
  consumeLatchedModifiers,
  DANGER_CONFIRM_MS,
  DEFAULT_KEYS_LAYOUT,
  dispatchStepToken,
  INITIAL_STICKY_MODIFIERS,
  type KeyCell,
  type KeysBoardLayout,
  type KeysPresetId,
  nextStickyModifierState,
  STEP_DELAY_MS,
  type StickyModifiersState,
} from '../utils/keys-layout'
import { KeysBoardEditor } from './keys-board-editor'
import {
  QUICK_ACTIONS,
  type QuickActionHandlers,
  runAction,
} from './quick-actions-menu'
import { FOCUS_RING } from './ui/button'

interface Props {
  onKey: (key: string) => void
  onCtrlKey: (key: string) => void
  onShiftKey?: (key: string) => void
  onCtrlShiftKey?: (key: string) => void
  onScroll?: (direction: 'up' | 'down', pages?: boolean) => void
  onTmuxCopy?: () => void
  // Adds a Select text key that opens the pane's text to select and copy
  onSelectText?: () => void
  // Hide the tmux copy-mode key (backend without copy mode)
  showTmuxCopy?: boolean
  onPaste?: () => void
  // Adds an Attach image key next to Paste (the server takes uploads)
  onAttachImage?: () => void
  onToggleKeyboard?: () => void
  onSendText?: (text: string) => void
  ctrlActive?: boolean
  onCtrlChange?: (active: boolean) => void
  shiftActive?: boolean
  onShiftChange?: (active: boolean) => void
  imeMode?: boolean
  onImeModeChange?: (active: boolean) => void
  defaultExpanded?: boolean
  onHistoryToggle?: () => void
  historyOpen?: boolean
  // The on-screen keyboard is open: the expanded rows close when it opens,
  // so the two together do not cover the whole terminal
  keyboardVisible?: boolean
  // Adds an Actions row (Clear, Cancel, Clear line, Exit) to the expanded keys
  quickActions?: QuickActionHandlers
  // View-only session: no input controls at all
  readOnly?: boolean
  // Customizable 2D keys board layout and editor callback
  keysLayout?: KeysBoardLayout
  onKeysLayoutChange?: (layout: KeysBoardLayout) => void
  defaultBoardOpen?: boolean
  leftHanded?: boolean
}

interface KeyConfig {
  label: ReactNode
  key: string
  ariaLabel?: string
  isCtrlModifier?: boolean
  isShiftModifier?: boolean
  isShiftTab?: boolean
  isScroll?: boolean
  scrollDir?: 'up' | 'down'
  isTmuxCopy?: boolean
  isSelectText?: boolean
  isPaste?: boolean
  isAttach?: boolean
  isKeyboardToggle?: boolean
  isImeToggle?: boolean
  isHistoryToggle?: boolean
}

const ICON_SIZE = 18

// The bottom row: the keys used most, in this order. It scrolls when it does
// not fit; the More key stays pinned at its right end.
const MAIN_KEYS: KeyConfig[] = [
  {
    label: <Keyboard size={ICON_SIZE} />,
    key: 'Keyboard',
    isKeyboardToggle: true,
  },
  {
    label: <Languages size={ICON_SIZE} />,
    key: 'ImeToggle',
    isImeToggle: true,
  },
  { label: 'Esc', key: 'Escape' },
  { label: 'Ctrl', key: 'Control', isCtrlModifier: true },
  { label: <ArrowUp size={ICON_SIZE} />, key: 'ArrowUp' },
  { label: <ArrowDown size={ICON_SIZE} />, key: 'ArrowDown' },
  { label: <CornerDownLeft size={ICON_SIZE} />, key: 'Enter' },
  { label: 'Tab', key: 'Tab' },
]

// Expanded rows, below the Actions row. Text and scroll keys share one row
// (TEXT_KEYS then SCROLL_KEYS), so the rows stay three on a phone.
const TEXT_KEYS: KeyConfig[] = [
  {
    label: <Clock size={ICON_SIZE} />,
    key: 'HistoryToggle',
    isHistoryToggle: true,
  },
  { label: <Clipboard size={ICON_SIZE} />, key: 'TmuxPaste', isPaste: true },
  {
    label: <ImagePlus size={ICON_SIZE} />,
    key: 'Attach',
    ariaLabel: 'Attach image',
    isAttach: true,
  },
  {
    label: <TextSelect size={ICON_SIZE} />,
    key: 'SelectText',
    ariaLabel: 'Select text',
    isSelectText: true,
  },
]

const NAVIGATE_KEYS: KeyConfig[] = [
  { label: 'Shift', key: 'Shift', isShiftModifier: true },
  { label: '⇧Tab', key: 'ShiftTab', ariaLabel: 'Shift+Tab', isShiftTab: true },
  { label: <ArrowLeft size={ICON_SIZE} />, key: 'ArrowLeft' },
  { label: <ArrowRight size={ICON_SIZE} />, key: 'ArrowRight' },
  { label: <ChevronFirst size={ICON_SIZE} />, key: 'Home' },
  { label: <ChevronLast size={ICON_SIZE} />, key: 'End' },
  { label: 'PgUp', key: 'PageUp' },
  { label: 'PgDn', key: 'PageDown' },
  { label: <Delete size={ICON_SIZE} />, key: 'Delete' },
  { label: 'Bksp', key: 'Backspace' },
  { label: 'Ins', key: 'Insert' },
]

const SCROLL_KEYS: KeyConfig[] = [
  { label: <History size={ICON_SIZE} />, key: 'TmuxCopy', isTmuxCopy: true },
  {
    label: <ChevronsUp size={ICON_SIZE} />,
    key: 'ScrollUp',
    isScroll: true,
    scrollDir: 'up',
  },
  {
    label: <ChevronsDown size={ICON_SIZE} />,
    key: 'ScrollDown',
    isScroll: true,
    scrollDir: 'down',
  },
]

const CTRL_COMBOS = [
  { label: 'C', combo: 'c' },
  { label: 'D', combo: 'd' },
  { label: 'Z', combo: 'z' },
  { label: 'L', combo: 'l' },
  { label: 'A', combo: 'a' },
  { label: 'E', combo: 'e' },
  { label: 'B', combo: 'b' },
  { label: 'X', combo: 'x' },
  { label: 'K', combo: 'k' },
  { label: 'U', combo: 'u' },
  { label: 'W', combo: 'w' },
  { label: 'R', combo: 'r' },
  { label: 'P', combo: 'p' },
  { label: 'N', combo: 'n' },
]

const CTRL_SHIFT_COMBOS = [
  { label: 'C', combo: 'c' },
  { label: 'V', combo: 'v' },
  { label: 'Z', combo: 'z' },
  { label: 'X', combo: 'x' },
]

// Keycap: the one look every toolbar key shares. Neutral is the base; the
// terminal and native styles override a few properties. A light native key is
// white on a white toolbar, so it carries an inset hairline as well: inset,
// because the key rows scroll and a scroller clips anything drawn outside.
const KEYCAP =
  'flex h-11 min-w-11 shrink-0 items-center justify-center rounded-control border border-border bg-surface-raised px-3 text-[13px] font-medium text-fg touch-manipulation select-none transition duration-(--duration-fast) ease-standard hover:border-border-strong active:bg-surface disabled:opacity-50 ' +
  'ui-terminal:bg-bg ui-terminal:font-label ui-terminal:text-[12px] ' +
  'ui-native:border-0 ui-native:text-[15px] ui-native:shadow-[inset_0_0_0_1px_rgb(0_0_0/0.1),0_1px_0_rgb(0_0_0/0.18)] dark:ui-native:shadow-[0_1px_0_rgb(0_0_0/0.8)] active:ui-native:scale-95 ' +
  FOCUS_RING
// Pressed state (Ctrl/Shift/History on)
const KEYCAP_ON = 'bg-accent! text-accent-fg! border-accent!'
// Ctrl+ combos while Ctrl is on
const KEYCAP_COMBO =
  'bg-accent-soft! text-accent! border-transparent! px-2 font-label'

interface KeycapProps extends ComponentProps<'button'> {
  // Set for keys that toggle (exposed as aria-pressed)
  pressed?: boolean
  // The keyboard toggle must let the touch through so the OS keyboard opens
  allowFocus?: boolean
}

// Every key stops the default of mousedown/touchstart so a tap does not move
// focus away from the terminal.
function Keycap({
  pressed,
  allowFocus = false,
  className = '',
  ...props
}: KeycapProps) {
  return (
    <button
      type="button"
      aria-pressed={pressed}
      onMouseDown={(e) => !allowFocus && e.preventDefault()}
      onTouchStart={(e) =>
        /* v8 ignore next */
        !allowFocus && e.preventDefault()
      }
      onContextMenu={(e) => e.preventDefault()}
      className={`${KEYCAP} ${pressed ? KEYCAP_ON : ''} ${className}`}
      {...props}
    />
  )
}

// A labelled group of keys in the expanded toolbar
function KeyGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <fieldset aria-label={label} className="m-0 min-w-0 border-0 p-0">
      <div
        aria-hidden="true"
        className="px-1 pb-1 pt-2 text-[10px] uppercase tracking-wider text-fg-subtle font-label"
      >
        {label}
      </div>
      <div className="flex items-center gap-2 overflow-x-auto pb-1">
        {children}
      </div>
    </fieldset>
  )
}

const ROOT_CLASS =
  'border-t border-border bg-surface pb-safe ui-terminal:bg-bg ui-native:border-t-0'

export function KeyboardToolbar({
  onKey,
  onCtrlKey,
  onShiftKey,
  onCtrlShiftKey,
  onScroll,
  onTmuxCopy,
  onSelectText,
  showTmuxCopy = true,
  onPaste,
  onAttachImage,
  onToggleKeyboard,
  onSendText,
  ctrlActive: externalCtrlActive,
  onCtrlChange,
  shiftActive: externalShiftActive,
  onShiftChange,
  imeMode: externalImeMode,
  onImeModeChange,
  defaultExpanded = false,
  onHistoryToggle,
  historyOpen,
  keyboardVisible = false,
  quickActions,
  readOnly = false,
  keysLayout,
  onKeysLayoutChange,
  defaultBoardOpen = false,
  leftHanded = false,
}: Props) {
  const [internalCtrlActive, setInternalCtrlActive] = useState(false)
  const [internalShiftActive, setInternalShiftActive] = useState(false)
  const [expanded, setExpanded] = useState(defaultExpanded)
  const [boardOpen, setBoardOpen] = useState(defaultBoardOpen)
  const [editorOpen, setEditorOpen] = useState(false)
  const [localLayout, setLocalLayout] = useState<KeysBoardLayout>(
    keysLayout ?? DEFAULT_KEYS_LAYOUT,
  )
  const activeLayout = keysLayout ?? localLayout

  const [stickyMods, setStickyMods] = useState<StickyModifiersState>(
    INITIAL_STICKY_MODIFIERS,
  )
  const lastModTapRef = useRef<{ mod: string; at: number } | null>(null)
  const [armedKeyId, setArmedKeyId] = useState<string | null>(null)
  const armedTimerRef = useRef<number | null>(null)
  const stepTimersRef = useRef<number[]>([])

  const [internalImeMode, setInternalImeMode] = useState(false)
  const imeMode = externalImeMode ?? internalImeMode
  const [imeText, setImeText] = useState('')
  const imeInputRef = useRef<HTMLInputElement>(null)
  const imeFocusTimeoutRef = useRef<number | null>(null)
  const ctrlActive = externalCtrlActive ?? internalCtrlActive
  const shiftActive = externalShiftActive ?? internalShiftActive
  const { trigger: haptic } = useHaptic()
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null)

  // Cleanup timeout on unmount
  useEffect(() => {
    return () => {
      if (imeFocusTimeoutRef.current) clearTimeout(imeFocusTimeoutRef.current)
      if (armedTimerRef.current) clearTimeout(armedTimerRef.current)
      for (const t of stepTimersRef.current) clearTimeout(t)
    }
  }, [])

  const updateLayout = useCallback(
    (next: KeysBoardLayout) => {
      setLocalLayout(next)
      onKeysLayoutChange?.(next)
    },
    [onKeysLayoutChange],
  )

  const handleBoardCellClick = useCallback(
    (cell: KeyCell) => {
      haptic('light')

      // Sticky modifier key ('Ctrl' | 'Alt' | 'Shift')
      if (cell.modifier) {
        const now = Date.now()
        const prevTap = lastModTapRef.current
        const isDoubleTap =
          prevTap !== null &&
          prevTap.mod === cell.modifier &&
          now - prevTap.at <= 320
        lastModTapRef.current = isDoubleTap
          ? null
          : { mod: cell.modifier, at: now }
        setStickyMods((prev) =>
          nextStickyModifierState(prev, cell.modifier!, isDoubleTap),
        )
        return
      }

      // Two-tap danger confirmation (2.5s window)
      if (cell.danger && armedKeyId !== cell.id) {
        if (armedTimerRef.current) clearTimeout(armedTimerRef.current)
        setArmedKeyId(cell.id)
        armedTimerRef.current = window.setTimeout(() => {
          setArmedKeyId(null)
          armedTimerRef.current = null
        }, DANGER_CONFIRM_MS)
        return
      }

      if (armedKeyId === cell.id) {
        if (armedTimerRef.current) {
          clearTimeout(armedTimerRef.current)
          armedTimerRef.current = null
        }
        setArmedKeyId(null)
      }

      const modsSnapshot = stickyMods
      setStickyMods((prev) => consumeLatchedModifiers(prev))

      const dispatchOne = (token: string) => {
        if (token === 'Shift+Tab' && onShiftKey) {
          onShiftKey('Tab')
          return
        }
        if (token === 'Tab' && modsSnapshot.Shift !== 'off' && onShiftKey) {
          onShiftKey('Tab')
          return
        }
        dispatchStepToken(token, modsSnapshot, {
          onKeyPress: onKey,
          onCtrlKey,
          onCtrlShiftKey,
          onSendText,
        })
      }

      const [first, ...rest] = cell.steps
      if (first) dispatchOne(first)
      rest.forEach((step, idx) => {
        const timer = window.setTimeout(
          () => dispatchOne(step),
          STEP_DELAY_MS * (idx + 1),
        )
        stepTimersRef.current.push(timer)
      })
    },
    [
      haptic,
      armedKeyId,
      stickyMods,
      onKey,
      onCtrlKey,
      onShiftKey,
      onCtrlShiftKey,
      onSendText,
    ],
  )

  // Keys filtered by the handlers and capabilities available
  // The main keys apart: their identity tells useScrollEdges the scroller's
  // children changed, so it must not follow the other handlers (App passes
  // some inline)
  const mainKeys = useMemo(
    () => MAIN_KEYS.filter((k) => !!onSendText || !k.isImeToggle),
    [onSendText],
  )
  const { toolKeys, navigateKeys } = useMemo(
    () => ({
      toolKeys: [...TEXT_KEYS, ...SCROLL_KEYS].filter(
        (k) =>
          (!!onHistoryToggle || !k.isHistoryToggle) &&
          (!!onAttachImage || !k.isAttach) &&
          (!!onSelectText || !k.isSelectText) &&
          (showTmuxCopy || !k.isTmuxCopy),
      ),
      navigateKeys: NAVIGATE_KEYS.filter((k) => !!onShiftKey || !k.isShiftTab),
    }),
    [onHistoryToggle, onAttachImage, onSelectText, onShiftKey, showTmuxCopy],
  )
  const edges = useScrollEdges(scroller, mainKeys)
  const mask = scrollEdgeMask(edges)

  const setCtrlActive = useCallback(
    (value: boolean | ((prev: boolean) => boolean)) => {
      const newValue = typeof value === 'function' ? value(ctrlActive) : value
      setInternalCtrlActive(newValue)
      onCtrlChange?.(newValue)
    },
    [ctrlActive, onCtrlChange],
  )

  const setShiftActive = useCallback(
    (value: boolean | ((prev: boolean) => boolean)) => {
      const newValue = typeof value === 'function' ? value(shiftActive) : value
      setInternalShiftActive(newValue)
      onShiftChange?.(newValue)
    },
    [shiftActive, onShiftChange],
  )

  const setImeMode = useCallback(
    (value: boolean) => {
      setInternalImeMode(value)
      onImeModeChange?.(value)
    },
    [onImeModeChange],
  )

  const toggleImeMode = useCallback(() => {
    haptic('medium')
    const next = !imeMode
    setImeMode(next)
    if (next) {
      imeFocusTimeoutRef.current = window.setTimeout(
        () => imeInputRef.current?.focus(),
        50,
      )
    }
    setImeText('')
  }, [haptic, imeMode, setImeMode])

  const handleImeSend = useCallback(() => {
    if (imeText.trim() && onSendText) {
      haptic('light')
      onSendText(imeText)
      setImeText('')
      imeInputRef.current?.focus()
    }
  }, [imeText, onSendText, haptic])

  const handleImeKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === 'Enter' && !e.nativeEvent.isComposing) {
        e.preventDefault()
        handleImeSend()
      }
    },
    [handleImeSend],
  )

  // Shift lives in the expanded rows: closing them turns it off, so no
  // modifier stays on with nothing on screen showing it
  const collapse = useCallback(() => {
    if (shiftActive) setShiftActive(false)
    setExpanded(false)
  }, [shiftActive, setShiftActive])

  const toggleExpanded = useCallback(() => {
    haptic('medium')
    if (expanded) collapse()
    else setExpanded(true)
  }, [haptic, expanded, collapse])

  // Close the expanded rows when the on-screen keyboard opens (only then:
  // opening them again while it is up stays possible)
  const keyboardWasVisible = useRef(keyboardVisible)
  useEffect(() => {
    if (keyboardVisible && !keyboardWasVisible.current && expanded) collapse()
    keyboardWasVisible.current = keyboardVisible
  }, [keyboardVisible, expanded, collapse])

  const handleKey = useCallback(
    (
      key: string,
      opts?: {
        isCtrlModifier?: boolean
        isShiftModifier?: boolean
        isShiftTab?: boolean
        scrollDir?: 'up' | 'down'
        isTmuxCopy?: boolean
        isSelectText?: boolean
        isPaste?: boolean
        isAttach?: boolean
        isKeyboardToggle?: boolean
        isImeToggle?: boolean
        isHistoryToggle?: boolean
      },
    ) => {
      haptic('light')
      if (opts?.isImeToggle) {
        toggleImeMode()
        return
      }
      // The history list opens above the toolbar: the expanded rows close
      // so the two do not stack over the whole terminal
      if (opts?.isHistoryToggle && onHistoryToggle) {
        onHistoryToggle()
        collapse()
        return
      }
      if (opts?.isKeyboardToggle && onToggleKeyboard) {
        onToggleKeyboard()
        return
      }
      if (opts?.isTmuxCopy && onTmuxCopy) {
        onTmuxCopy()
        return
      }
      if (opts?.isSelectText && onSelectText) {
        onSelectText()
        return
      }
      if (opts?.isPaste && onPaste) {
        onPaste()
        return
      }
      if (opts?.isAttach && onAttachImage) {
        onAttachImage()
        return
      }
      if (opts?.scrollDir && onScroll) {
        onScroll(opts.scrollDir)
        return
      }
      if (opts?.isCtrlModifier) {
        setCtrlActive((prev) => !prev)
        return
      }
      if (opts?.isShiftModifier) {
        setShiftActive((prev) => !prev)
        return
      }
      // Always Shift+Tab, whatever modifier is on; the modifiers then clear
      if (opts?.isShiftTab) {
        onShiftKey?.('Tab')
        if (ctrlActive) setCtrlActive(false)
        if (shiftActive) setShiftActive(false)
        return
      }
      // Escape clears active modifiers instead of sending Escape key
      if (key === 'Escape' && (ctrlActive || shiftActive)) {
        setCtrlActive(false)
        setShiftActive(false)
        return
      }
      // Handle key with modifiers
      // Only lowercase single letters, preserve special key names (Tab, ArrowUp, etc.)
      const keyToSend = /^[a-z]$/i.test(key) ? key.toLowerCase() : key
      if (ctrlActive && shiftActive) {
        onCtrlShiftKey?.(keyToSend)
        setCtrlActive(false)
        setShiftActive(false)
      } else if (ctrlActive) {
        onCtrlKey(keyToSend)
        setCtrlActive(false)
      } else if (shiftActive) {
        onShiftKey?.(keyToSend)
        setShiftActive(false)
      } else {
        onKey(key)
      }
    },
    [
      ctrlActive,
      shiftActive,
      onKey,
      onCtrlKey,
      onShiftKey,
      onCtrlShiftKey,
      onScroll,
      onTmuxCopy,
      onSelectText,
      onPaste,
      onAttachImage,
      onToggleKeyboard,
      onHistoryToggle,
      collapse,
      toggleImeMode,
      haptic,
      setShiftActive,
      setCtrlActive,
    ],
  )

  if (readOnly) return null

  // IME input mode - full width text input for Vietnamese/CJK
  if (imeMode) {
    return (
      <div
        className={`flex items-center gap-2 px-3 py-2 ${ROOT_CLASS}`}
        style={{
          paddingBottom: 'max(env(safe-area-inset-bottom, 0px), 0.5rem)',
        }}
      >
        <button
          onClick={toggleImeMode}
          // `!`: the colour must beat KEYCAP's text-fg, whatever the CSS order
          className={`${KEYCAP} text-danger!`}
          aria-label="Close IME input"
        >
          <X size={ICON_SIZE} />
        </button>
        <input
          ref={imeInputRef}
          type="text"
          value={imeText}
          onChange={(e) => setImeText(e.target.value)}
          onKeyDown={handleImeKeyDown}
          placeholder="Type non-Latin text here... (Vietnamese, CJK, ...)"
          className={`h-11 flex-1 rounded-control border border-border bg-surface-raised px-4 text-fg placeholder:text-fg-subtle ui-native:border-0 ${FOCUS_RING}`}
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          spellCheck={false}
        />
        <button
          onClick={handleImeSend}
          disabled={!imeText.trim()}
          className={`${KEYCAP} ${KEYCAP_ON}`}
          aria-label="Send text"
        >
          <Send size={ICON_SIZE} />
        </button>
      </div>
    )
  }

  const renderKey = (keyConfig: KeyConfig) => (
    <Keycap
      key={keyConfig.key}
      data-key={keyConfig.key}
      allowFocus={keyConfig.isKeyboardToggle}
      pressed={
        keyConfig.isCtrlModifier
          ? ctrlActive
          : keyConfig.isShiftModifier
            ? shiftActive
            : keyConfig.isHistoryToggle
              ? !!historyOpen
              : undefined
      }
      onClick={() =>
        handleKey(keyConfig.key, {
          isCtrlModifier: keyConfig.isCtrlModifier,
          isShiftModifier: keyConfig.isShiftModifier,
          isShiftTab: keyConfig.isShiftTab,
          scrollDir: keyConfig.scrollDir,
          isTmuxCopy: keyConfig.isTmuxCopy,
          isSelectText: keyConfig.isSelectText,
          isPaste: keyConfig.isPaste,
          isAttach: keyConfig.isAttach,
          isKeyboardToggle: keyConfig.isKeyboardToggle,
          isImeToggle: keyConfig.isImeToggle,
          isHistoryToggle: keyConfig.isHistoryToggle,
        })
      }
      aria-label={keyConfig.ariaLabel}
    >
      {keyConfig.label}
    </Keycap>
  )

  // Combos float above the toolbar instead of taking a row or a place in the
  // scroller: a row that comes and goes with Ctrl would resize the terminal
  // (and make the running TUI redraw) on every Ctrl press, and the end of the
  // scroller is usually off screen.
  const renderCombos = (
    label: string,
    testId: string,
    combos: { label: string; combo: string }[],
    prefix: string,
  ) => (
    <div
      data-testid={testId}
      className="absolute inset-x-0 bottom-full z-10 border-t border-border bg-surface px-3 shadow-lg ui-terminal:bg-bg"
    >
      <KeyGroup label={label}>
        {combos.map(({ label, combo }) => (
          <Keycap
            key={combo}
            onClick={() => handleKey(combo)}
            className={KEYCAP_COMBO}
          >
            {prefix}
            {label}
          </Keycap>
        ))}
      </KeyGroup>
    </div>
  )

  return (
    <div
      data-left-handed={leftHanded ? 'true' : undefined}
      className={`relative flex flex-col px-3 py-2 ${ROOT_CLASS}`}
      style={{ paddingBottom: 'max(env(safe-area-inset-bottom, 0px), 0.5rem)' }}
    >
      {boardOpen && (
        <fieldset
          aria-label="Keys board"
          data-testid="keys-board-panel"
          className="m-0 mb-2 flex min-w-0 flex-col gap-1.5 border-0 border-b border-border p-0 pb-2"
        >
          <div className="flex items-center justify-between gap-1.5">
            <div className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
              {(
                [
                  ['default', 'Default'],
                  ['claude-code', 'Claude Code'],
                  ['prefix', 'Prefix'],
                  ['vim', 'Vim'],
                  ['navigation', 'Navigation'],
                ] as [Exclude<KeysPresetId, 'custom'>, string][]
              ).map(([id, label]) => (
                <button
                  key={id}
                  type="button"
                  data-preset={id}
                  aria-pressed={activeLayout.preset === id}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => updateLayout(BUILTIN_KEYS_PRESETS[id])}
                  className={`h-6 shrink-0 select-none rounded-control border px-2 text-[11px] font-medium transition-colors ${FOCUS_RING} ${
                    activeLayout.preset === id
                      ? 'border-accent bg-accent-soft text-accent-fg'
                      : 'border-border bg-bg text-fg-muted hover:text-fg'
                  }`}
                >
                  {label}
                </button>
              ))}
            </div>
            <button
              type="button"
              data-testid="open-keys-editor"
              aria-label="Customize keys"
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => setEditorOpen(true)}
              className={`flex h-6 shrink-0 select-none items-center gap-1 rounded-control border border-border bg-bg px-2 text-[11px] text-fg-muted hover:text-fg ${FOCUS_RING}`}
            >
              <SlidersHorizontal size={12} aria-hidden="true" />
              Edit
            </button>
          </div>

          <div
            data-testid="keys-board-grid"
            className="grid grid-cols-7 gap-1"
            style={{ gridTemplateColumns: 'repeat(7, minmax(0, 1fr))' }}
          >
            {activeLayout.keys.map((cell) => {
              const modState = cell.modifier
                ? stickyMods[cell.modifier]
                : undefined
              const isArmed = armedKeyId === cell.id
              return (
                <button
                  key={cell.id}
                  type="button"
                  data-key-id={cell.id}
                  data-col-span={cell.colSpan}
                  data-row-span={cell.rowSpan}
                  data-danger={cell.danger ? 'true' : undefined}
                  data-armed={isArmed ? 'true' : undefined}
                  data-modifier-state={modState}
                  aria-pressed={
                    modState ? modState !== 'off' : isArmed ? true : undefined
                  }
                  onMouseDown={(e) => e.preventDefault()}
                  onContextMenu={(e) => e.preventDefault()}
                  onClick={() => handleBoardCellClick(cell)}
                  style={{
                    gridColumn: `span ${cell.colSpan} / span ${cell.colSpan}`,
                    gridRow: `span ${cell.rowSpan} / span ${cell.rowSpan}`,
                  }}
                  className={`flex min-h-9 select-none items-center justify-center rounded-control border px-1 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                    isArmed
                      ? 'border-danger bg-danger text-white'
                      : modState === 'locked'
                        ? 'border-accent bg-accent text-white ring-2 ring-accent/40'
                        : modState === 'latched'
                          ? 'border-accent bg-accent-soft text-accent-fg'
                          : cell.danger
                            ? 'border-danger/40 bg-bg text-danger hover:bg-danger/10'
                            : 'border-border bg-bg text-fg hover:bg-surface-hover'
                  }`}
                >
                  <span className="truncate">
                    {isArmed
                      ? 'Tap again'
                      : modState === 'locked'
                        ? `${cell.label}●`
                        : cell.label}
                  </span>
                </button>
              )
            })}
          </div>
        </fieldset>
      )}

      {expanded && (
        <>
          {quickActions && (
            <KeyGroup label="Actions">
              {QUICK_ACTIONS.map((action) => (
                <Keycap
                  key={action.label}
                  data-key={`Action-${action.label}`}
                  onClick={() => {
                    haptic('light')
                    runAction(action, quickActions)
                  }}
                  className="gap-1.5"
                >
                  <span aria-hidden="true" className="text-fg-muted">
                    {action.icon}
                  </span>
                  {action.label}
                </Keycap>
              ))}
            </KeyGroup>
          )}
          <KeyGroup label="Text · Scroll">{toolKeys.map(renderKey)}</KeyGroup>
          <KeyGroup label="Navigate">{navigateKeys.map(renderKey)}</KeyGroup>
        </>
      )}

      {ctrlActive &&
        !shiftActive &&
        renderCombos('Ctrl +', 'ctrl-combos-overlay', CTRL_COMBOS, '^')}
      {ctrlActive &&
        shiftActive &&
        renderCombos(
          'Ctrl + Shift +',
          'ctrl-shift-combos-overlay',
          CTRL_SHIFT_COMBOS,
          '^⇧',
        )}

      <div
        className={`flex items-center gap-2 ${leftHanded ? 'flex-row-reverse' : ''}`}
      >
        {/* A horizontal scroller clips vertically too: the native keycap's 1px
            bottom shadow needs room below it. Only there, so the other styles
            keep their height (the terminal would resize). The mask fades an
            end that still hides keys. */}
        <div
          ref={setScroller}
          data-testid="toolbar-scroller"
          className="flex min-w-0 flex-1 items-center gap-2 overflow-x-auto ui-native:pb-0.5"
          style={{ maskImage: mask, WebkitMaskImage: mask }}
        >
          {mainKeys.map(renderKey)}
        </div>
        {/* Pinned outside the scroller, so it is always on screen */}
        <div
          className={`flex shrink-0 items-center gap-1 ui-native:pb-0.5 ${
            leftHanded
              ? 'border-r border-border pr-2'
              : 'border-l border-border pl-2'
          }`}
        >
          <Keycap
            data-key="KeysBoard"
            aria-label="Keys board"
            aria-expanded={boardOpen}
            pressed={boardOpen}
            onClick={() => {
              haptic('medium')
              setBoardOpen((prev) => !prev)
            }}
          >
            <Grid2x2 size={ICON_SIZE} />
          </Keycap>
          <Keycap
            data-key="More"
            aria-label="Extra keys"
            aria-expanded={expanded}
            onClick={toggleExpanded}
          >
            <Ellipsis size={ICON_SIZE} />
          </Keycap>
        </div>
      </div>

      <KeysBoardEditor
        isOpen={editorOpen}
        onClose={() => setEditorOpen(false)}
        layout={activeLayout}
        onSave={updateLayout}
      />
    </div>
  )
}
