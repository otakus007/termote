import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { MachineStatusResponse } from '../hooks/use-mux-api'

const { mockFetchMachineStatus, mockUpdateMachineAlerts } = vi.hoisted(() => ({
  mockFetchMachineStatus: vi.fn(),
  mockUpdateMachineAlerts: vi.fn(),
}))

vi.mock('../hooks/use-mux-api', async (orig) => ({
  ...(await orig<typeof import('../hooks/use-mux-api')>()),
  fetchMachineStatus: (
    range?: '1h' | '24h',
    signal?: AbortSignal,
    machineId?: string,
  ) =>
    machineId
      ? mockFetchMachineStatus(range, signal, machineId)
      : mockFetchMachineStatus(range, signal),
  updateMachineAlerts: (alerts: unknown) => mockUpdateMachineAlerts(alerts),
}))

import { HostResourcePill, MachinesView } from './machines-view'

const SAMPLE_STATUS: MachineStatusResponse = {
  host: {
    id: 'local',
    name: 'devbox',
    os: 'linux',
    arch: 'amd64',
    cpus: 16,
    uptimeSec: 90000,
  },
  current: {
    ts: '2026-10-16T12:00:00Z',
    cpuPct: 34.2,
    memUsedBytes: 12 * 1024 * 1024 * 1024,
    memTotalBytes: 32 * 1024 * 1024 * 1024,
    memPct: 37.5,
    load1: 2.15,
    load5: 1.8,
    load15: 1.45,
    rxBytesPerSec: 102400,
    txBytesPerSec: 20480,
    disks: [
      {
        mount: '/',
        usedBytes: 200 * 1024 * 1024 * 1024,
        totalBytes: 500 * 1024 * 1024 * 1024,
        usedPct: 40.0,
      },
    ],
  },
  samples: [
    {
      ts: '2026-10-16T11:59:00Z',
      cpuPct: 28.0,
      memUsedBytes: 12 * 1024 * 1024 * 1024,
      memTotalBytes: 32 * 1024 * 1024 * 1024,
      memPct: 37.5,
      load1: 1.9,
      load5: 1.7,
      load15: 1.4,
      rxBytesPerSec: 51200,
      txBytesPerSec: 10240,
      disks: [
        {
          mount: '/',
          usedBytes: 200 * 1024 * 1024 * 1024,
          totalBytes: 500 * 1024 * 1024 * 1024,
          usedPct: 40.0,
        },
      ],
    },
    {
      ts: '2026-10-16T12:00:00Z',
      cpuPct: 34.2,
      memUsedBytes: 12 * 1024 * 1024 * 1024,
      memTotalBytes: 32 * 1024 * 1024 * 1024,
      memPct: 37.5,
      load1: 2.15,
      load5: 1.8,
      load15: 1.45,
      rxBytesPerSec: 102400,
      txBytesPerSec: 20480,
      disks: [
        {
          mount: '/',
          usedBytes: 200 * 1024 * 1024 * 1024,
          totalBytes: 500 * 1024 * 1024 * 1024,
          usedPct: 40.0,
        },
      ],
    },
  ],
  alerts: [
    {
      id: 'cpu-high',
      metric: 'cpu',
      thresholdPct: 90,
      durationMin: 15,
      enabled: true,
      firing: true,
    },
  ],
}

