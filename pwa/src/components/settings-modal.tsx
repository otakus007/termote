import { Trash2, X } from 'lucide-react'
import { type ReactNode, useEffect, useId, useState } from 'react'
import {
  deleteTelegramConfig,
  fetchNotifyConfig,
  type NotifyCategories,
  type NotifyConfig,
  setNotifyMute,
  testTelegramNotification,
  updateNotifyConfig,
  updateTelegramConfig,
} from '../hooks/use-mux-api'
import {
  FIND_EXCLUDES_DEFAULT,
  FIND_EXCLUDES_MAX,
  findExcludeProblem,
  type ImeSendBehavior,
  type PasteSource,
  QUICK_REPLIES_DEFAULT,
  QUICK_REPLIES_MAX,
  type Settings,
} from '../hooks/use-settings'
import { UI_STYLES, type UiStyle } from '../ui-style'
import { STUCK_MINUTES_OPTIONS } from '../utils/agent-stuck'
import type { ServerInfo } from '../utils/app-update'
import { DEFAULT_KEYS_LAYOUT } from '../utils/keys-layout'
import {
  needsHomeScreenApp,
  notificationSupport,
  notifyWorkerReady,
  requestNotify,
} from '../utils/notify-permission'
import { DevicesSection } from './devices-section'
import { KeysBoardEditor } from './keys-board-editor'
import { SigninsSection } from './signins-section'
import { Button, FOCUS_RING, IconButton } from './ui/button'
import { SegmentedControl } from './ui/segmented-control'
import { Sheet } from './ui/sheet'
import { Switch } from './ui/switch'
import { UpdatesSection } from './updates-section'

interface Props {
  isOpen: boolean
  onClose: () => void
  settings: Settings
  onUpdateSetting: <K extends keyof Settings>(
    key: K,
    value: Settings[K],
  ) => void
  onShowGestureHints?: () => void
  // The Updates group: what the server runs, and the reload to its page
  updates?: {
    server: ServerInfo | null
    stale: boolean
    reloading: boolean
    onReload: () => void
  }
  onClearHistory?: () => void
  historyCount?: number
  // Backend has a paste buffer (caps.copyMode); false hides the paste source choice
  tmuxBufferSupported?: boolean
  // Name of the backend's paste buffer in the option label (e.g. "tmux buffer");
  // the backend name is not known here, so the default stays generic.
  pasteBufferLabel?: string
  // Backend lets this device take over the pane size (caps.driveSize)
  driveSizeSupported?: boolean
  // The server sends Web Push (caps.push); turning notifications on then
  // subscribes this device, turning them off unsubscribes it.
  pushAvailable?: boolean
  onEnableNotify?: () => Promise<unknown>
  onDisableNotify?: () => void
  // View-only role: nothing here may change the server (no notifications,
  // which would subscribe this device to Web Push)
  readOnly?: boolean
  // The Devices group: pair, list and revoke devices (caps.devices, full)
  devices?: boolean
  // The Signed-in browsers group: list and sign out the browsers signed in
  // with the password (caps.signins, full)
  signins?: boolean
  // Log out, for Signed-in browsers to sign this browser out with
  onLogout?: () => Promise<void> | void
  // Group to show when the sheet opens (About's "Check for updates")
  initialGroup?: string
}

const CONTROL =
  'h-9 rounded-control border border-border bg-bg text-[13px] text-fg pointer-coarse:h-touch'

// One line of a settings group: the title (and hint) on the left, the control
// on the right. `htmlFor` makes the title the label of a text field.
function SettingsRow({
  title,
  desc,
  htmlFor,
  children,
}: {
  title: string
  desc?: string
  htmlFor?: string
  children: ReactNode
}) {
  const titleClass =
    'block text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]'
  return (
    <div className="flex min-h-14 flex-wrap items-center gap-x-3 gap-y-2 px-4 py-2.5 ui-native:bg-surface-raised">
      <div className="min-w-0 flex-1 basis-40">
        {htmlFor ? (
          <label htmlFor={htmlFor} className={titleClass}>
            {title}
          </label>
        ) : (
          <p className={`m-0 ${titleClass}`}>{title}</p>
        )}
        {desc && <p className="m-0 text-[12px] text-fg-muted">{desc}</p>}
      </div>
      {children}
    </div>
  )
}

// A titled block of rows. On desktop only the group picked in the left rail
// shows; on a phone every group stacks in one scrolling sheet.
function SettingsGroup({
  id,
  title,
  active,
  children,
}: {
  id: string
  title: string
  active: boolean
  children: ReactNode
}) {
  const headingId = useId()
  return (
    <section
      data-group={id}
      aria-labelledby={headingId}
      className={`pb-3 ${active ? '' : 'md:hidden'}`}
    >
      <h3
        id={headingId}
        className="m-0 px-4 pb-1.5 pt-3 font-label text-[11px] font-semibold uppercase tracking-wider text-fg-subtle"
      >
        {title}
      </h3>
      <div className="divide-y divide-border border-y border-border ui-native:mx-4 ui-native:overflow-hidden ui-native:rounded-panel ui-native:border-0 ui-neutral:mx-4 ui-neutral:rounded-panel ui-neutral:border">
        {children}
      </div>
    </section>
  )
}

