<script setup lang="ts">
import type { RateLimitStatus } from '~/types'

const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const status = ref<RateLimitStatus | null>(null)
let pollInterval: ReturnType<typeof setInterval> | null = null

async function fetchStatus() {
  try {
    status.value = await $fetch<RateLimitStatus>(`${apiBase}/api/ratelimit`)
  } catch (e) {
    console.error('Failed to fetch rate limit status:', e)
  }
}

onMounted(() => {
  fetchStatus()
  pollInterval = setInterval(fetchStatus, 5000)
})

onUnmounted(() => {
  if (pollInterval) {
    clearInterval(pollInterval)
  }
})

const utilization = computed(() => status.value?.current_utilization ?? 0)

const gaugeColor = computed(() => {
  const pct = utilization.value * 100
  if (pct > 80) return '#f43f5e'
  if (pct > 60) return '#eab308'
  return '#10b981'
})

const gaugeTrailColor = '#1e293b'

const circumference = 2 * Math.PI * 36
const strokeDasharray = computed(() => {
  const progress = utilization.value * circumference
  return `${progress} ${circumference - progress}`
})
</script>

<template>
  <div class="flex items-center gap-3">
    <div class="relative w-12 h-12">
      <svg class="w-12 h-12 -rotate-90" viewBox="0 0 80 80">
        <circle
          cx="40"
          cy="40"
          r="36"
          fill="none"
          :stroke="gaugeTrailColor"
          stroke-width="6"
        />
        <circle
          cx="40"
          cy="40"
          r="36"
          fill="none"
          :stroke="gaugeColor"
          stroke-width="6"
          stroke-linecap="round"
          :stroke-dasharray="strokeDasharray"
        />
      </svg>
      <div class="absolute inset-0 flex items-center justify-center">
        <span class="text-[10px] font-bold font-mono" :style="{ color: gaugeColor }">
          {{ status ? Math.round(utilization * 100) : '--' }}%
        </span>
      </div>
    </div>
    <div v-if="status" class="text-xs">
      <div class="text-slate-400 font-mono">
        {{ status.used }} / {{ status.max_per_minute }}
      </div>
      <div class="text-slate-600">
        {{ status.active_companies }} active
      </div>
    </div>
  </div>
</template>
