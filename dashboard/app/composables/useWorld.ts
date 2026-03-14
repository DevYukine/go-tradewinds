import type { WorldData } from '~/types'

// Shared global state so every component that calls useWorld() sees the same data.
const world = ref<WorldData | null>(null)
const loading = ref(false)
let fetched = false

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

  // Auto-fetch once on first use.
  if (!fetched) {
    fetched = true
    fetchWorld()
  }

  return { world, loading, fetchWorld }
}
