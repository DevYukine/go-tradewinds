<script setup lang="ts">
import type { Company } from '~/types'

const props = defineProps<{
  companiesByStrategy: Record<string, Company[]>
  selectedCompany: Company | null
}>()

const emit = defineEmits<{
  select: [company: Company]
}>()

const strategyColors: Record<string, string> = {
  arbitrage: 'text-emerald-400 border-emerald-500',
  bulk_hauler: 'text-blue-400 border-blue-500',
  market_maker: 'text-amber-400 border-amber-500',
  default: 'text-purple-400 border-purple-500',
}

function getStrategyColor(strategy: string): string {
  return strategyColors[strategy.toLowerCase()] || strategyColors.default
}

function statusColor(status: Company['status']): string {
  switch (status) {
    case 'running': return 'bg-emerald-500'
    case 'paused': return 'bg-yellow-500'
    case 'error': return 'bg-rose-500'
    case 'bankrupt': return 'bg-gray-500'
    default: return 'bg-gray-500'
  }
}

function formatTreasury(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toFixed(0)
}
</script>

<template>
  <aside class="w-72 bg-slate-900 border-r border-slate-700 h-screen overflow-y-auto flex flex-col">
    <div class="p-4 border-b border-slate-700">
      <div class="flex items-center gap-2">
        <Icon name="mdi:sail-boat" class="text-emerald-400 text-2xl" />
        <h1 class="text-lg font-bold text-slate-100">Tradewinds</h1>
      </div>
      <p class="text-xs text-slate-500 mt-1">Trading Bot Dashboard</p>
    </div>

    <div class="flex-1 overflow-y-auto p-2">
      <div
        v-for="(companies, strategy) in companiesByStrategy"
        :key="strategy"
        class="mb-4"
      >
        <div
          class="px-3 py-1.5 text-xs font-semibold uppercase tracking-wider border-l-2 mb-1"
          :class="getStrategyColor(strategy as string)"
        >
          {{ strategy }}
        </div>

        <button
          v-for="company in companies"
          :key="company.id"
          class="w-full flex items-center gap-3 px-3 py-2 rounded-md text-left transition-colors"
          :class="
            selectedCompany?.id === company.id
              ? 'bg-slate-700 text-slate-100'
              : 'text-slate-400 hover:bg-slate-800 hover:text-slate-200'
          "
          @click="emit('select', company)"
        >
          <span
            class="w-2 h-2 rounded-full flex-shrink-0"
            :class="statusColor(company.status)"
          />
          <span class="font-mono text-sm font-medium flex-shrink-0">{{ company.ticker }}</span>
          <span class="text-xs text-slate-500 truncate flex-1">{{ company.name }}</span>
          <span class="text-xs font-mono text-slate-500">{{ formatTreasury(company.treasury) }}</span>
        </button>
      </div>

      <div
        v-if="Object.keys(companiesByStrategy).length === 0"
        class="text-center text-slate-600 text-sm py-8"
      >
        <Icon name="mdi:loading" class="animate-spin text-2xl mb-2" />
        <p>Loading companies...</p>
      </div>
    </div>
  </aside>
</template>