// Terminal font name; saved on blur or Enter, so the terminal is not
// re-measured (and the pane resized) on every keystroke.
function TerminalFontRow({
  value,
  onSave,
}: {
  value: string
  onSave: (value: string) => void
}) {
  const [draft, setDraft] = useState(value)
  useEffect(() => setDraft(value), [value])
  const save = () => {
    const next = draft.trim()
    if (next !== value) onSave(next)
  }
  return (
    <SettingsRow
      title="Terminal font"
      desc="A font installed on this device (e.g. a Nerd Font). Nerd Font icons work without one."
      htmlFor="terminal-font"
    >
      <input
        id="terminal-font"
        type="text"
        value={draft}
        placeholder="JetBrainsMono Nerd Font"
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        onChange={(e) => setDraft(e.target.value)}
        onBlur={save}
        onKeyDown={(e) => e.key === 'Enter' && save()}
        className={`w-36 px-2 ui-terminal:font-label ${CONTROL} ${FOCUS_RING}`}
      />
    </SettingsRow>
  )
}

// The folder names a file search never enters: each removable, one added at
// a time (refused with the reason), and the defaults back in one press.
function ExcludedFoldersRow({
  value,
  onSave,
}: {
  value: string[]
  onSave: (value: string[]) => void
}) {
  const [name, setName] = useState('')
  const [problem, setProblem] = useState<string>()
  const inputId = useId()
  const errorId = useId()
  const add = () => {
    const next = name.trim()
    const why =
      findExcludeProblem(next) ??
      (value.includes(next) ? 'Already in the list' : undefined) ??
      (value.length >= FIND_EXCLUDES_MAX
        ? `At most ${FIND_EXCLUDES_MAX} folders`
        : undefined)
    if (why) {
      setProblem(why)
      return
    }
    onSave([...value, next])
    setName('')
  }
  const isDefault =
    value.length === FIND_EXCLUDES_DEFAULT.length &&
    value.every((v, i) => v === FIND_EXCLUDES_DEFAULT[i])
  return (
    <div className="flex flex-col gap-2 px-4 py-2.5 ui-native:bg-surface-raised">
      <div>
        <label
          htmlFor={inputId}
          className="block text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]"
        >
          Excluded folders
        </label>
        <p className="m-0 text-[12px] text-fg-muted">
          Never searched for ignored files, and never outside a repository.
          Tracked files are always searched.
        </p>
      </div>
      <ul
        aria-label="Folders never searched"
        className="flex flex-wrap gap-1.5"
      >
        {value.map((v) => (
          <li
            key={v}
            className="flex items-center gap-0.5 rounded-control border border-border bg-bg pl-2 font-term text-[12px] text-fg"
          >
            {v}
            <IconButton
              size="sm"
              variant="ghost"
              aria-label={`Remove ${v}`}
              onClick={() => onSave(value.filter((x) => x !== v))}
            >
              <X size={12} aria-hidden="true" />
            </IconButton>
          </li>
        ))}
      </ul>
      <div className="flex flex-wrap items-center gap-2">
        <input
          id={inputId}
          type="text"
          value={name}
          placeholder="Folder name"
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          aria-invalid={problem ? true : undefined}
          aria-describedby={problem ? errorId : undefined}
          onChange={(e) => {
            setName(e.target.value)
            setProblem(undefined)
          }}
          onKeyDown={(e) => {
            if (e.key !== 'Enter') return
            e.preventDefault()
            add()
          }}
          className={`w-40 px-2 font-term ${CONTROL} ${FOCUS_RING}`}
        />
        <Button size="sm" onClick={add}>
          Add
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={isDefault}
          onClick={() => {
            onSave(FIND_EXCLUDES_DEFAULT)
            setProblem(undefined)
          }}
        >
          Reset to defaults
        </Button>
      </div>
      {problem && (
        <p id={errorId} role="alert" className="m-0 text-[12px] text-danger">
          {problem}
        </p>
      )}
    </div>
  )
}

