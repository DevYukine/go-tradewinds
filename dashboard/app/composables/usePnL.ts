import type { PnLPoint } from '~/types'

const MAX_HISTORY = 500

export function usePnL() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const history = ref<PnLPoint[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let eventSource: EventSource | null = null
  let maxSeenId = 0

  async function fetchHistory(companyId: number) {
    loading.value = true
    error.value = null
    try {
      const since = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString()
      const data = await $fetch<PnLPoint[]>(`${apiBase}/api/companies/${companyId}/pnl?since=${since}`)
      history.value = data.slice(-MAX_HISTORY)
      // Track max ID for SSE dedup
      for (const p of data) {
        if (p.id > maxSeenId) maxSeenId = p.id
      }
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch P&L history'
      console.error('Failed to fetch P&L history:', e)
    } finally {
      loading.value = false
    }
  }

  function connectSSE(companyId: number) {
    disconnectSSE()
    // Pass since_id so backend only sends new snapshots
    eventSource = new EventSource(`${apiBase}/sse/pnl/${companyId}?since_id=${maxSeenId}`)

    eventSource.onmessage = (event) => {
      try {
        const point: PnLPoint = JSON.parse(event.data)
        // Dedup by ID
        if (point.id <= maxSeenId) return
        maxSeenId = point.id
        history.value.push(point)
        // Cap history size
        if (history.value.length > MAX_HISTORY) {
          history.value = history.value.slice(-MAX_HISTORY)
        }
      } catch (e) {
        console.error('Failed to parse SSE PnL data:', e)
      }
    }

    eventSource.onerror = () => {
      error.value = 'SSE connection lost'
    }
  }

  function disconnectSSE() {
    if (eventSource) {
      eventSource.close()
      eventSource = null
    }
  }

  onUnmounted(() => {
    disconnectSSE()
  })

  return {
    history,
    loading,
    error,
    fetchHistory,
    connectSSE,
    disconnectSSE,
  }
}
