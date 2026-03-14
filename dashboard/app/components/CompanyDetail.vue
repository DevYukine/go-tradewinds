<script setup lang="ts">
import type { Company, PnLPoint } from '~/types'

const props = defineProps<{
  company: Company
  latestPnL: PnLPoint | null
}>()

function statusLabel(status: Company['status']): string {
  return status.charAt(0).toUpperCase() + status.slice(1)
}

function statusClasses(status: Company['status']): string {
  switch (status) {
    case 'running': return 'bg-emerald-500/20 text-emerald-400 border-emerald-500/30'
    case 'paused': return 'bg-yellow-500/20 text-yellow-400 border-yellow-500/30'
    case 'error': return 'bg-rose-500/20 text-rose-400 border-rose-500/30'
    case 'bankrupt': return 'bg-gray-500/20 text-gray-400 border-gray-500/30'
    default: return 'bg-gray-500/20 text-gray-400 border-gray-500/30'
  }
}

function strategyBadgeClasses(strategy: string): string {
  const colors: Record<string, string> = {
    arbitrage: 'bg-emerald-500/20 text-emerald-300',
    bulk_hauler: 'bg-blue-500/20 text-blue-300',
    market_maker: 'bg-amber-500/20 text-amber-300',
  }
  return colors[strategy.toLowerCase()] || 'bg-purple-500/20 text-purple-300'
}

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US', {
    style: 'decimal',
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(value)
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-start justify-between mb-4">
      <div>
        <div class="flex items-center gap-3">
          <h2 class="text-xl font-bold text-slate-100">{{ company.name }}</h2>
          <span
            class="px-2.5 py-0.5 rounded-full text-xs font-medium border"
            :class="statusClasses(company.status)"
          >
            {{ statusLabel(company.status) }}
          </span>
        </div>
        <div class="flex items-center gap-2 mt-1">
          <span class="font-mono text-sm text-slate-400">{{ company.ticker }}</span>
          <span class="text-slate-600">|</span>
          <span
            class="px-2 py-0.5 rounded-full text-xs font-medium"
            :class="strategyBadgeClasses(company.strategy)"
          >
            {{ company.strategy }}
          </span>
        </div>
      </div>
      <span class="text-xs text-slate-500 font-mono">ID: {{ company.game_id }}</span>
    </div>

    <div class="grid grid-cols-2 lg:grid-cols-6 gap-4">
      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:coins" class="text-amber-400" />
          Treasury
        </div>
        <div class="text-lg font-bold text-slate-100 font-mono">
          {{ formatCurrency(company.treasury) }}
        </div>
      </div>

      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="mdi:ship" class="text-blue-400" />
          Ships
        </div>
        <div class="text-lg font-bold text-slate-100 font-mono">
          {{ latestPnL?.ship_count ?? '---' }}
        </div>
      </div>

      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:trending-up" class="text-emerald-400" />
          Net P&L
        </div>
        <div
          class="text-lg font-bold font-mono"
          :class="(latestPnL?.net_pnl ?? 0) >= 0 ? 'text-emerald-400' : 'text-rose-400'"
        >
          {{ latestPnL ? formatCurrency(latestPnL.net_pnl) : '---' }}
        </div>
      </div>

      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:arrow-up-right" class="text-emerald-400" />
          Revenue
        </div>
        <div class="text-lg font-bold text-emerald-400 font-mono">
          {{ latestPnL ? formatCurrency(latestPnL.total_rev) : '---' }}
        </div>
      </div>

      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:arrow-down-right" class="text-rose-400" />
          Costs
        </div>
        <div class="text-lg font-bold text-rose-400 font-mono">
          {{ latestPnL ? formatCurrency(latestPnL.total_costs) : '---' }}
        </div>
      </div>

      <div class="bg-slate-900/50 rounded-lg p-3">
        <div class="flex items-center gap-2 text-slate-400 text-xs mb-1">
          <Icon name="lucide:star" class="text-yellow-400" />
          Reputation
        </div>
        <div class="text-lg font-bold text-slate-100 font-mono">
          {{ company.reputation }}
        </div>
      </div>
    </div>
  </div>
</template>
