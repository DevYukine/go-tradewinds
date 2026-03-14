<script setup lang="ts">
const { companies, selectedCompany, companiesByStrategy, fetchCompanies, selectCompany, clearSelection } = useCompanies()
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
          <button
            v-if="selectedCompany"
            class="text-slate-400 hover:text-slate-200 transition-colors"
            @click="clearSelection"
            title="Back to overview"
          >
            <Icon name="lucide:arrow-left" />
          </button>
          <h1 class="text-lg font-bold text-slate-100">
            {{ selectedCompany ? selectedCompany.name : 'Dashboard' }}
          </h1>
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
        <!-- Landing overview when no company selected -->
        <OverviewLanding
          v-if="!selectedCompany"
          :companies="companies"
          :companies-by-strategy="companiesByStrategy"
          @select="selectCompany"
        />

        <!-- Company selected -->
        <template v-else>
          <!-- Company Detail -->
          <CompanyDetail
            :company="selectedCompany"
            :latest-pn-l="latestPnL"
          />

          <!-- Fleet & Inventory + Trade History -->
          <div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
            <FleetOverview :company-id="selectedCompany.id" />
            <TradeHistory :company-id="selectedCompany.id" />
          </div>

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
