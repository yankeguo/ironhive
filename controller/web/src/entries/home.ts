// Dashboard entry: polls /controller/v1/pools every few seconds and
// renders pool summary cards plus the pod table. Read-only — the page
// has no interactions and is meant to be embedded into other systems.

interface PoolSummary {
  name: string
  standby: number
  pending: number
  allocated: number
}

interface PodInfo {
  name: string
  pool: string
  phase: string
  ready: boolean
  ip?: string
  deleting?: boolean
  allocated: boolean
  leaseExpires?: string
  createdAt: string
}

interface PoolsResponse {
  pools: PoolSummary[]
  pods: PodInfo[]
}

const poolsEl = document.querySelector<HTMLElement>('#pools')
const podsEl = document.querySelector<HTMLElement>('#pods')
const bannerEl = document.querySelector<HTMLElement>('#banner')

function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`)
}

// fmtDuration renders a length of time in the largest whole unit.
function fmtDuration(secs: number): string {
  secs = Math.max(0, Math.floor(secs))
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.floor(secs / 60)}m`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h`
  return `${Math.floor(secs / 86400)}d`
}

function fmtLease(until?: string): string {
  if (!until) return '—'
  const secs = (Date.parse(until) - Date.now()) / 1000
  return secs <= 0 ? 'expired' : fmtDuration(secs)
}

// statusBadge classifies a pod the same way the pools summary counts it.
function statusBadge(p: PodInfo): string {
  if (p.deleting) return '<span class="text-rose-400">terminating</span>'
  if (p.allocated) return '<span class="text-sky-400">allocated</span>'
  if (p.phase === 'Running' && p.ready) return '<span class="text-emerald-400">standby</span>'
  if (p.phase === 'Succeeded' || p.phase === 'Failed') return '<span class="text-neutral-500">terminated</span>'
  return '<span class="text-amber-400">pending</span>'
}

function render(data: PoolsResponse): void {
  if (poolsEl) {
    poolsEl.innerHTML = data.pools
      .map(
        (p) => `
      <div class="card">
        <div class="card-header flex items-center gap-1.5">
          <span class="icon-[lucide--boxes]" aria-hidden="true"></span> ${esc(p.name)}
        </div>
        <div class="card-body grid grid-cols-3 gap-2 text-center">
          <div>
            <div class="text-2xl font-semibold text-emerald-400">${p.standby}</div>
            <div class="text-xs text-neutral-400">standby</div>
          </div>
          <div>
            <div class="text-2xl font-semibold text-amber-400">${p.pending}</div>
            <div class="text-xs text-neutral-400">pending</div>
          </div>
          <div>
            <div class="text-2xl font-semibold text-sky-400">${p.allocated}</div>
            <div class="text-xs text-neutral-400">allocated</div>
          </div>
        </div>
      </div>`,
      )
      .join('')
  }
  if (podsEl) {
    podsEl.innerHTML =
      data.pods.length === 0
        ? '<tr><td class="px-4 py-3 text-neutral-500" colspan="8">no pods</td></tr>'
        : data.pods
            .map(
              (p) => `
        <tr class="border-b border-neutral-800">
          <td class="px-4 py-2 font-mono text-xs">${esc(p.name)}</td>
          <td class="px-4 py-2">${esc(p.pool)}</td>
          <td class="px-4 py-2">${esc(p.phase || 'Pending')}</td>
          <td class="px-4 py-2">${p.ready ? '<span class="text-emerald-400">yes</span>' : '<span class="text-neutral-500">no</span>'}</td>
          <td class="px-4 py-2 font-mono text-xs">${esc(p.ip || '—')}</td>
          <td class="px-4 py-2">${statusBadge(p)}</td>
          <td class="px-4 py-2">${fmtLease(p.leaseExpires)}</td>
          <td class="px-4 py-2">${fmtDuration((Date.now() - Date.parse(p.createdAt)) / 1000)}</td>
        </tr>`,
            )
            .join('')
  }
}

function showBanner(message?: string): void {
  if (!bannerEl) return
  if (message === undefined) {
    bannerEl.classList.add('hidden')
  } else {
    bannerEl.textContent = message
    bannerEl.classList.remove('hidden')
  }
}

let refreshInFlight = false

async function refresh(): Promise<void> {
  if (refreshInFlight) return
  refreshInFlight = true
  try {
    const resp = await fetch('/controller/v1/pools', { cache: 'no-store' })
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    render((await resp.json()) as PoolsResponse)
    showBanner()
  } catch (e) {
    showBanner(`cluster state unavailable: ${e instanceof Error ? e.message : e}`)
  } finally {
    refreshInFlight = false
  }
}

void refresh()
setInterval(() => void refresh(), 3000)

export {}
