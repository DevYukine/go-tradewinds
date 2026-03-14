import type { Company } from '~/types'

export function useCompanies() {
  const config = useRuntimeConfig()
  const apiBase = config.public.apiBase

  const companies = ref<Company[]>([])
  const selectedCompany = ref<Company | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  const companiesByStrategy = computed(() => {
    const grouped: Record<string, Company[]> = {}
    for (const company of companies.value) {
      if (!grouped[company.strategy]) {
        grouped[company.strategy] = []
      }
      grouped[company.strategy].push(company)
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
      console.error('Failed to fetch companies:', e)
    } finally {
      loading.value = false
    }
  }

  function selectCompany(company: Company) {
    selectedCompany.value = company
  }

  return {
    companies,
    selectedCompany,
    companiesByStrategy,
    loading,
    error,
    fetchCompanies,
    selectCompany,
  }
}
