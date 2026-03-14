<script setup lang="ts">
import type { StrategyMetric } from '~/types'

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const metrics = ref<StrategyMetric[]>([])
const loading = ref(false)

async function fetchMetrics() {
  loading.value = true
  try {
    metrics.value = await $fetch<StrategyMetric[]>(`${apiBase}/api/strategy-metrics`)
  } catch (e) {
    console.error('Failed to fetch optimizer data:', e)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  fetchMetrics()
})

function formatDate(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleString('en-US', {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

function scoreColor(profit: number): string {
  if (profit > 0) return 'text-emerald-400'
  if (profit < 0) return 'text-rose-400'
  return 'text-slate-400'
}

function scoreBorderColor(profit: number): string {
  if (profit > 0) return 'border-emerald-500/30'
  if (profit < 0) return 'border-rose-500/30'
  return 'border-slate-600'
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:settings" class="text-orange-400" />
        Optimizer Log
      </h3>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div v-else-if="metrics.length === 0" class="text-center text-slate-600 text-sm py-8">
      No optimizer data available
    </div>

    <div v-else class="space-y-3 max-h-72 overflow-y-auto">
      <div
        v-for="(m, i) in metrics"
        :key="m.strategy_name"
        class="relative pl-6"
      >
        <!-- Timeline line -->
        <div
          v-if="i < metrics.length - 1"
          class="absolute left-2 top-6 w-px h-full bg-slate-700"
        />
        <!-- Timeline dot -->
        <div
          class="absolute left-0.5 top-1.5 w-3 h-3 rounded-full border-2"
          :class="[
            m.total_profit >= 0 ? 'bg-emerald-500/20 border-emerald-500' : 'bg-rose-500/20 border-rose-500'
          ]"
        />

        <div
          class="bg-slate-900/50 rounded-lg border p-3"
          :class="scoreBorderColor(m.total_profit)"
        >
          <div class="flex items-center justify-between mb-1">
            <span class="text-sm font-medium text-slate-200">{{ m.strategy_name }}</span>
            <span
              class="font-mono text-sm font-bold"
              :class="scoreColor(m.total_profit)"
            >
              {{ m.total_profit >= 0 ? '+' : '' }}{{ m.total_profit.toFixed(0) }}
            </span>
          </div>
          <div class="flex items-center gap-3 text-xs text-slate-500">
            <span>{{ m.trades_executed }} trades</span>
            <span>{{ m.company_count }} companies</span>
            <span>Win: {{ (m.win_rate * 100).toFixed(0) }}%</span>
          </div>
          <div class="text-[10px] text-slate-600 mt-1">
            {{ formatDate(m.period_start) }} - {{ formatDate(m.period_end) }}
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
