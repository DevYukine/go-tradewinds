import type { AgentDecision } from '~/types'

export function useMarketOrders() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const orders = ref<AgentDecision[]>([])
  const loading = ref(false)
  const lastUpdated = ref<number>(0)

  async function fetchMarketOrders(companyId: number) {
    loading.value = true
    try {
      const data = await $fetch<AgentDecision[]>(
        `${apiBase}/api/companies/${companyId}/market-orders`,
        { params: { _t: Date.now() } }
      )
      orders.value = data
      lastUpdated.value = Date.now()
    } catch (e: any) {
      console.error('Failed to fetch market orders:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    orders,
    loading,
    lastUpdated,
    fetchMarketOrders,
  }
}
