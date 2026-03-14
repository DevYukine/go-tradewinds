import type { CompanyInventory, ShipDetail } from '~/types'

export function useShipPositions(companyId: Ref<number>) {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const shipsByPort = ref<Map<string, ShipDetail[]>>(new Map())
  const shipsInTransit = ref<ShipDetail[]>([])
  const loading = ref(false)
  let pollInterval: ReturnType<typeof setInterval> | null = null

  async function fetchPositions() {
    if (!companyId.value) return
    loading.value = true
    try {
      const inv = await $fetch<CompanyInventory>(`${apiBase}/api/companies/${companyId.value}/inventory`)
      const byPort = new Map<string, ShipDetail[]>()
      const transit: ShipDetail[] = []

      for (const ship of inv.ships) {
        if (ship.status === 'docked' && ship.port_id) {
          const list = byPort.get(ship.port_id) || []
          list.push(ship)
          byPort.set(ship.port_id, list)
        } else if (ship.route_id) {
          transit.push(ship)
        }
      }

      shipsByPort.value = byPort
      shipsInTransit.value = transit
    } catch (e) {
      console.error('Failed to fetch ship positions:', e)
    } finally {
      loading.value = false
    }
  }

  function startPolling() {
    stopPolling()
    fetchPositions()
    pollInterval = setInterval(fetchPositions, 30_000)
  }

  function stopPolling() {
    if (pollInterval) {
      clearInterval(pollInterval)
      pollInterval = null
    }
  }

  watch(companyId, (id) => {
    if (id) startPolling()
    else stopPolling()
  }, { immediate: true })

  onUnmounted(() => {
    stopPolling()
  })

  return {
    shipsByPort,
    shipsInTransit,
    loading,
    fetchPositions,
  }
}
