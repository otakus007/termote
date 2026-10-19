import { describe, expect, it, vi } from 'vitest'
import {
  BUILTIN_KEYS_PRESETS,
  CLAUDE_CODE_KEYS_LAYOUT,
  consumeLatchedModifiers,
  DEFAULT_KEYS_LAYOUT,
  decodeKeysLayout,
  dispatchStepToken,
  encodeKeysLayout,
  INITIAL_STICKY_MODIFIERS,
  isValidStepToken,
  KEYS_LAYOUT_PREFIX,
  type KeysBoardLayout,
  MAX_SHARE_CODE_BYTES,
  NAVIGATION_KEYS_LAYOUT,
  nextStickyModifierState,
  PREFIX_KEYS_LAYOUT,
  VIM_KEYS_LAYOUT,
  validateKeysLayout,
} from './keys-layout'

describe('keys-layout presets', () => {
  it('defines all 5 built-in presets with valid cells', () => {
    const presets = [
      DEFAULT_KEYS_LAYOUT,
      CLAUDE_CODE_KEYS_LAYOUT,
      PREFIX_KEYS_LAYOUT,
      VIM_KEYS_LAYOUT,
      NAVIGATION_KEYS_LAYOUT,
    ]
    expect(Object.keys(BUILTIN_KEYS_PRESETS)).toEqual([
      'default',
      'claude-code',
      'prefix',
      'vim',
      'navigation',
    ])
    for (const preset of presets) {
      const validated = validateKeysLayout(preset)
      expect(validated).not.toBeNull()
      expect(validated?.keys.length).toBeGreaterThan(0)
    }
  })
})

describe('encodeKeysLayout / decodeKeysLayout', () => {
  it('round-trips a layout via tmk1: base64url code', () => {
    const code = encodeKeysLayout(PREFIX_KEYS_LAYOUT)
    expect(code.startsWith(KEYS_LAYOUT_PREFIX)).toBe(true)
    const decoded = decodeKeysLayout(code)
    expect(decoded).toEqual(PREFIX_KEYS_LAYOUT)
  })

  it('rejects missing prefix, empty input, or oversized (> 16 KB) input', () => {
    expect(decodeKeysLayout('')).toBeNull()
    expect(decodeKeysLayout('invalid:abc')).toBeNull()
    expect(decodeKeysLayout('tmk1:')).toBeNull()
    expect(decodeKeysLayout('tmk1:!!!not-base64!!!')).toBeNull()
    const huge = 'tmk1:' + 'A'.repeat(MAX_SHARE_CODE_BYTES + 10)
    expect(decodeKeysLayout(huge)).toBeNull()
  })

  it('rejects out-of-range colSpan, rowSpan, or step counts', () => {
    const makeCode = (obj: unknown) =>
      KEYS_LAYOUT_PREFIX +
      btoa(JSON.stringify(obj))
        .replace(/\+/g, '-')
        .replace(/\//g, '_')
        .replace(/=+$/, '')

    expect(
      decodeKeysLayout(
        makeCode({
          version: 1,
          preset: 'custom',
          name: 'Bad colSpan',
          keys: [{ id: 'a', label: 'A', colSpan: 4, rowSpan: 1, steps: ['a'] }],
        }),
      ),
    ).toBeNull()

    expect(
      decodeKeysLayout(
        makeCode({
          version: 1,
          preset: 'custom',
          name: 'Bad rowSpan',
          keys: [{ id: 'a', label: 'A', colSpan: 1, rowSpan: 3, steps: ['a'] }],
        }),
      ),
    ).toBeNull()

    expect(
      decodeKeysLayout(
        makeCode({
          version: 1,
          preset: 'custom',
          name: 'Too many steps',
          keys: [
            {
              id: 'a',
              label: 'A',
              colSpan: 1,
              rowSpan: 1,
              steps: ['1', '2', '3', '4', '5'],
            },
          ],
        }),
      ),
    ).toBeNull()

    expect(
      decodeKeysLayout(
        makeCode({
          version: 1,
          preset: 'custom',
          name: 'Empty steps',
          keys: [{ id: 'a', label: 'A', colSpan: 1, rowSpan: 1, steps: [] }],
        }),
      ),
    ).toBeNull()
  })

  it('rejects > 64 keys', () => {
    const keys = Array.from({ length: 65 }, (_, i) => ({
      id: `k-${i}`,
      label: 'K',
      colSpan: 1,
      rowSpan: 1,
      steps: ['a'],
    }))
    const raw = { version: 1, preset: 'custom', name: 'Many', keys }
    expect(validateKeysLayout(raw)).toBeNull()
  })

  it('rejects control-character injection and prototype pollution payloads', () => {
    expect(isValidStepToken('hello\x00world')).toBe(false)
    expect(isValidStepToken('rm -rf\r\n')).toBe(false)

    const pollutedJson =
      '{"version":1,"__proto__":{"polluted":true},"keys":[{"id":"k","label":"K","colSpan":1,"rowSpan":1,"steps":["a"]}]}'
    const code =
      KEYS_LAYOUT_PREFIX +
      btoa(pollutedJson)
        .replace(/\+/g, '-')
        .replace(/\//g, '_')
        .replace(/=+$/, '')
    expect(decodeKeysLayout(code)).toBeNull()
    expect(({} as Record<string, unknown>).polluted).toBeUndefined()
  })
})

describe('sticky modifiers and step dispatch', () => {
  it('latches on single tap, locks on double tap, and consumes latched state', () => {
    let state = INITIAL_STICKY_MODIFIERS
    state = nextStickyModifierState(state, 'Ctrl', false)
    expect(state.Ctrl).toBe('latched')

    // Consuming after a key press resets latched -> off
    expect(consumeLatchedModifiers(state).Ctrl).toBe('off')

    // Double tap locks
    state = nextStickyModifierState(state, 'Ctrl', true)
    expect(state.Ctrl).toBe('locked')
    expect(consumeLatchedModifiers(state).Ctrl).toBe('locked')

    // Single tap when locked turns it off
    state = nextStickyModifierState(state, 'Ctrl', false)
    expect(state.Ctrl).toBe('off')
  })

  it('dispatches chords, named keys, and sticky modifiers accurately', () => {
    const onKeyPress = vi.fn()
    const onCtrlKey = vi.fn()
    const onCtrlShiftKey = vi.fn()
    const onSendText = vi.fn()
    const cb = { onKeyPress, onCtrlKey, onCtrlShiftKey, onSendText }

    dispatchStepToken('Ctrl+B', INITIAL_STICKY_MODIFIERS, cb)
    expect(onCtrlKey).toHaveBeenCalledWith('B')

    dispatchStepToken('c', { Ctrl: 'latched', Alt: 'off', Shift: 'off' }, cb)
    expect(onCtrlKey).toHaveBeenCalledWith('C')

    dispatchStepToken('Tab', { Ctrl: 'off', Alt: 'off', Shift: 'latched' }, cb)
    expect(onKeyPress).toHaveBeenCalledWith('Shift+Tab')

    dispatchStepToken('f', { Ctrl: 'off', Alt: 'latched', Shift: 'off' }, cb)
    expect(onSendText).toHaveBeenCalledWith('\x1bf')

    const customLayout: KeysBoardLayout = {
      version: 1,
      preset: 'custom',
      name: 'Custom',
      keys: [{ id: '1', label: 'A', colSpan: 1, rowSpan: 1, steps: ['a'] }],
    }
    expect(validateKeysLayout(customLayout)).toEqual(customLayout)
  })
})
