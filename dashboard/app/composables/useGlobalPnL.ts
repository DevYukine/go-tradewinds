import type { GlobalPnLResponse } from '~/types'

export function useGlobalPnL() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const data = ref<GlobalPnLResponse | null>(null)
  const loading = ref(false)
  const lastUpdated = ref<number>(0)

  async function fetchGlobalPnL() {
    loading.value = true
    try {
      const resp = await $fetch<GlobalPnLResponse>(
        `${apiBase}/api/global-pnl`,
        { params: { _t: Date.now() } }
      )
      data.value = resp
      lastUpdated.value = Date.now()
    } catch (e: any) {
      console.error('Failed to fetch global PnL:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    data,
    loading,
    lastUpdated,
    fetchGlobalPnL,
  }
}