describe('MachinesView & HostResourcePill', () => {
  beforeEach(() => {
    mockFetchMachineStatus.mockReset()
    mockUpdateMachineAlerts.mockReset()
    mockFetchMachineStatus.mockResolvedValue(SAMPLE_STATUS)
    mockUpdateMachineAlerts.mockImplementation(async (alerts) => alerts)
  })

  it('renders HostResourcePill summary and active alert badge', () => {
    const onClick = vi.fn()
    render(<HostResourcePill status={SAMPLE_STATUS} onClick={onClick} />)
    expect(screen.getByText(/CPU 34% · Mem 38% · Disk 40%/)).toBeInTheDocument()
    expect(screen.getByTestId('host-alert-badge')).toHaveTextContent('1')
    fireEvent.click(screen.getByRole('button', { name: /Host resources/ }))
    expect(onClick).toHaveBeenCalledOnce()
  })

  it('renders gauges, switches between 1h and 24h range, and edits alert rules', async () => {
    render(<MachinesView />)

    await waitFor(() => {
      expect(screen.getByTestId('cpu-pct')).toHaveTextContent('34.2%')
    })
    expect(screen.getByTestId('mem-pct')).toHaveTextContent('37.5%')
    expect(screen.getByTestId('load-avg')).toHaveTextContent(
      '2.15 / 1.80 / 1.45',
    )
    expect(screen.getByText('/')).toBeInTheDocument()
    expect(screen.getByText('Firing')).toBeInTheDocument()
    expect(screen.queryByTestId('machines-crew-grid')).toBeNull()

    // Switch to 24h range
    fireEvent.click(screen.getByRole('button', { name: '24h' }))
    await waitFor(() => {
      expect(mockFetchMachineStatus).toHaveBeenCalledWith(
        '24h',
        expect.anything(),
      )
    })

    // Edit threshold and save alert rules
    const thresholdInput = screen.getByLabelText(
      'Threshold percent for cpu-high',
    )
    fireEvent.change(thresholdInput, { target: { value: '85' } })
    const saveBtn = screen.getByRole('button', { name: 'Save alert rules' })
    expect(saveBtn).not.toBeDisabled()
    fireEvent.click(saveBtn)

    await waitFor(() => {
      expect(mockUpdateMachineAlerts).toHaveBeenCalledWith([
        expect.objectContaining({
          id: 'cpu-high',
          metric: 'cpu',
          thresholdPct: 85,
          durationMin: 15,
          enabled: true,
        }),
      ])
    })
  })

  it('renders multi-host crew summary grid and switches selected machine', async () => {
    mockFetchMachineStatus.mockResolvedValue({
      ...SAMPLE_STATUS,
      machines: [
        {
          id: 'local',
          name: 'devbox',
          role: 'lead',
          status: 'online',
          latencyMs: 0,
          host: SAMPLE_STATUS.host,
          current: SAMPLE_STATUS.current,
          samples: SAMPLE_STATUS.samples,
          alerts: SAMPLE_STATUS.alerts,
        },
        {
          id: 'gpu-1',
          name: 'gpu-box',
          role: 'deputy',
          status: 'online',
          latencyMs: 4,
          host: {
            id: 'gpu-1',
            name: 'gpu-box',
            os: 'linux',
            arch: 'amd64',
            cpus: 32,
            uptimeSec: 120000,
          },
          current: {
            ...SAMPLE_STATUS.current,
            cpuPct: 78.4,
            memPct: 62.0,
          },
          samples: SAMPLE_STATUS.samples,
        },
        {
          id: 'nas-1',
          name: 'nas-box',
          role: 'member',
          status: 'unreachable',
          latencyMs: 0,
        },
      ],
    })

    render(<MachinesView />)

    await waitFor(() => {
      expect(screen.getByTestId('machines-crew-grid')).toBeInTheDocument()
    })
    expect(screen.getByText('Lead')).toBeInTheDocument()
    expect(screen.getByText('Standby Deputy')).toBeInTheDocument()
    expect(screen.getByText('Member')).toBeInTheDocument()
    expect(screen.getAllByText('Unreachable').length).toBeGreaterThan(0)

    fireEvent.click(screen.getByTestId('machine-node-gpu-1'))
    await waitFor(() => {
      expect(mockFetchMachineStatus).toHaveBeenCalledWith(
        '1h',
        expect.anything(),
        'gpu-1',
      )
    })
    expect(screen.getByTestId('cpu-pct')).toHaveTextContent('78.4%')
  })
})