function QuickRepliesSettingsRow({
  value,
  onSave,
}: {
  value: string[]
  onSave: (value: string[]) => void
}) {
  const [draft, setDraft] = useState('')
  const inputId = useId()
  const addReply = () => {
    const trimmed = draft.trim()
    if (
      !trimmed ||
      value.includes(trimmed) ||
      value.length >= QUICK_REPLIES_MAX
    ) {
      return
    }
    onSave([...value, trimmed])
    setDraft('')
  }
  const isDefault =
    value.length === QUICK_REPLIES_DEFAULT.length &&
    value.every((v, i) => v === QUICK_REPLIES_DEFAULT[i])
  return (
    <div className="flex flex-col gap-2 px-4 py-2.5 ui-native:bg-surface-raised">
      <div>
        <label
          htmlFor={inputId}
          className="block text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]"
        >
          Quick replies
        </label>
        <p className="m-0 text-[12px] text-fg-muted">
          One-tap reply chips shown above the chat composer and terminal
        </p>
      </div>
      <ul aria-label="Quick reply chips" className="flex flex-wrap gap-1.5">
        {value.map((chip) => (
          <li
            key={chip}
            className="flex items-center gap-0.5 rounded-control border border-border bg-bg pl-2 text-[12px] text-fg"
          >
            {chip}
            <IconButton
              size="sm"
              variant="ghost"
              aria-label={`Remove quick reply ${chip}`}
              onClick={() => onSave(value.filter((x) => x !== chip))}
            >
              <X size={12} aria-hidden="true" />
            </IconButton>
          </li>
        ))}
      </ul>
      <div className="flex flex-wrap items-center gap-2">
        <input
          id={inputId}
          type="text"
          value={draft}
          placeholder="New quick reply…"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== 'Enter') return
            e.preventDefault()
            addReply()
          }}
          className={`w-44 px-2 ${CONTROL} ${FOCUS_RING}`}
        />
        <Button size="sm" onClick={addReply}>
          Add reply
        </Button>
        <Button
          size="sm"
          variant="ghost"
          disabled={isDefault}
          onClick={() => onSave(QUICK_REPLIES_DEFAULT)}
        >
          Reset quick replies
        </Button>
      </div>
    </div>
  )
}

const formatSeconds = (s: number) => (s >= 60 ? `${s / 60}m` : `${s}s`)

const POLL_INTERVAL_OPTIONS = [3, 5, 10, 15, 30, 60, 120, 300]

const IME_SEND_OPTIONS: {
  value: ImeSendBehavior
  label: string
  desc: string
}[] = [
  {
    value: 'send-only',
    label: 'Send text only',
    desc: 'Send text to terminal without Enter',
  },
  {
    value: 'send-enter',
    label: 'Send + Enter',
    desc: 'Send text then press Enter automatically',
  },
]

function pasteSourceOptions(bufferLabel: string): {
  value: PasteSource
  label: string
  desc: string
}[] {
  return [
    {
      value: 'clipboard',
      label: 'System clipboard',
      desc: 'Paste from device clipboard (Ctrl+Shift+V)',
    },
    {
      // The stored value stays 'tmux' (the localStorage schema is unchanged)
      value: 'tmux',
      label: bufferLabel,
      desc: 'Paste from the copy mode buffer',
    },
  ]
}

// Notify when an agent needs me. Turning it on asks for the permission first
// thing in the click (Safari asks only from a gesture), then subscribes this
// device to Web Push in the same click when the server sends it; it stays
// off while the browser blocks notifications or the active service worker
// predates the click handler.
function NotifyAgentsRow({
  enabled,
  onChange,
  pushAvailable,
  onEnable,
  onDisable,
}: {
  enabled: boolean
  onChange: (on: boolean) => void
  pushAvailable: boolean
  onEnable?: () => Promise<unknown>
  onDisable?: () => void
}) {
  const [permission, setPermission] = useState(notificationSupport)
  // undefined while the worker is asked
  const [workerReady, setWorkerReady] = useState<boolean>()
  const [homeScreenOnly] = useState(needsHomeScreenApp)
  useEffect(() => {
    let live = true
    notifyWorkerReady().then((ready) => {
      if (live) setWorkerReady(ready)
    })
    return () => {
      live = false
    }
  }, [])
  if (permission === 'unsupported' && !homeScreenOnly) return null
  const checked = enabled && permission === 'granted'
  const desc = homeScreenOnly
    ? 'iPhone/iPad: works only from the Home Screen app (iOS 16.4+)'
    : permission === 'denied'
      ? 'Blocked in browser settings'
      : workerReady === false
        ? 'Reload to turn on'
        : 'A dialog waits or a turn finished. On a phone, needs the installed app'
  const toggle = async (on: boolean) => {
    if (!on) {
      onChange(false)
      onDisable?.()
      return
    }
    const result = await requestNotify()
    setPermission(result)
    if (result !== 'granted') return
    // Nothing awaited between the permission and the subscribe.
    const subscribed = pushAvailable ? onEnable?.() : undefined
    onChange(true)
    await subscribed
  }
  return (
    <SettingsRow title="Notify when an agent needs me" desc={desc}>
      <Switch
        label="Notify when an agent needs me"
        checked={checked}
        disabled={
          !checked &&
          (homeScreenOnly || permission === 'denied' || !workerReady)
        }
        onChange={toggle}
      />
    </SettingsRow>
  )
}

const DEFAULT_NOTIFY_CONFIG: NotifyConfig = {
  blockedSettleSec: 5,
  doneSettleSec: 15,
  cooldownSec: 30,
  skipWhileViewing: true,
  categories: {
    blocked: true,
    done: true,
    update: false,
    cacheCold: false,
    machineLoad: true,
  },
  publicUrl: '',
  telegram: {
    configured: false,
    enabled: false,
    chatIdTail: '',
  },
}

