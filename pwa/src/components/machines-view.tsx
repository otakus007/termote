import {
  Activity,
  AlertTriangle,
  Cpu,
  HardDrive,
  Plus,
  Server,
  Trash2,
  Wifi,
  X,
} from 'lucide-react'
import { useCallback, useEffect, useId, useMemo, useState } from 'react'
import {
  fetchMachineStatus,
  type MachineAlertRule,
  type MachineSample,
  type MachineStatusResponse,
  RequestError,
  updateMachineAlerts,
} from '../hooks/use-mux-api'
import { Button, FOCUS_RING, IconButton } from './ui/button'

export function formatMachineBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let val = bytes
  let idx = 0
  while (val >= 1024 && idx < units.length - 1) {
    val /= 1024
    idx++
  }
  return `${val >= 100 || idx === 0 ? Math.round(val) : val.toFixed(1)} ${units[idx]}`
}

export function formatMachineRate(bytesPerSec: number): string {
  return `${formatMachineBytes(bytesPerSec)}/s`
}

export function formatUptime(uptimeSec: number): string {
  if (!Number.isFinite(uptimeSec) || uptimeSec <= 0) return '—'
  const days = Math.floor(uptimeSec / 86400)
  const hours = Math.floor((uptimeSec % 86400) / 3600)
  const mins = Math.floor((uptimeSec % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${mins}m`
  return `${Math.max(1, mins)}m`
}

interface SparkChartProps {
  label: string
  values: number[]
  thresholdPct?: number
  colorClass?: string
}

export function MachineSparkChart({
  label,
  values,
  thresholdPct,
  colorClass = 'text-accent',
}: SparkChartProps) {
  const gradId = useId()
  const width = 280
  const height = 64
  const pad = 4

  const { linePath, areaPath } = useMemo(() => {
    const pts = values.length > 0 ? values : [0]
    const n = pts.length
    const coords = pts.map((v, i) => {
      const x = n === 1 ? width / 2 : pad + (i / (n - 1)) * (width - pad * 2)
      const clamped = Math.max(0, Math.min(100, v))
      const y = height - pad - (clamped / 100) * (height - pad * 2)
      return [x, y] as const
    })
    if (coords.length === 1) {
      const [, y] = coords[0]
      const line = `M ${pad} ${y.toFixed(1)} L ${width - pad} ${y.toFixed(1)}`
      const area = `${line} L ${width - pad} ${height - pad} L ${pad} ${height - pad} Z`
      return { linePath: line, areaPath: area }
    }
    const line = coords
      .map(
        ([x, y], i) => `${i === 0 ? 'M' : 'L'} ${x.toFixed(1)} ${y.toFixed(1)}`,
      )
      .join(' ')
    const firstX = coords[0][0].toFixed(1)
    const lastX = coords[coords.length - 1][0].toFixed(1)
    const area = `${line} L ${lastX} ${height - pad} L ${firstX} ${height - pad} Z`
    return { linePath: line, areaPath: area }
  }, [values])

  const thresholdY =
    thresholdPct !== undefined && thresholdPct >= 50 && thresholdPct <= 99
      ? height - pad - (thresholdPct / 100) * (height - pad * 2)
      : undefined

  return (
    <svg
      role="img"
      aria-label={label}
      viewBox={`0 0 ${width} ${height}`}
      className={`h-16 w-full overflow-visible ${colorClass}`}
    >
      <defs>
        <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="currentColor" stopOpacity="0.28" />
          <stop offset="100%" stopColor="currentColor" stopOpacity="0.02" />
        </linearGradient>
      </defs>
      {thresholdY !== undefined && (
        <line
          data-testid="threshold-line"
          x1={pad}
          y1={thresholdY}
          x2={width - pad}
          y2={thresholdY}
          stroke="currentColor"
          strokeOpacity="0.45"
          strokeDasharray="4 4"
          strokeWidth="1"
        />
      )}
      <path d={areaPath} fill={`url(#${gradId})`} />
      <path
        d={linePath}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

export interface HostResourcePillProps {
  status?: MachineStatusResponse | null
  active?: boolean
  onClick: () => void
}

export function HostResourcePill({
  status,
  active,
  onClick,
}: HostResourcePillProps) {
  const firingCount =
    status?.alerts?.filter((a) => a.enabled && a.firing).length ?? 0
  const fullestDisk = status?.current?.disks?.[0]
  const summary = status
    ? `CPU ${Math.round(status.current.cpuPct)}% · Mem ${Math.round(status.current.memPct)}%${
        fullestDisk ? ` · Disk ${Math.round(fullestDisk.usedPct)}%` : ''
      }`
    : 'Host'

  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      aria-label={
        status
          ? `Host resources: ${summary}${firingCount > 0 ? `, ${firingCount} active alert` : ''}`
          : 'Host resources'
      }
      title="Host resource monitor & load alerts"
      className={`flex h-7 shrink-0 items-center gap-1.5 rounded-control border px-2.5 text-[12px] tabular-nums transition-colors pointer-coarse:h-9 ${FOCUS_RING} ${
        firingCount > 0
          ? 'border-danger/60 bg-danger/10 text-danger'
          : active
            ? 'border-accent bg-surface-raised text-fg'
            : 'border-border bg-surface text-fg-muted hover:bg-surface-raised hover:text-fg'
      }`}
    >
      <Server size={14} aria-hidden="true" />
      <span>{summary}</span>
      {firingCount > 0 && (
        <span
          data-testid="host-alert-badge"
          className="inline-flex items-center gap-0.5 rounded-full bg-danger px-1.5 py-0.2 text-[11px] font-semibold text-white"
        >
          <AlertTriangle size={11} aria-hidden="true" />
          {firingCount}
        </span>
      )}
    </button>
  )
}

const ROLE_LABELS: Record<'lead' | 'deputy' | 'member', string> = {
  lead: 'Lead',
  deputy: 'Standby Deputy',
  member: 'Member',
}

export interface MachinesViewProps {
  readOnly?: boolean
  onClose?: () => void
  initialRange?: '1h' | '24h'
  onStatusLoaded?: (status: MachineStatusResponse) => void
}

export function MachinesView({
  readOnly = false,
  onClose,
  initialRange = '1h',
  onStatusLoaded,
}: MachinesViewProps) {
  const [range, setRange] = useState<'1h' | '24h'>(initialRange)
  const [selectedMachineId, setSelectedMachineId] = useState<string>('local')
  const [status, setStatus] = useState<MachineStatusResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [draftAlerts, setDraftAlerts] = useState<MachineAlertRule[]>([])
  const [dirtyAlerts, setDirtyAlerts] = useState(false)
  const [savingAlerts, setSavingAlerts] = useState(false)
  const [saveMessage, setSaveMessage] = useState<string | null>(null)

  const load = useCallback(
    async (
      targetRange: '1h' | '24h',
      signal?: AbortSignal,
      machineId?: string,
    ) => {
      try {
        const res =
          machineId && machineId !== 'local'
            ? await fetchMachineStatus(targetRange, signal, machineId)
            : await fetchMachineStatus(targetRange, signal)
        setStatus(res)
        setError(null)
        onStatusLoaded?.(res)
        setDraftAlerts((prev) =>
          dirtyAlerts && prev.length > 0 ? prev : res.alerts,
        )
      } catch (err) {
        if (signal?.aborted) return
        setError(
          err instanceof RequestError
            ? err.message
            : 'Could not read host resource telemetry.',
        )
      } finally {
        if (!signal?.aborted) setLoading(false)
      }
    },
    [dirtyAlerts, onStatusLoaded],
  )

  useEffect(() => {
    const ctrl = new AbortController()
    setLoading(true)
    void load(range, ctrl.signal, selectedMachineId)
    const id = setInterval(() => {
      void load(range, ctrl.signal, selectedMachineId)
    }, 30_000)
    return () => {
      ctrl.abort()
      clearInterval(id)
    }
  }, [range, selectedMachineId, load])

  const machines = status?.machines ?? []
  const isMultiHost = machines.length > 1
  const selectedNode = useMemo(
    () =>
      isMultiHost
        ? machines.find((m) => m.id === selectedMachineId)
        : undefined,
    [isMultiHost, machines, selectedMachineId],
  )

  const samples: MachineSample[] =
    selectedNode?.samples ?? status?.samples ?? []
  const cpuSeries = useMemo(() => samples.map((s) => s.cpuPct), [samples])
  const memSeries = useMemo(() => samples.map((s) => s.memPct), [samples])

  const cpuThreshold = useMemo(
    () =>
      draftAlerts.find((r) => r.enabled && r.metric === 'cpu')?.thresholdPct,
    [draftAlerts],
  )
  const memThreshold = useMemo(
    () =>
      draftAlerts.find((r) => r.enabled && r.metric === 'memory')?.thresholdPct,
    [draftAlerts],
  )

  const updateRule = (idx: number, patch: Partial<MachineAlertRule>) => {
    setDraftAlerts((prev) =>
      prev.map((r, i) => (i === idx ? { ...r, ...patch } : r)),
    )
    setDirtyAlerts(true)
    setSaveMessage(null)
  }

  const addRule = () => {
    if (draftAlerts.length >= 16) return
    const nextIdx = draftAlerts.length + 1
    setDraftAlerts((prev) => [
      ...prev,
      {
        id: `rule-${nextIdx}-${Date.now().toString(36)}`,
        metric: 'cpu',
        thresholdPct: 85,
        durationMin: 10,
        enabled: true,
        firing: false,
      },
    ])
    setDirtyAlerts(true)
    setSaveMessage(null)
  }

  const removeRule = (idx: number) => {
    setDraftAlerts((prev) => prev.filter((_, i) => i !== idx))
    setDirtyAlerts(true)
    setSaveMessage(null)
  }

  const handleSaveAlerts = async () => {
    setSavingAlerts(true)
    setSaveMessage(null)
    try {
      const updated = await updateMachineAlerts(draftAlerts)
      setDraftAlerts(updated)
      setDirtyAlerts(false)
      setStatus((prev) => (prev ? { ...prev, alerts: updated } : prev))
      setSaveMessage('Alert rules saved.')
    } catch (err) {
      setSaveMessage(
        err instanceof RequestError
          ? err.message
          : 'Failed to save alert rules.',
      )
    } finally {
      setSavingAlerts(false)
    }
  }

  const current = selectedNode?.current ?? status?.current
  const host = selectedNode?.host ?? status?.host

  return (
    <section
      aria-label="Host resources"
      className="flex flex-col gap-4 rounded-panel border border-border bg-surface p-4 text-[13px] text-fg"
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border pb-3">
        <div className="flex items-center gap-2">
          <Server size={18} className="text-accent" aria-hidden="true" />
          <div>
            <h3 className="text-[14px] font-semibold">
              {host?.name || selectedNode?.name || 'Local Host'}
            </h3>
            {host && (
              <p className="text-[12px] text-fg-muted">
                {host.os}/{host.arch} · {host.cpus}{' '}
                {host.cpus === 1 ? 'CPU' : 'CPUs'} · Uptime{' '}
                {formatUptime(host.uptimeSec)}
              </p>
            )}
          </div>
        </div>
        <div className="flex items-center gap-1.5">
          <fieldset
            aria-label="Time range"
            className="flex rounded-control border border-border bg-bg p-0.5"
          >
            {(['1h', '24h'] as const).map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setRange(r)}
                aria-pressed={range === r}
                className={`rounded-[calc(var(--radius-control)-2px)] px-2.5 py-1 text-[12px] font-medium transition-colors ${FOCUS_RING} ${
                  range === r
                    ? 'bg-surface-raised text-fg shadow-xs'
                    : 'text-fg-muted hover:text-fg'
                }`}
              >
                {r}
              </button>
            ))}
          </fieldset>
          {onClose && (
            <IconButton
              onClick={onClose}
              aria-label="Close host resources"
              title="Close"
            >
              <X size={16} aria-hidden="true" />
            </IconButton>
          )}
        </div>
      </div>

      {isMultiHost && (
        <fieldset
          data-testid="machines-crew-grid"
          aria-label="Crew machines"
          className="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3"
        >
          {machines.map((m) => {
            const active = selectedMachineId === m.id
            const disk0 = m.current?.disks?.[0]
            return (
              <button
                key={m.id}
                type="button"
                data-testid={`machine-node-${m.id}`}
                aria-pressed={active}
                onClick={() => setSelectedMachineId(m.id)}
                className={`flex flex-col gap-1.5 rounded-control border p-2.5 text-left transition-colors ${FOCUS_RING} ${
                  active
                    ? 'border-accent bg-accent-soft text-fg'
                    : 'border-border bg-bg text-fg hover:bg-surface-raised'
                }`}
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="flex min-w-0 items-center gap-1.5 font-semibold">
                    <span
                      aria-hidden="true"
                      className={`size-2 shrink-0 rounded-full ${
                        m.status === 'online' ? 'bg-success' : 'bg-danger'
                      }`}
                    />
                    <span className="truncate">{m.name}</span>
                  </span>
                  <span className="shrink-0 rounded-control border border-border px-1.5 py-0.5 text-[11px] font-medium text-fg-muted">
                    {ROLE_LABELS[m.role] ?? 'Member'}
                  </span>
                </div>
                <div className="flex items-center justify-between gap-2 text-[11px] text-fg-muted tabular-nums">
                  <span>
                    {m.status === 'online' && m.current
                      ? `CPU ${Math.round(m.current.cpuPct)}% · Mem ${Math.round(m.current.memPct)}%${
                          disk0 ? ` · Disk ${Math.round(disk0.usedPct)}%` : ''
                        }`
                      : m.status === 'unreachable'
                        ? 'Unreachable'
                        : 'Online'}
                  </span>
                  <span
                    className={
                      m.status === 'online' ? 'text-success' : 'text-danger'
                    }
                  >
                    {m.status === 'online' ? 'Online' : 'Unreachable'}
                  </span>
                </div>
              </button>
            )
          })}
        </fieldset>
      )}

      {error && (
        <p role="alert" className="text-[12px] text-danger">
          {error}
        </p>
      )}

      {loading && !status ? (
        <p role="status" className="py-4 text-center text-fg-muted">
          Loading host telemetry…
        </p>
      ) : current ? (
        <>
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {/* CPU Card */}
            <div className="flex flex-col gap-2 rounded-control border border-border bg-bg p-3">
              <div className="flex items-center justify-between">
                <span className="flex items-center gap-1.5 font-medium text-fg">
                  <Cpu size={15} className="text-fg-muted" aria-hidden="true" />
                  CPU
                </span>
                <span
                  data-testid="cpu-pct"
                  className="text-[15px] font-semibold tabular-nums"
                >
                  {current.cpuPct.toFixed(1)}%
                </span>
              </div>
              <MachineSparkChart
                label={`CPU usage (${range})`}
                values={cpuSeries}
                thresholdPct={cpuThreshold}
              />
            </div>

            {/* Memory Card */}
            <div className="flex flex-col gap-2 rounded-control border border-border bg-bg p-3">
              <div className="flex items-center justify-between">
                <span className="flex items-center gap-1.5 font-medium text-fg">
                  <Activity
                    size={15}
                    className="text-fg-muted"
                    aria-hidden="true"
                  />
                  Memory
                </span>
                <span
                  data-testid="mem-pct"
                  className="text-[15px] font-semibold tabular-nums"
                >
                  {current.memPct.toFixed(1)}%
                  <span className="ml-1.5 text-[12px] font-normal text-fg-muted">
                    ({formatMachineBytes(current.memUsedBytes)} /{' '}
                    {formatMachineBytes(current.memTotalBytes)})
                  </span>
                </span>
              </div>
              <MachineSparkChart
                label={`Memory usage (${range})`}
                values={memSeries}
                thresholdPct={memThreshold}
              />
            </div>

            {/* Load Average Card */}
            <div className="flex items-center justify-between rounded-control border border-border bg-bg p-3">
              <span className="font-medium text-fg">
                Load Average (1m / 5m / 15m)
              </span>
              <span
                data-testid="load-avg"
                className="font-mono text-[13px] tabular-nums text-fg"
              >
                {current.load1.toFixed(2)} / {current.load5.toFixed(2)} /{' '}
                {current.load15.toFixed(2)}
              </span>
            </div>

            {/* Network Card */}
            <div className="flex items-center justify-between rounded-control border border-border bg-bg p-3">
              <span className="flex items-center gap-1.5 font-medium text-fg">
                <Wifi size={15} className="text-fg-muted" aria-hidden="true" />
                Network (RX / TX)
              </span>
              <span
                data-testid="net-rates"
                className="font-mono text-[13px] tabular-nums text-fg"
              >
                ↓ {formatMachineRate(current.rxBytesPerSec)} · ↑{' '}
                {formatMachineRate(current.txBytesPerSec)}
              </span>
            </div>
          </div>

          {/* Fullest Disks */}
          <div className="flex flex-col gap-2 rounded-control border border-border bg-bg p-3">
            <div className="flex items-center gap-1.5 font-medium text-fg">
              <HardDrive
                size={15}
                className="text-fg-muted"
                aria-hidden="true"
              />
              <span>Fullest Disks</span>
            </div>
            {current.disks.length === 0 ? (
              <p className="text-[12px] text-fg-muted">
                No physical disks reported.
              </p>
            ) : (
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                {current.disks.map((d) => (
                  <div
                    key={d.mount}
                    className="flex flex-col gap-1 rounded-control border border-border/70 bg-surface px-2.5 py-2"
                  >
                    <div className="flex items-center justify-between gap-2 text-[12px]">
                      <span className="truncate font-mono font-medium">
                        {d.mount}
                      </span>
                      <span className="shrink-0 tabular-nums text-fg-muted">
                        {d.usedPct.toFixed(1)}% (
                        {formatMachineBytes(d.usedBytes)} /{' '}
                        {formatMachineBytes(d.totalBytes)})
                      </span>
                    </div>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-border">
                      <div
                        className={`h-full rounded-full ${
                          d.usedPct >= 90
                            ? 'bg-danger'
                            : d.usedPct >= 75
                              ? 'bg-warning'
                              : 'bg-accent'
                        }`}
                        style={{
                          width: `${Math.max(0, Math.min(100, d.usedPct))}%`,
                        }}
                      />
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* Sustained High-Load Alert Rules */}
          <div className="flex flex-col gap-2.5 rounded-control border border-border bg-bg p-3">
            <div className="flex items-center justify-between gap-2">
              <div>
                <h4 className="font-semibold text-fg">
                  Sustained High-Load Alert Rules
                </h4>
                <p className="text-[12px] text-fg-muted">
                  Notifies via Web Push / Telegram when resource usage stays at
                  or above the threshold for the full duration window (resets
                  once usage drops 5% below threshold).
                </p>
              </div>
              {!readOnly && draftAlerts.length < 16 && (
                <Button size="sm" onClick={addRule}>
                  <Plus size={14} aria-hidden="true" />
                  Add rule
                </Button>
              )}
            </div>

            <div className="flex flex-col gap-2">
              {draftAlerts.map((rule, idx) => (
                <div
                  key={rule.id}
                  data-testid={`alert-rule-${rule.id}`}
                  className="flex flex-wrap items-center gap-2 rounded-control border border-border/70 bg-surface px-2.5 py-2 text-[12px]"
                >
                  <label className="flex items-center gap-1.5">
                    <input
                      type="checkbox"
                      checked={rule.enabled}
                      disabled={readOnly}
                      onChange={(e) =>
                        updateRule(idx, { enabled: e.target.checked })
                      }
                      aria-label={`Enable ${rule.metric} alert`}
                    />
                    <span className="sr-only">Enabled</span>
                  </label>

                  <select
                    aria-label={`Metric for ${rule.id}`}
                    value={rule.metric}
                    disabled={readOnly}
                    onChange={(e) =>
                      updateRule(idx, {
                        metric: e.target.value as MachineAlertRule['metric'],
                      })
                    }
                    className="rounded-control border border-border bg-bg px-2 py-1 text-[12px] text-fg"
                  >
                    <option value="cpu">CPU</option>
                    <option value="memory">Memory</option>
                    <option value="disk">Disk</option>
                  </select>

                  <span className="text-fg-muted">&ge;</span>

                  <label className="flex items-center gap-1">
                    <input
                      type="number"
                      min={50}
                      max={99}
                      aria-label={`Threshold percent for ${rule.id}`}
                      value={rule.thresholdPct}
                      disabled={readOnly}
                      onChange={(e) =>
                        updateRule(idx, {
                          thresholdPct: Math.max(
                            50,
                            Math.min(99, Number(e.target.value) || 50),
                          ),
                        })
                      }
                      className="w-16 rounded-control border border-border bg-bg px-2 py-1 tabular-nums text-fg"
                    />
                    <span className="text-fg-muted">% for</span>
                  </label>

                  <label className="flex items-center gap-1">
                    <input
                      type="number"
                      min={5}
                      max={120}
                      aria-label={`Duration minutes for ${rule.id}`}
                      value={rule.durationMin}
                      disabled={readOnly}
                      onChange={(e) =>
                        updateRule(idx, {
                          durationMin: Math.max(
                            5,
                            Math.min(120, Number(e.target.value) || 5),
                          ),
                        })
                      }
                      className="w-16 rounded-control border border-border bg-bg px-2 py-1 tabular-nums text-fg"
                    />
                    <span className="text-fg-muted">min</span>
                  </label>

                  {rule.firing && (
                    <span className="inline-flex items-center gap-1 rounded-full bg-danger/15 px-2 py-0.5 text-[11px] font-semibold text-danger">
                      <AlertTriangle size={12} aria-hidden="true" />
                      Firing
                    </span>
                  )}

                  {!readOnly && (
                    <div className="ml-auto">
                      <IconButton
                        size="sm"
                        onClick={() => removeRule(idx)}
                        aria-label={`Remove alert rule ${rule.id}`}
                        title="Remove rule"
                      >
                        <Trash2 size={14} aria-hidden="true" />
                      </IconButton>
                    </div>
                  )}
                </div>
              ))}
            </div>

            {!readOnly && (
              <div className="flex items-center justify-end gap-2 pt-1">
                {saveMessage && (
                  <span role="status" className="text-[12px] text-fg-muted">
                    {saveMessage}
                  </span>
                )}
                <Button
                  variant="primary"
                  size="sm"
                  disabled={!dirtyAlerts || savingAlerts}
                  onClick={() => void handleSaveAlerts()}
                >
                  {savingAlerts ? 'Saving…' : 'Save alert rules'}
                </Button>
              </div>
            )}
          </div>
        </>
      ) : null}
    </section>
  )
}
