import type { TradeLog } from '~/types'

export function useTrades() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const trades = ref<TradeLog[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function fetchTrades(companyId: number, limit = 50) {
    loading.value = true
    error.value = null
    try {
      trades.value = await $fetch<TradeLog[]>(
        `${apiBase}/api/companies/${companyId}/trades`,
        { params: { limit } }
      )
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
    fetchTrades,
  }
}
