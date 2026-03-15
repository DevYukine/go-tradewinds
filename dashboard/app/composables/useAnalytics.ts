import type {
  GoodAnalytics,
  RouteAnalytics,
  TimelineResponse,
  PassengerAnalyticsResponse,
} from '~/types'

export function useAnalytics() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const goods = ref<GoodAnalytics[]>([])
  const routes = ref<RouteAnalytics[]>([])
  const timeline = ref<TimelineResponse | null>(null)
  const passengers = ref<PassengerAnalyticsResponse | null>(null)
  const loading = ref(false)

  async function fetchGoods(hours?: number) {
    loading.value = true
    try {
      const params: Record<string, any> = { _t: Date.now() }
      if (hours) params.hours = hours
      goods.value = await $fetch<GoodAnalytics[]>(
        `${apiBase}/api/analytics/goods`,
        { params }
      )
    } catch (e: any) {
      console.error('Failed to fetch goods analytics:', e)
    } finally {
      loading.value = false
    }
  }

  async function fetchRoutes(hours?: number) {
    loading.value = true
    try {
      const params: Record<string, any> = { _t: Date.now() }
      if (hours) params.hours = hours
      routes.value = await $fetch<RouteAnalytics[]>(
        `${apiBase}/api/analytics/routes`,
        { params }
      )
    } catch (e: any) {
      console.error('Failed to fetch routes analytics:', e)
    } finally {
      loading.value = false
    }
  }

  async function fetchTimeline(groupBy: string = 'good', hours: number = 168) {
    loading.value = true
    try {
      timeline.value = await $fetch<TimelineResponse>(
        `${apiBase}/api/analytics/timeline`,
        { params: { group_by: groupBy, hours, _t: Date.now() } }
      )
    } catch (e: any) {
      console.error('Failed to fetch timeline:', e)
    } finally {
      loading.value = false
    }
  }

  async function fetchPassengers(hours?: number) {
    loading.value = true
    try {
      const params: Record<string, any> = { _t: Date.now() }
      if (hours) params.hours = hours
      passengers.value = await $fetch<PassengerAnalyticsResponse>(
        `${apiBase}/api/analytics/passengers`,
        { params }
      )
    } catch (e: any) {
      console.error('Failed to fetch passengers analytics:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    goods,
    routes,
    timeline,
    passengers,
    loading,
    fetchGoods,
    fetchRoutes,
    fetchTimeline,
    fetchPassengers,
  }
}
