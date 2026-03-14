import type { GameTradeEntry } from '~/types'

export function useGameTrades() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const gameTrades = ref<GameTradeEntry[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  const lastUpdated = ref<number>(0)

  async function fetchGameTrades(companyId: number, role?: string) {
    loading.value = true
    error.value = null
    try {
      const params: Record<string, unknown> = { _t: Date.now() }
      if (role) params.role = role
      const data = await $fetch<GameTradeEntry[]>(
        `${apiBase}/api/companies/${companyId}/game-trades`,
        { params }
      )
      gameTrades.value = data
      lastUpdated.value = Date.now()
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch game trades'
      console.error('Failed to fetch game trades:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    gameTrades,
    loading,
    error,
    lastUpdated,
    fetchGameTrades,
  }
}
