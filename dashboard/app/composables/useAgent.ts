import type { AgentDecision } from '~/types'

export function useAgent() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const decisions = ref<AgentDecision[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function fetchDecisions(companyId: number) {
    loading.value = true
    error.value = null
    try {
      decisions.value = await $fetch<AgentDecision[]>(
        `${apiBase}/api/companies/${companyId}/agent-decisions`
      )
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch agent decisions'
      console.error('Failed to fetch agent decisions:', e)
    } finally {
      loading.value = false
    }
  }

  return {
    decisions,
    loading,
    error,
    fetchDecisions,
  }
}
