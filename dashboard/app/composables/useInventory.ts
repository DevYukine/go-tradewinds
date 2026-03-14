import type { CompanyInventory } from '~/types'

// Shared global state keyed by company id so multiple components
// (FleetOverview, company page stats bar, etc.) share the same data.
const inventories = ref<Record<number, CompanyInventory>>({})
const pollers = new Map<number, { timer: ReturnType<typeof setInterval>; subscribers: number }>()

export function useInventory(companyId?: Ref<number> | number) {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const loading = ref(false)
  const error = ref<string | null>(null)

  // Convenience: inventory for the single company this instance tracks.
  const inventory = computed<CompanyInventory | null>(() => {
    const id = unref(companyId)
    return id ? inventories.value[id] ?? null : null
  })

  async function fetchInventory(id: number) {
    loading.value = true
    error.value = null
    try {
      const data = await $fetch<CompanyInventory>(
        `${apiBase}/api/companies/${id}/inventory`,
        { params: { _t: Date.now() } }
      )
      inventories.value = { ...inventories.value, [id]: data }
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch inventory'
      console.error('Failed to fetch inventory:', e)
    } finally {
      loading.value = false
    }
  }

  function startPolling(id: number, intervalMs = 10000) {
    const existing = pollers.get(id)
    if (existing) {
      existing.subscribers++
      // Already polling — just bump refcount.
      return
    }
    fetchInventory(id)
    const timer = setInterval(() => fetchInventory(id), intervalMs)
    pollers.set(id, { timer, subscribers: 1 })
  }

  function stopPolling(id?: number) {
    const resolvedId = id ?? unref(companyId)
    if (!resolvedId) return
    const entry = pollers.get(resolvedId)
    if (!entry) return
    entry.subscribers--
    if (entry.subscribers <= 0) {
      clearInterval(entry.timer)
      pollers.delete(resolvedId)
    }
  }

  onUnmounted(() => stopPolling())

  return {
    inventory,
    inventories,
    loading,
    error,
    fetchInventory,
    startPolling,
    stopPolling,
  }
}
