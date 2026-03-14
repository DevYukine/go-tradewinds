import type { WorldData } from '~/types'

export function useWorld() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const world = ref<WorldData | null>(null)
  const loading = ref(false)

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

  return { world, loading, fetchWorld }
}
