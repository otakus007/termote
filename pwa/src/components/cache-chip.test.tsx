import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CacheChip, cacheChipLabel, cacheChipTooltip } from './cache-chip'
import { GroupDialog } from './group-dialog'
import { StartAgentPanel } from './start-agent-panel'

describe('CacheChip', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-16T12:00:00Z'))
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders nothing when cache is absent', () => {
    const { container } = render(<CacheChip />)
    expect(container.firstChild).toBeNull()
  })

  it('renders warm state with green styling and countdown', () => {
    const lastTouchAt = new Date(Date.now() - 60_000).toISOString() // 240s left of 300s
    render(
      <CacheChip
        cache={{
          state: 'warm',
          lastTouchAt,
          ttlSec: 300,
        }}
      />,
    )
    const chip = screen.getByTestId('cache-chip')
    expect(chip).toHaveAttribute('data-cache-state', 'warm')
    expect(chip.textContent).toContain('4m 0s')
  })

  it('transitions to cooling state in final 25% of TTL', () => {
    const lastTouchAt = new Date(Date.now() - 250_000).toISOString() // 50s left of 300s (<= 75s)
    render(
      <CacheChip
        cache={{
          state: 'warm',
          lastTouchAt,
          ttlSec: 300,
        }}
      />,
    )
    const chip = screen.getByTestId('cache-chip')
    expect(chip).toHaveAttribute('data-cache-state', 'cooling')
    expect(chip.textContent).toContain('50s')
  })

  it('transitions to cold when TTL expires', () => {
    const lastTouchAt = new Date(Date.now() - 310_000).toISOString()
    render(
      <CacheChip
        cache={{
          state: 'warm',
          lastTouchAt,
          ttlSec: 300,
        }}
      />,
    )
    const chip = screen.getByTestId('cache-chip')
    expect(chip).toHaveAttribute('data-cache-state', 'cold')
    expect(chip.textContent).toContain('cold')
  })

  it('renders busted state and tooltip reason', () => {
    render(
      <CacheChip
        cache={{
          state: 'busted',
          ttlSec: 300,
          bustReason: 'compacted',
        }}
      />,
    )
    const chip = screen.getByTestId('cache-chip')
    expect(chip).toHaveAttribute('data-cache-state', 'busted')
    expect(chip.textContent).toContain('busted')
    expect(chip.getAttribute('title')).toContain('compacted')
  })

  it('formats labels and tooltips accurately', () => {
    expect(cacheChipLabel('warm', 125)).toBe('2m 5s')
    expect(cacheChipLabel('cooling', 45)).toBe('45s')
    expect(cacheChipLabel('cold', 0)).toBe('cold')
    expect(cacheChipLabel('busted', 0)).toBe('busted')
    expect(
      cacheChipTooltip(
        { state: 'busted', bustReason: 'model-change' },
        'busted',
        0,
      ),
    ).toContain('model-change')
  })
})

describe('StartAgentPanel with launchers and No prompts confirmation', () => {
  it('requires confirmation when a noPrompts option chip is selected', () => {
    const onStart = vi.fn()
    render(
      <StartAgentPanel
        kinds={['claude', 'codex']}
        launchers={{
          launchers: [
            {
              id: 'r1',
              name: 'Claude Fast',
              harness: 'claude',
              options: ['--continue'],
            },
          ],
          recentCommands: ['npm run dev'],
        }}
        onStart={onStart}
      />,
    )

    // Toggle "Skip permissions" (noPrompts: true)
    fireEvent.click(screen.getByRole('button', { name: /Skip permissions/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Claude Code' }))

    // Should not start yet; confirmation modal should appear
    expect(onStart).not.toHaveBeenCalled()
    expect(
      screen.getByRole('alertdialog', { name: 'Confirm unprompted launch' }),
    ).toBeInTheDocument()

    // Confirm launch
    fireEvent.click(screen.getByRole('button', { name: 'Start anyway' }))
    expect(onStart).toHaveBeenCalledWith('claude', {
      harness: 'claude',
      options: ['--dangerously-skip-permissions'],
    })
  })

  it('starts recipes and recent commands directly', () => {
    const onStart = vi.fn()
    render(
      <StartAgentPanel
        kinds={['claude', 'codex']}
        launchers={{
          launchers: [
            {
              id: 'r1',
              name: 'Claude Continue',
              harness: 'claude',
              options: ['--continue'],
            },
          ],
          recentCommands: ['npm test'],
        }}
        onStart={onStart}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Claude Continue' }))
    expect(onStart).toHaveBeenCalledWith('claude', {
      recipeId: 'r1',
      harness: 'claude',
      options: ['--continue'],
      command: undefined,
    })

    fireEvent.click(screen.getByRole('button', { name: 'npm test' }))
    expect(onStart).toHaveBeenCalledWith('claude', {
      command: 'npm test',
    })
  })
})

describe('GroupDialog folder chips', () => {
  beforeEach(() => {
    HTMLDialogElement.prototype.showModal = vi.fn(function (
      this: HTMLDialogElement,
    ) {
      this.setAttribute('open', '')
    })
    HTMLDialogElement.prototype.close = vi.fn()
  })

  it('fills directory and empty name when clicking a folder chip', () => {
    render(
      <GroupDialog
        noun="workspace"
        folders={{
          favourites: ['/srv/herdr-remote'],
          recent: ['/tmp/scratch'],
        }}
        onClose={vi.fn()}
        onCreate={vi.fn(async () => {})}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: '/srv/herdr-remote' }))
    expect(screen.getByLabelText('Name')).toHaveValue('herdr-remote')
    expect(
      screen.getByLabelText('Directory (an absolute path on the host)'),
    ).toHaveValue('/srv/herdr-remote')
  })
})
