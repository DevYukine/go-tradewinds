import type { StrategyMetric } from '~/types'

// Shared composable for strategy metrics — used by both StrategyComparison and OptimizerLog.
const metrics = ref<StrategyMetric[]>([])
const loading = ref(false)
let pollTimer: ReturnType<typeof setInterval> | null = null
let subscribers = 0

export function useStrategyMetrics() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  async function fetchMetrics() {
    loading.value = true
    try {
      metrics.value = await $fetch<StrategyMetric[]>(`${apiBase}/api/strategy-metrics`)
    } catch (e) {
      console.error('Failed to fetch strategy metrics:', e)
    } finally {
      loading.value = false
    }
  }

  onMounted(() => {
    subscribers++
    if (subscribers === 1) {
      fetchMetrics()
      pollTimer = setInterval(fetchMetrics, 30000)
    }
  })

  onUnmounted(() => {
    subscribers--
    if (subscribers === 0 && pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  })

  return { metrics, loading, fetchMetrics }
}
