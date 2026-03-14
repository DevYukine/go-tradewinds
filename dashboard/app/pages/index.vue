<script setup lang="ts">
const { companies, selectedCompany, companiesByStrategy, fetchCompanies, selectCompany } = useCompanies()
const { history } = usePnL()

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const healthOk = ref(false)

async function checkHealth() {
  try {
    await $fetch(`${apiBase}/api/health`)
    healthOk.value = true
  } catch {
    healthOk.value = false
  }
}

onMounted(async () => {
  await fetchCompanies()
  await checkHealth()
  setInterval(checkHealth, 15000)
})

const latestPnL = computed(() => {
  if (history.value.length === 0) return null
  return history.value[history.value.length - 1]
})
</script>

<template>
  <div class="flex h-screen overflow-hidden">
    <!-- Sidebar -->
    <CompanySidebar
      :companies-by-strategy="companiesByStrategy"
      :selected-company="selectedCompany"
      @select="selectCompany"
    />

    <!-- Main Content -->
    <div class="flex-1 flex flex-col overflow-hidden">
      <!-- Top Bar -->
      <header class="bg-slate-900 border-b border-slate-700 px-6 py-3 flex items-center justify-between flex-shrink-0">
        <div class="flex items-center gap-3">
          <h1 class="text-lg font-bold text-slate-100">Dashboard</h1>
          <div class="flex items-center gap-1.5">
            <span
              class="w-2 h-2 rounded-full"
              :class="healthOk ? 'bg-emerald-500' : 'bg-rose-500'"
            />
            <span class="text-xs text-slate-500">
              {{ healthOk ? 'API Connected' : 'API Disconnected' }}
            </span>
          </div>
        </div>
        <RateLimitGauge />
      </header>

      <!-- Scrollable Content -->
      <main class="flex-1 overflow-y-auto p-6 space-y-6">
        <!-- No company selected state -->
        <div
          v-if="!selectedCompany"
          class="flex flex-col items-center justify-center h-full text-slate-600"
        >
          <Icon name="mdi:sail-boat" class="text-6xl mb-4 text-slate-700" />
          <h2 class="text-xl font-semibold text-slate-500 mb-2">Select a Company</h2>
          <p class="text-sm">Choose a company from the sidebar to view its details</p>
        </div>

        <!-- Company selected -->
        <template v-else>
          <!-- Company Detail -->
          <CompanyDetail
            :company="selectedCompany"
            :latest-pn-l="latestPnL"
          />

          <!-- P&L Chart -->
          <PnLChart :company-id="selectedCompany.id" />

          <!-- Agent Decisions -->
          <AgentDecisions :company-id="selectedCompany.id" />

          <!-- Live Logs -->
          <LiveLogs :company-id="selectedCompany.id" />

          <!-- Bottom row: Strategy Comparison + Optimizer Log -->
          <div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
            <StrategyComparison />
            <OptimizerLog />
          </div>
        </template>
      </main>
    </div>
  </div>
</template>
