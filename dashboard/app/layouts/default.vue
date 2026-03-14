<script setup lang="ts">
import type { RateLimitStatus } from '~/types'

const route = useRoute()
const config = useRuntimeConfig()
const apiBase = config.public.apiBase

const healthOk = ref(false)
const health = ref<{ uptime_seconds: number; company_count: number; agent_type: string } | null>(null)
const rateLimit = ref<RateLimitStatus | null>(null)

async function fetchHealth() {
  try {
    health.value = await $fetch(`${apiBase}/api/health`)
    healthOk.value = true
  } catch {
    healthOk.value = false
  }
}

async function fetchRateLimit() {
  try {
    rateLimit.value = await $fetch<RateLimitStatus>(`${apiBase}/api/ratelimit`)
  } catch { /* ignore */ }
}

let healthInterval: ReturnType<typeof setInterval>
let rateLimitInterval: ReturnType<typeof setInterval>

onMounted(() => {
  fetchHealth()
  fetchRateLimit()
  healthInterval = setInterval(fetchHealth, 15000)
  rateLimitInterval = setInterval(fetchRateLimit, 15000)
})

onUnmounted(() => {
  clearInterval(healthInterval)
  clearInterval(rateLimitInterval)
})

const utilization = computed(() => rateLimit.value?.current_utilization ?? 0)
const gaugeColor = computed(() => {
  const pct = utilization.value * 100
  if (pct > 80) return '#f43f5e'
  if (pct > 60) return '#eab308'
  return '#10b981'
})

function formatUptime(seconds: number): string {
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

const navItems = [
  { to: '/', label: 'Overview', icon: 'lucide:layout-dashboard' },
  { to: '/market', label: 'Market', icon: 'lucide:bar-chart-3' },
  { to: '/world', label: 'World', icon: 'lucide:globe' },
]
</script>

<template>
  <div class="flex flex-col min-h-screen bg-slate-950 text-slate-100">
    <!-- Header -->
    <header class="bg-slate-900 border-b border-slate-700/50 sticky top-0 z-50">
      <div class="max-w-[2400px] mx-auto px-6 h-14 flex items-center justify-between">
        <!-- Left: Logo + Nav -->
        <div class="flex items-center gap-6">
          <NuxtLink to="/" class="flex items-center gap-2 hover:opacity-80 transition-opacity">
            <Icon name="mdi:sail-boat" class="text-emerald-400 text-2xl" />
            <span class="text-lg font-bold text-slate-100 hidden sm:inline">Tradewinds</span>
          </NuxtLink>

          <nav class="flex items-center gap-1">
            <NuxtLink
              v-for="item in navItems"
              :key="item.to"
              :to="item.to"
              class="flex items-center gap-1.5 px-3 py-1.5 rounded-md text-sm font-medium transition-colors"
              :class="route.path === item.to || (item.to !== '/' && route.path.startsWith(item.to))
                ? 'bg-slate-700/50 text-slate-100'
                : 'text-slate-400 hover:text-slate-200 hover:bg-slate-800'"
            >
              <Icon :name="item.icon" class="text-sm" />
              {{ item.label }}
            </NuxtLink>
          </nav>
        </div>

        <!-- Right: Status + Rate Limit -->
        <div class="flex items-center gap-4">
          <div class="flex items-center gap-1.5">
            <span
              class="w-2 h-2 rounded-full"
              :class="healthOk ? 'bg-emerald-500 animate-pulse' : 'bg-rose-500'"
            />
            <span class="text-xs text-slate-500 hidden sm:inline">
              {{ healthOk ? 'Connected' : 'Disconnected' }}
            </span>
          </div>

          <!-- Rate Limit Gauge -->
          <div v-if="rateLimit" class="flex items-center gap-2">
            <div class="relative w-9 h-9">
              <svg class="w-9 h-9 -rotate-90" viewBox="0 0 80 80">
                <circle cx="40" cy="40" r="34" fill="none" stroke="#1e293b" stroke-width="5" />
                <circle
                  cx="40" cy="40" r="34" fill="none"
                  :stroke="gaugeColor" stroke-width="5" stroke-linecap="round"
                  :stroke-dasharray="`${utilization * 213.6} ${213.6 - utilization * 213.6}`"
                />
              </svg>
              <div class="absolute inset-0 flex items-center justify-center">
                <span class="text-[9px] font-bold font-mono" :style="{ color: gaugeColor }">
                  {{ Math.round(utilization * 100) }}
                </span>
              </div>
            </div>
            <div class="text-[10px] text-slate-500 hidden lg:block leading-tight">
              <div class="font-mono">{{ rateLimit.used }}/{{ rateLimit.max_per_minute }}</div>
              <div>req/min</div>
            </div>
          </div>
        </div>
      </div>
    </header>

    <!-- Main Content -->
    <main class="flex-1">
      <div class="max-w-[2400px] mx-auto px-6 py-6">
        <slot />
      </div>
    </main>

    <!-- Footer -->
    <footer class="bg-slate-900/50 border-t border-slate-700/30 mt-auto">
      <div class="max-w-[2400px] mx-auto px-6 py-3 flex items-center justify-between text-xs text-slate-600">
        <div class="flex items-center gap-4">
          <span v-if="health">
            <Icon name="lucide:bot" class="inline text-slate-500" />
            {{ health.agent_type }} agent
          </span>
          <span v-if="health">
            {{ health.company_count }} {{ health.company_count === 1 ? 'company' : 'companies' }} active
          </span>
          <span v-if="health">
            Uptime: {{ formatUptime(health.uptime_seconds) }}
          </span>
        </div>
        <div class="flex items-center gap-1">
          <Icon name="mdi:sail-boat" class="text-emerald-500/50" />
          <span>Tradewinds Bot Dashboard</span>
        </div>
      </div>
    </footer>
  </div>
</template>
