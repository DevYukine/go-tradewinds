import type { TradeLog } from '~/types'

export function useTrades() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const trades = ref<TradeLog[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  const lastUpdated = ref<number>(0)

  async function fetchTrades(companyId: number, limit = 50) {
    loading.value = true
    error.value = null
    try {
      const data = await $fetch<TradeLog[]>(
        `${apiBase}/api/companies/${companyId}/trades`,
        { params: { limit, _t: Date.now() } }
      )
      trades.value = data
      lastUpdated.value = Date.now()
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch trades'
      console.error('Failed to fetch trades:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    trades,
    loading,
    error,
    lastUpdated,
    fetchTrades,
  }
}
