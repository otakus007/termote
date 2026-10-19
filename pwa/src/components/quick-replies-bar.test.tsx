import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { QuickRepliesBar } from './quick-replies-bar'

describe('QuickRepliesBar', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders default quick reply chips and sends on tap', () => {
    const onSendReply = vi.fn()
    render(<QuickRepliesBar onSendReply={onSendReply} />)

    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(onSendReply).toHaveBeenCalledWith('Continue')

    fireEvent.click(
      screen.getByRole('button', { name: 'Run tests & fix failures' }),
    )
    expect(onSendReply).toHaveBeenCalledWith('Run tests & fix failures')
  })

  it('collapses and expands the quick replies bar', () => {
    render(<QuickRepliesBar onSendReply={vi.fn()} />)
    const toggle = screen.getByTestId('quick-replies-toggle')
    expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument()

    fireEvent.click(toggle)
    expect(screen.queryByRole('button', { name: 'Continue' })).toBeNull()

    fireEvent.click(toggle)
    expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument()
  })

  it('supports two-tap confirmation when confirmBeforeSend=true', () => {
    vi.useFakeTimers()
    const onSendReply = vi.fn()
    render(<QuickRepliesBar onSendReply={onSendReply} confirmBeforeSend />)

    const btn = screen.getByRole('button', { name: 'Commit changes' })
    fireEvent.click(btn)
    expect(onSendReply).not.toHaveBeenCalled()
    expect(btn).toHaveTextContent('Send "Commit changes"?')

    // Disarms after 2.5s
    act(() => {
      vi.advanceTimersByTime(2600)
    })
    expect(btn).toHaveTextContent('Commit changes')

    // Two taps within 2.5s sends
    fireEvent.click(btn)
    fireEvent.click(btn)
    expect(onSendReply).toHaveBeenCalledWith('Commit changes')
  })

  it('renders Harness Shortcut Belt adapting to claude vs codex', () => {
    const onRunShortcut = vi.fn()
    const { rerender } = render(
      <QuickRepliesBar
        agentName="claude"
        onSendReply={vi.fn()}
        onRunShortcut={onRunShortcut}
      />,
    )

    fireEvent.click(screen.getByTestId('harness-belt-toggle'))
    const belt = screen.getByTestId('harness-shortcut-belt')
    expect(belt).toHaveTextContent('/effort')
    expect(belt).toHaveTextContent('/branch')

    fireEvent.click(
      belt.querySelector('[data-shortcut-id="compact"]') as HTMLElement,
    )
    expect(onRunShortcut).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'compact', command: '/compact' }),
    )

    rerender(
      <QuickRepliesBar
        agentName="codex"
        onSendReply={vi.fn()}
        onRunShortcut={onRunShortcut}
      />,
    )
    expect(screen.getByTestId('harness-shortcut-belt')).toHaveTextContent(
      '/worktree',
    )
  })
})
