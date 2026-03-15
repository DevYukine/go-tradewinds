import type { Company } from '~/types'

// Shared global state so all pages see the same company list.
const companies = ref<Company[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
let pollTimer: ReturnType<typeof setInterval> | null = null
let subscribers = 0

export function useCompanies() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const companiesByStrategy = computed(() => {
    const grouped: Record<string, Company[]> = {}
    for (const company of companies.value) {
      if (!grouped[company.strategy]) {
        grouped[company.strategy] = []
      }
      grouped[company.strategy]!.push(company)
    }
    return grouped
  })

  async function fetchCompanies() {
    loading.value = true
    error.value = null
    try {
      companies.value = await $fetch<Company[]>(`${apiBase}/api/companies`)
    } catch (e: any) {
      error.value = e.message || 'Failed to fetch companies'
    } finally {
      loading.value = false
    }
  }

  function getCompanyById(id: number): Company | undefined {
    return companies.value.find(c => c.id === id)
  }

  onMounted(() => {
    subscribers++
    if (subscribers === 1) {
      fetchCompanies()
      pollTimer = setInterval(fetchCompanies, 10000)
    }
  })

  onUnmounted(() => {
    subscribers--
    if (subscribers === 0 && pollTimer) {
      clearInterval(pollTimer)
      pollTimer = null
    }
  })

  return {
    companies,
    companiesByStrategy,
    loading,
    error,
    fetchCompanies,
    getCompanyById,
  }
}
