import type { WorldData } from '~/types'

// Shared global state so every component that calls useWorld() sees the same data.
const world = ref<WorldData | null>(null)
const loading = ref(false)
let fetched = false
let pollInterval: ReturnType<typeof setInterval> | null = null

// Poll interval for world data refresh (picks up new ports/routes).
const WORLD_POLL_MS = 60_000

export function useWorld() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  async function fetchWorld() {
    loading.value = true
    try {
      world.value = await $fetch<WorldData>(`${apiBase}/api/world`)
    } catch (e) {
      console.error('Failed to fetch world data:', e)
    } finally {
      loading.value = false
    }
  }

  // Auto-fetch on first use and start periodic refresh.
  if (!fetched) {
    fetched = true
    fetchWorld()
    pollInterval = setInterval(fetchWorld, WORLD_POLL_MS)
  }

  return { world, loading, fetchWorld }
}
