import { Hourglass } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { AgentCacheInfo } from '../hooks/use-mux-api'
import { formatTokens } from './agent-card'

interface Props {
  cache?: AgentCacheInfo
  serverOffsetMs?: number
}

export type EffectiveCacheState = 'warm' | 'cooling' | 'cold' | 'busted'

export function formatRemainingSec(sec: number): string {
  const s = Math.max(0, Math.floor(sec))
  const m = Math.floor(s / 60)
  const rem = s % 60
  if (m > 0) return `${m}m ${rem}s`
  return `${rem}s`
}

export function cacheChipLabel(
  state: EffectiveCacheState,
  remainingSec: number,
  reason?: string,
): string {
  switch (state) {
    case 'warm':
    case 'cooling':
      return remainingSec > 0 ? formatRemainingSec(remainingSec) : 'warm'
    case 'cold':
      return reason ? `cold · ${reason}` : 'cold'
    case 'busted':
      return reason ? `busted · ${reason}` : 'busted'
  }
}

export function cacheChipTooltip(
  cache: AgentCacheInfo,
  state: EffectiveCacheState,
  remainingSec: number,
): string {
  const parts: string[] = [`Prompt cache: ${state}`]
  const reason = cache.bustReason || cache.reason
  if (reason) parts.push(`Reason: ${reason}`)
  if ((state === 'warm' || state === 'cooling') && remainingSec > 0) {
    parts.push(`Remaining: ${formatRemainingSec(remainingSec)}`)
  } else if (cache.minutesRemain !== undefined) {
    parts.push(`Remaining: ${cache.minutesRemain}m`)
  }
  if (cache.ttlSec !== undefined && cache.ttlSec > 0) {
    parts.push(`TTL: ${Math.round(cache.ttlSec / 60)}m`)
  } else if (cache.windowMinutes !== undefined) {
    parts.push(`TTL window: ${cache.windowMinutes}m`)
  }
  if (cache.hitRate !== undefined) {
    parts.push(`Hit rate: ${Math.round(cache.hitRate * 100)}%`)
  }
  const tokens: string[] = []
  if (cache.cacheReadTokens !== undefined) {
    tokens.push(`read ${formatTokens(cache.cacheReadTokens)}`)
  }
  if (cache.cacheWriteTokens !== undefined) {
    tokens.push(`write ${formatTokens(cache.cacheWriteTokens)}`)
  }
  if (cache.inputTokens !== undefined) {
    tokens.push(`input ${formatTokens(cache.inputTokens)}`)
  }
  if (tokens.length > 0) {
    parts.push(`Tokens: ${tokens.join(', ')}`)
  }
  return parts.join(' · ')
}

function evaluateCache(
  cache: AgentCacheInfo,
  nowMs: number,
): { state: EffectiveCacheState; remainingSec: number } {
  if (cache.state === 'busted' || cache.bustReason) {
    return { state: 'busted', remainingSec: 0 }
  }
  if (cache.state === 'cold') {
    return { state: 'cold', remainingSec: 0 }
  }
  const ttlSec =
    cache.ttlSec && cache.ttlSec > 0
      ? cache.ttlSec
      : cache.windowMinutes
        ? cache.windowMinutes * 60
        : 300
  const touchStr = cache.lastTouchAt || cache.lastActivityTs
  if (touchStr) {
    const touchMs = Date.parse(touchStr)
    if (!Number.isNaN(touchMs)) {
      const remaining = ttlSec - (nowMs - touchMs) / 1000
      if (remaining <= 0) {
        return { state: 'cold', remainingSec: 0 }
      }
      if (remaining <= ttlSec / 4) {
        return { state: 'cooling', remainingSec: remaining }
      }
      return { state: 'warm', remainingSec: remaining }
    }
  }
  if (cache.minutesRemain !== undefined) {
    const remSec = cache.minutesRemain * 60
    if (remSec <= 0) return { state: 'cold', remainingSec: 0 }
    if (remSec <= ttlSec / 4) return { state: 'cooling', remainingSec: remSec }
    return { state: 'warm', remainingSec: remSec }
  }
  return {
    state: cache.state === 'cooling' ? 'cooling' : 'warm',
    remainingSec: 0,
  }
}

export function CacheChip({ cache, serverOffsetMs = 0 }: Props) {
  const [nowMs, setNowMs] = useState(() => Date.now() + serverOffsetMs)

  const hasCountdown =
    !!cache &&
    cache.state !== 'unknown' &&
    cache.state !== 'cold' &&
    cache.state !== 'busted' &&
    !cache.bustReason &&
    Boolean(cache.lastTouchAt || cache.lastActivityTs)

  useEffect(() => {
    setNowMs(Date.now() + serverOffsetMs)
    if (!hasCountdown) return
    const id = window.setInterval(() => {
      setNowMs(Date.now() + serverOffsetMs)
    }, 1000)
    return () => window.clearInterval(id)
  }, [hasCountdown, serverOffsetMs])

  if (!cache || cache.state === 'unknown') return null
  const { state, remainingSec } = evaluateCache(cache, nowMs)
  const reason = cache.bustReason || cache.reason
  const label = cacheChipLabel(state, remainingSec, reason)
  if (!label) return null
  const tooltip = cacheChipTooltip(cache, state, remainingSec)

  // Green when warm, red in final quarter (cooling) or busted, blue when cold
  const tone =
    state === 'warm'
      ? 'border-success/60 text-success'
      : state === 'cooling' || state === 'busted'
        ? 'border-danger/60 text-danger'
        : 'border-accent/60 text-accent'

  return (
    <span
      role="note"
      data-testid="cache-chip"
      data-cache-state={state}
      aria-label={tooltip}
      title={tooltip}
      className={`inline-flex shrink-0 items-center gap-1 rounded-control border bg-surface px-1.5 py-0.5 text-[11px] tabular-nums ${tone}`}
    >
      <Hourglass size={11} aria-hidden="true" />
      <span>{label}</span>
    </span>
  )
}
