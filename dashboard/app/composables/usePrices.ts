import type { PriceEntry } from '~/types'

export function usePrices() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const prices = ref<PriceEntry[]>([])
  const loading = ref(false)

  async function fetchPrices() {
    loading.value = true
    try {
      prices.value = await $fetch<PriceEntry[]>(`${apiBase}/api/prices`)
    } catch (e) {
      console.error('Failed to fetch prices:', e)
    } finally {
      loading.value = false
    }
  }

  return { prices, loading, fetchPrices }
}
