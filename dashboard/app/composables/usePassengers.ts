import type { PassengerLog } from '~/types'

export function usePassengers() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const passengers = ref<PassengerLog[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  const lastUpdated = ref<number>(0)

  async function fetchPassengers(companyId: number, limit = 50) {
    loading.value = true
    error.value = null
    try {
      const data = await $fetch<PassengerLog[]>(
        `${apiBase}/api/companies/${companyId}/passengers`,
        { params: { limit, _t: Date.now() } }
      )
      passengers.value = data
      lastUpdated.value = Date.now()
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch passengers'
      console.error('Failed to fetch passengers:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    passengers,
    loading,
    error,
    lastUpdated,
    fetchPassengers,
  }
}
