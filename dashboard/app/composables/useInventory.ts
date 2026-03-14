import type { CompanyInventory } from '~/types'

export function useInventory() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const inventory = ref<CompanyInventory | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  let pollTimer: ReturnType<typeof setInterval> | null = null

  async function fetchInventory(companyId: number) {
    loading.value = true
    error.value = null
    try {
      inventory.value = await $fetch<CompanyInventory>(
        `${apiBase}/api/companies/${companyId}/inventory`
      )
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch inventory'
      console.error('Failed to fetch inventory:', e)
    } finally {
      loading.value = false
    }
  }

  function startPolling(companyId: number, intervalMs = 10000) {
    stopPolling()
    fetchInventory(companyId)
    pollTimer = setInterval(() => fetchInventory(companyId), intervalMs)
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  }

  onUnmounted(() => stopPolling())

  return {
    inventory,
    loading,
    error,
    fetchInventory,
    startPolling,
    stopPolling,
  }
}
