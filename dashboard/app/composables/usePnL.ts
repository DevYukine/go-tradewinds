import type { PnLPoint } from '~/types'

export function usePnL() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const history = ref<PnLPoint[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let eventSource: EventSource | null = null

  async function fetchHistory(companyId: number) {
    loading.value = true
    error.value = null
    try {
      history.value = await $fetch<PnLPoint[]>(`${apiBase}/api/companies/${companyId}/pnl`)
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch P&L history'
      console.error('Failed to fetch P&L history:', e)
    } finally {
      loading.value = false
    }
  }

  function connectSSE(companyId: number) {
    disconnectSSE()
    eventSource = new EventSource(`${apiBase}/api/companies/${companyId}/pnl/stream`)

    eventSource.onmessage = (event) => {
      try {
        const point: PnLPoint = JSON.parse(event.data)
        history.value.push(point)
      } catch (e) {
        console.error('Failed to parse SSE PnL data:', e)
      }
    }

    eventSource.onerror = (e) => {
      console.error('PnL SSE error:', e)
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