const CATEGORY_ITEMS: { key: keyof NotifyCategories; label: string }[] = [
  { key: 'blocked', label: 'Agent blocked' },
  { key: 'done', label: 'Agent finished' },
  { key: 'update', label: 'Server updates' },
  { key: 'cacheCold', label: 'Prompt cache cooling' },
  { key: 'machineLoad', label: 'High machine load' },
]

const MUTE_OPTIONS = [
  { label: '1h', minutes: 60 },
  { label: '4h', minutes: 240 },
  { label: '8h', minutes: 480 },
]

function ServerNotifySection({
  isOpen,
  onUpdateSetting,
}: {
  isOpen: boolean
  onUpdateSetting: <K extends keyof Settings>(
    key: K,
    value: Settings[K],
  ) => void
}) {
  const [config, setConfig] = useState<NotifyConfig>(DEFAULT_NOTIFY_CONFIG)
  const [publicUrlDraft, setPublicUrlDraft] = useState('')
  const [botToken, setBotToken] = useState('')
  const [chatId, setChatId] = useState('')
  const [tgFeedback, setTgFeedback] = useState<string>()
  const [tgError, setTgError] = useState<string>()

  useEffect(() => {
    if (!isOpen || typeof fetch !== 'function') return
    const ctrl = new AbortController()
    fetchNotifyConfig(ctrl.signal)
      .then((cfg) => {
        if (ctrl.signal.aborted || !cfg?.categories) return
        setConfig(cfg)
        setPublicUrlDraft(cfg.publicUrl ?? '')
      })
      .catch(() => {})
    return () => ctrl.abort()
  }, [isOpen])

  const applyConfigUpdate = async (
    patch: Parameters<typeof updateNotifyConfig>[0],
  ) => {
    try {
      const updated = await updateNotifyConfig(patch)
      if (updated?.categories) {
        setConfig(updated)
        setPublicUrlDraft(updated.publicUrl ?? '')
      }
      setTgError(undefined)
    } catch (err) {
      setTgError(
        (err as Error)?.message || 'Could not update notification settings',
      )
    }
  }

  const handleMute = async (minutes: number) => {
    try {
      const mutedUntil = await setNotifyMute(minutes)
      setConfig((prev) => ({ ...prev, mutedUntil: mutedUntil || undefined }))
    } catch {
      // ignore
    }
  }

  const handleSaveTelegram = async () => {
    setTgFeedback(undefined)
    setTgError(undefined)
    try {
      const st = await updateTelegramConfig({
        botToken: botToken.trim(),
        chatId: chatId.trim(),
        enabled: true,
      })
      setConfig((prev) => ({ ...prev, telegram: st }))
      setBotToken('')
      setChatId('')
      setTgFeedback('Telegram configured')
    } catch (err) {
      setTgError((err as Error)?.message || 'Invalid Telegram credentials')
    }
  }

  const handleToggleTelegram = async (enabled: boolean) => {
    setTgFeedback(undefined)
    setTgError(undefined)
    try {
      const st = await updateTelegramConfig({ enabled })
      setConfig((prev) => ({ ...prev, telegram: st }))
    } catch (err) {
      setTgError((err as Error)?.message || 'Could not update Telegram status')
    }
  }

  const handleTestTelegram = async () => {
    setTgFeedback(undefined)
    setTgError(undefined)
    try {
      await testTelegramNotification()
      setTgFeedback('Test notification sent')
    } catch (err) {
      setTgError((err as Error)?.message || 'Telegram test failed')
    }
  }

  const handleRemoveTelegram = async () => {
    setTgFeedback(undefined)
    setTgError(undefined)
    try {
      await deleteTelegramConfig()
      setConfig((prev) => ({
        ...prev,
        telegram: { configured: false, enabled: false, chatIdTail: '' },
      }))
      setTgFeedback('Telegram removed')
    } catch (err) {
      setTgError((err as Error)?.message || 'Could not remove Telegram')
    }
  }

  const savePublicUrl = () => {
    const trimmed = publicUrlDraft.trim()
    if (trimmed !== config.publicUrl) {
      void applyConfigUpdate({ publicUrl: trimmed })
    }
  }

  const isMuted = Boolean(config.mutedUntil)

  return (
    <>
      <SettingsRow
        title="Mute notifications"
        desc={
          isMuted
            ? `Muted until ${new Date(config.mutedUntil!).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`
            : 'Temporarily silence Web Push and Telegram alerts'
        }
      >
        <div className="flex flex-wrap items-center gap-1.5">
          {MUTE_OPTIONS.map((m) => (
            <Button
              key={m.label}
              size="sm"
              variant="ghost"
              aria-label={`Mute ${m.label}`}
              onClick={() => handleMute(m.minutes)}
            >
              {m.label}
            </Button>
          ))}
          {isMuted && (
            <Button size="sm" variant="ghost" onClick={() => handleMute(0)}>
              Unmute
            </Button>
          )}
        </div>
      </SettingsRow>

      <SettingsRow
        title="Skip while viewing"
        desc="Suppress alerts for a pane you are actively watching"
      >
        <Switch
          label="Skip while viewing"
          checked={config.skipWhileViewing}
          onChange={(v) => {
            setConfig((prev) => ({ ...prev, skipWhileViewing: v }))
            void applyConfigUpdate({ skipWhileViewing: v })
          }}
        />
      </SettingsRow>

      <SettingsRow
        title="Settle & cooldown"
        desc="Wait before alerting on transient status changes (seconds)"
      >
        <div className="flex flex-wrap items-center gap-2 text-[12px] text-fg-muted">
          <label className="flex items-center gap-1">
            Blocked
            <input
              type="number"
              min={0}
              max={60}
              aria-label="Blocked settle seconds"
              value={config.blockedSettleSec}
              onChange={(e) => {
                const v = Math.max(0, Math.min(60, Number(e.target.value) || 0))
                setConfig((prev) => ({ ...prev, blockedSettleSec: v }))
                onUpdateSetting('blockedSettleSec', v)
                void applyConfigUpdate({ blockedSettleSec: v })
              }}
              className={`w-14 px-1.5 text-center ${CONTROL} ${FOCUS_RING}`}
            />
          </label>
          <label className="flex items-center gap-1">
            Done
            <input
              type="number"
              min={0}
              max={120}
              aria-label="Done settle seconds"
              value={config.doneSettleSec}
              onChange={(e) => {
                const v = Math.max(
                  0,
                  Math.min(120, Number(e.target.value) || 0),
                )
                setConfig((prev) => ({ ...prev, doneSettleSec: v }))
                onUpdateSetting('doneSettleSec', v)
                void applyConfigUpdate({ doneSettleSec: v })
              }}
              className={`w-14 px-1.5 text-center ${CONTROL} ${FOCUS_RING}`}
            />
          </label>
          <label className="flex items-center gap-1">
            Cooldown
            <input
              type="number"
              min={0}
              max={600}
              aria-label="Notification cooldown seconds"
              value={config.cooldownSec}
              onChange={(e) => {
                const v = Math.max(
                  0,
                  Math.min(600, Number(e.target.value) || 0),
                )
                setConfig((prev) => ({ ...prev, cooldownSec: v }))
                void applyConfigUpdate({ cooldownSec: v })
              }}
              className={`w-16 px-1.5 text-center ${CONTROL} ${FOCUS_RING}`}
            />
          </label>
        </div>
      </SettingsRow>

      <div className="flex flex-col gap-2 px-4 py-2.5 ui-native:bg-surface-raised">
        <p className="m-0 text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]">
          Notification categories
        </p>
        <div className="flex flex-wrap gap-3">
          {CATEGORY_ITEMS.map(({ key, label }) => (
            <label
              key={key}
              className="flex items-center gap-1.5 text-[13px] text-fg"
            >
              <input
                type="checkbox"
                checked={config.categories[key]}
                onChange={(e) => {
                  const nextVal = e.target.checked
                  setConfig((prev) => ({
                    ...prev,
                    categories: { ...prev.categories, [key]: nextVal },
                  }))
                  void applyConfigUpdate({ categories: { [key]: nextVal } })
                }}
              />
              {label}
            </label>
          ))}
        </div>
      </div>

      <SettingsRow
        title="Public URL"
        desc="HTTPS base URL for deep-links in Telegram alerts"
        htmlFor="notify-public-url"
      >
        <input
          id="notify-public-url"
          aria-label="Public URL"
          type="url"
          value={publicUrlDraft}
          placeholder="https://termote.example.com"
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          onChange={(e) => setPublicUrlDraft(e.target.value)}
          onBlur={savePublicUrl}
          onKeyDown={(e) => e.key === 'Enter' && savePublicUrl()}
          className={`w-52 px-2 font-term ${CONTROL} ${FOCUS_RING}`}
        />
      </SettingsRow>

      <div className="flex flex-col gap-2 px-4 py-2.5 ui-native:bg-surface-raised">
        <div className="flex items-center justify-between gap-2">
          <div>
            <p className="m-0 text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]">
              Telegram Bot
            </p>
            <p className="m-0 text-[12px] text-fg-muted">
              {config.telegram.configured
                ? `Configured (chat …${config.telegram.chatIdTail})`
                : 'Send notifications via a Telegram bot'}
            </p>
          </div>
          {config.telegram.configured && (
            <Switch
              label="Enable Telegram"
              checked={config.telegram.enabled}
              onChange={handleToggleTelegram}
            />
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input
            type="password"
            aria-label="Telegram bot token"
            value={botToken}
            placeholder="Bot token (123456:ABC...)"
            autoComplete="off"
            onChange={(e) => setBotToken(e.target.value)}
            className={`w-44 px-2 font-term ${CONTROL} ${FOCUS_RING}`}
          />
          <input
            type="text"
            aria-label="Telegram chat ID"
            value={chatId}
            placeholder="Chat ID"
            autoComplete="off"
            onChange={(e) => setChatId(e.target.value)}
            className={`w-32 px-2 font-term ${CONTROL} ${FOCUS_RING}`}
          />
          <Button
            size="sm"
            disabled={!botToken.trim() || !chatId.trim()}
            onClick={handleSaveTelegram}
          >
            Save Telegram
          </Button>
          {config.telegram.configured && (
            <>
              <Button size="sm" variant="ghost" onClick={handleTestTelegram}>
                Send test
              </Button>
              <Button size="sm" variant="danger" onClick={handleRemoveTelegram}>
                Remove Telegram
              </Button>
            </>
          )}
        </div>
        {tgFeedback && (
          <p className="m-0 text-[12px] text-fg-muted">{tgFeedback}</p>
        )}
        {tgError && (
          <p role="alert" className="m-0 text-[12px] text-danger">
            {tgError}
          </p>
        )}
      </div>
    </>
  )
}

// A tiny drawing of each style's shape: corner radius, hairline or raised.
const STYLE_PREVIEW: Record<UiStyle, string> = {
  terminal: 'rounded-[2px] border border-border-strong bg-bg',
  native: 'rounded-[6px] bg-surface-raised shadow-sm ring-1 ring-border',
  neutral: 'rounded-[4px] border border-border-strong bg-surface',
}

function StylePreview({ style }: { style: UiStyle }) {
  return (
    <span
      aria-hidden="true"
      data-preview={style}
      className={`mr-1.5 flex h-4 w-6 flex-col justify-center gap-0.5 px-1 ${STYLE_PREVIEW[style]}`}
    >
      <span className="h-0.5 w-full rounded-full bg-accent" />
      <span className="h-0.5 w-2/3 rounded-full bg-fg-subtle" />
    </span>
  )
}

const STYLE_OPTIONS = UI_STYLES.map((s) => ({
  value: s.id,
  label: s.label,
  content: (
    <>
      <StylePreview style={s.id} />
      {s.label}
    </>
  ),
}))

interface GroupDef {
  id: string
  title: string
  rows: ReactNode
}

export function SettingsModal({
  isOpen,
  onClose,
  settings,
  onUpdateSetting,
  onShowGestureHints,
  updates,
  onClearHistory,
  historyCount = 0,
  tmuxBufferSupported = true,
  pasteBufferLabel = 'Session buffer',
  driveSizeSupported = false,
  pushAvailable = false,
  onEnableNotify,
  onDisableNotify,
  readOnly = false,
  devices = false,
  signins = false,
  onLogout,
  initialGroup,
}: Props) {
  const [activeGroup, setActiveGroup] = useState('appearance')

  // Opened for one group: pick it in the desktop rail, and on a phone, where
  // every group stacks, scroll it to the top once the sheet has mounted.
  useEffect(() => {
    if (!isOpen || !initialGroup) return
    setActiveGroup(initialGroup)
    const frame = requestAnimationFrame(() => {
      document
        .querySelector(`[data-group="${initialGroup}"]`)
        // jsdom has no scrollIntoView
        ?.scrollIntoView?.({ block: 'start' })
    })
    return () => cancelAnimationFrame(frame)
  }, [isOpen, initialGroup])
  const [keysEditorOpen, setKeysEditorOpen] = useState(false)

  const imeOption = IME_SEND_OPTIONS.find(
    (o) => o.value === settings.imeSendBehavior,
  )
  const pasteOptions = pasteSourceOptions(pasteBufferLabel)
  const pasteOption = pasteOptions.find((o) => o.value === settings.pasteSource)
  const hasActions = onShowGestureHints || onClearHistory

  const groups: GroupDef[] = [
    {
      id: 'appearance',
      title: 'Appearance',
      rows: (
        <>
          <div className="flex flex-col gap-2 px-4 py-2.5 ui-native:bg-surface-raised">
            <div>
              <p className="m-0 text-[15px] text-fg ui-terminal:font-label ui-terminal:text-[13px]">
                Interface style
              </p>
              <p className="m-0 text-[12px] text-fg-muted">
                Applies at once. Theme stays in the ⋯ menu.
              </p>
            </div>
            <SegmentedControl
              label="Interface style"
              options={STYLE_OPTIONS}
              value={settings.uiStyle}
              onChange={(v) => onUpdateSetting('uiStyle', v)}
              className="self-start"
            />
          </div>
          <SettingsRow
            title="Left-handed mode"
            desc="Mirror floating action buttons and thumb controls to the left side"
          >
            <Switch
              label="Left-handed mode"
              checked={settings.leftHanded === true}
              onChange={(v) => onUpdateSetting('leftHanded', v)}
            />
          </SettingsRow>
          <SettingsRow
            title="Zen mode"
            desc="Collapse top chrome into a minimal floating exit pill"
          >
            <Switch
              label="Zen mode"
              checked={settings.zenMode === true}
              onChange={(v) => onUpdateSetting('zenMode', v)}
            />
          </SettingsRow>
        </>
      ),
    },
    {
      id: 'keyboard',
      title: 'Keyboard',
      rows: (
        <>
          <SettingsRow title="Text input send behavior" desc={imeOption?.desc}>
            <SegmentedControl
              label="Text input send behavior"
              options={IME_SEND_OPTIONS.map((o) => ({
                value: o.value,
                label: o.label,
                content: o.label,
              }))}
              value={settings.imeSendBehavior}
              onChange={(v) => onUpdateSetting('imeSendBehavior', v)}
            />
          </SettingsRow>
          {tmuxBufferSupported && (
            <SettingsRow title="Paste button source" desc={pasteOption?.desc}>
              <SegmentedControl
                label="Paste button source"
                options={pasteOptions.map((o) => ({
                  value: o.value,
                  label: o.label,
                  content: o.label,
                }))}
                value={settings.pasteSource}
                onChange={(v) => onUpdateSetting('pasteSource', v)}
              />
            </SettingsRow>
          )}
          <SettingsRow
            title="Toolbar default expanded"
            desc="Show all keys when toolbar loads"
          >
            <Switch
              label="Toolbar default expanded"
              checked={settings.toolbarDefaultExpanded}
              onChange={(v) => onUpdateSetting('toolbarDefaultExpanded', v)}
            />
          </SettingsRow>
          <SettingsRow
            title="2D keys board"
            desc="Customize the 7-column key grid, sticky modifiers, and shareable tmk1: layout codes"
          >
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setKeysEditorOpen(true)}
            >
              Customize keys…
            </Button>
          </SettingsRow>
          <QuickRepliesSettingsRow
            value={settings.quickReplies ?? QUICK_REPLIES_DEFAULT}
            onSave={(v) => onUpdateSetting('quickReplies', v)}
          />
        </>
      ),
    },
    {
      id: 'terminal',
      title: 'Terminal',
      rows: (
        <>
          <TerminalFontRow
            value={settings.terminalFont}
            onSave={(v) => onUpdateSetting('terminalFont', v)}
          />
          <SettingsRow
            title="Disable right-click menu"
            desc="Block context menu on terminal area"
          >
            <Switch
              label="Disable right-click menu"
              checked={settings.disableContextMenu}
              onChange={(v) => onUpdateSetting('disableContextMenu', v)}
            />
          </SettingsRow>
          <SettingsRow
            title="Copy on select"
            desc="Copy the text you select with the mouse as soon as you release the button"
          >
            <Switch
              label="Copy on select"
              checked={settings.copyOnSelect}
              onChange={(v) => onUpdateSetting('copyOnSelect', v)}
            />
          </SettingsRow>
          {driveSizeSupported && (
            <SettingsRow
              title="Fit herdr pane to this device"
              desc="While the terminal shows; resizes the pane on the desktop too, until this page is hidden or closed, another view opens, or another device takes over"
            >
              <Switch
                label="Fit herdr pane to this device"
                checked={settings.driveTerminalSize}
                onChange={(v) => onUpdateSetting('driveTerminalSize', v)}
              />
            </SettingsRow>
          )}
        </>
      ),
    },
    {
      id: 'sessions',
      title: 'Sessions',
      rows: (
        <>
          <SettingsRow
            title="Show session tabs"
            desc="Display tab bar for quick session switching (desktop)"
          >
            <Switch
              label="Show session tabs"
              checked={settings.showSessionTabs}
              onChange={(v) => onUpdateSetting('showSessionTabs', v)}
            />
          </SettingsRow>
          <SettingsRow
            title="Blocked sessions first"
            desc="List sessions waiting on you at the top of each group"
          >
            <Switch
              label="Blocked sessions first"
              checked={settings.sortBlockedFirst}
              onChange={(v) => onUpdateSetting('sortBlockedFirst', v)}
            />
          </SettingsRow>
          {!readOnly && (
            <NotifyAgentsRow
              enabled={settings.notifyAgents}
              onChange={(v) => onUpdateSetting('notifyAgents', v)}
              pushAvailable={pushAvailable}
              onEnable={onEnableNotify}
              onDisable={onDisableNotify}
            />
          )}
          {!readOnly && pushAvailable && (
            <ServerNotifySection
              isOpen={isOpen}
              onUpdateSetting={onUpdateSetting}
            />
          )}
          <SettingsRow
            title="Stuck agent"
            desc={`Mark a working agent quiet for ${settings.stuckMinutes} min in the overview`}
          >
            <select
              aria-label="Stuck agent after"
              value={settings.stuckMinutes}
              onChange={(e) =>
                onUpdateSetting('stuckMinutes', Number(e.target.value))
              }
              className={`px-2 ${CONTROL} ${FOCUS_RING}`}
            >
              {STUCK_MINUTES_OPTIONS.map((v) => (
                <option key={v} value={v}>
                  {v} min
                </option>
              ))}
            </select>
          </SettingsRow>
          <SettingsRow
            title="Agent usage in the header"
            desc="Show how full the open pane's agent context is, and its Codex limits"
          >
            <Switch
              label="Agent usage in the header"
              checked={settings.showUsageChip}
              onChange={(v) => onUpdateSetting('showUsageChip', v)}
            />
          </SettingsRow>
          <SettingsRow
            title="Session poll interval"
            desc={`How often to sync session list (${formatSeconds(settings.pollInterval)})`}
          >
            <select
              aria-label="Session poll interval"
              value={settings.pollInterval}
              onChange={(e) =>
                onUpdateSetting('pollInterval', Number(e.target.value))
              }
              className={`px-2 ${CONTROL} ${FOCUS_RING}`}
            >
              {POLL_INTERVAL_OPTIONS.map((v) => (
                <option key={v} value={v}>
                  {formatSeconds(v)}
                </option>
              ))}
            </select>
          </SettingsRow>
        </>
      ),
    },
  ]

  groups.push({
    id: 'files',
    title: 'Files',
    rows: (
      <>
        <SettingsRow
          title="Preview HTML files"
          desc="Render .html files in a strictly sandboxed preview frame by default"
        >
          <Switch
            label="Preview HTML files"
            checked={settings.htmlPreview !== false}
            onChange={(v) => onUpdateSetting('htmlPreview', v)}
          />
        </SettingsRow>
        <ExcludedFoldersRow
          value={settings.findExcludes}
          onSave={(v) => onUpdateSetting('findExcludes', v)}
        />
      </>
    ),
  })

  if (devices) {
    groups.push({
      id: 'devices',
      title: 'Devices',
      rows: <DevicesSection />,
    })
  }

  if (signins) {
    groups.push({
      id: 'signins',
      title: 'Signed-in browsers',
      rows: <SigninsSection logOut={onLogout} />,
    })
  }

  if (updates) {
    groups.push({
      id: 'updates',
      title: 'Updates',
      rows: <UpdatesSection {...updates} />,
    })
  }

  if (hasActions) {
    groups.push({
      id: 'data',
      title: 'Data & help',
      rows: (
        <div className="flex flex-col gap-1 px-4 py-2.5 ui-native:bg-surface-raised">
          {onShowGestureHints && (
            <Button variant="ghost" onClick={onShowGestureHints}>
              Show Gesture Hints
            </Button>
          )}
          {onClearHistory && (
            <Button
              variant="danger"
              disabled={historyCount === 0}
              onClick={onClearHistory}
            >
              <Trash2 size={16} aria-hidden="true" />
              Clear Command History ({historyCount})
            </Button>
          )}
        </div>
      ),
    })
  }

  // A group can vanish (Data & help without handlers) while it is selected
  const shownGroup = groups.some((g) => g.id === activeGroup)
    ? activeGroup
    : groups[0].id

  return (
    <Sheet
      isOpen={isOpen}
      onClose={onClose}
      title="Settings"
      // Full screen on a phone, a two-column dialog on desktop. Full screen
      // reaches under the status bar of a notched phone, so the header is
      // pushed below it.
      className="max-md:h-(--app-height) max-md:max-h-(--app-height) max-md:pt-[env(safe-area-inset-top)] md:h-[34rem] md:max-w-3xl"
    >
      <div className="md:flex md:min-h-full">
        <nav
          aria-label="Settings groups"
          className="hidden md:sticky md:top-0 md:flex md:w-44 md:shrink-0 md:flex-col md:gap-0.5 md:self-start md:border-r md:border-border md:p-2"
        >
          {groups.map((g) => (
            <button
              key={g.id}
              type="button"
              aria-current={g.id === shownGroup ? 'true' : undefined}
              onClick={() => setActiveGroup(g.id)}
              className={`h-9 rounded-control px-3 text-left pointer-coarse:h-touch text-[14px] ui-terminal:font-label ui-terminal:text-[13px] ${FOCUS_RING} ${
                g.id === shownGroup
                  ? 'bg-accent-soft font-medium text-fg'
                  : 'text-fg-muted hover:bg-surface hover:text-fg'
              }`}
            >
              {g.title}
            </button>
          ))}
        </nav>
        <div className="min-w-0 flex-1">
          {groups.map((g) => (
            <SettingsGroup
              key={g.id}
              id={g.id}
              title={g.title}
              active={g.id === shownGroup}
            >
              {g.rows}
            </SettingsGroup>
          ))}
        </div>
      </div>
      <KeysBoardEditor
        isOpen={keysEditorOpen}
        onClose={() => setKeysEditorOpen(false)}
        layout={settings.keysLayout ?? DEFAULT_KEYS_LAYOUT}
        onSave={(l) => onUpdateSetting('keysLayout', l)}
      />
    </Sheet>
  )
}
