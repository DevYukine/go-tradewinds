<script setup lang="ts">
import type { GoodAnalytics, RouteAnalytics, TimelineSeries } from '~/types'

const { goods, routes, timeline, passengers, loading, fetchGoods, fetchRoutes, fetchTimeline, fetchPassengers } = useAnalytics()

// Time filter.
const timeFilter = ref<number | undefined>(undefined) // undefined = all time
const timeOptions = [
  { label: 'All Time', value: undefined },
  { label: '1h', value: 1 },
  { label: '6h', value: 6 },
  { label: '24h', value: 24 },
  { label: '3d', value: 72 },
  { label: '7d', value: 168 },
  { label: '30d', value: 720 },
]

// Active tab.
const activeTab = ref<'goods' | 'routes' | 'timeline' | 'passengers'>('goods')

// Timeline controls.
const timelineGroupBy = ref<'good' | 'route' | 'strategy'>('good')
const timelineHours = ref(168)

// Initial fetch.
onMounted(() => {
  fetchGoods(timeFilter.value)
  fetchRoutes(timeFilter.value)
  fetchTimeline(timelineGroupBy.value, timelineHours.value)
  fetchPassengers(timeFilter.value)
})

// Re-fetch on filter change.
watch(timeFilter, (h) => {
  fetchGoods(h)
  fetchRoutes(h)
  fetchPassengers(h)
})

watch([timelineGroupBy, timelineHours], ([g, h]) => {
  fetchTimeline(g, h)
})

function formatCurrency(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  if (value <= -1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value <= -1_000) return `${(value / 1_000).toFixed(1)}K`
  return new Intl.NumberFormat('en-US').format(value)
}

function formatFull(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

function formatPct(value: number): string {
  return `${(value * 100).toFixed(1)}%`
}

function profitColor(value: number): string {
  if (value > 0) return 'text-emerald-400'
  if (value < 0) return 'text-rose-400'
  return 'text-slate-400'
}

function timeAgo(iso: string): string {
  if (!iso) return '---'
  const diff = Date.now() - new Date(iso).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 60) return `${mins}m ago`
  const hours = Math.floor(mins / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

// Goods summary stats.
const goodsSummary = computed(() => {
  const g = goods.value
  const totalProfit = g.reduce((s, x) => s + x.total_profit, 0)
  const totalTrades = g.reduce((s, x) => s + x.trade_count, 0)
  const profitable = g.filter(x => x.total_profit > 0).length
  const unprofitable = g.filter(x => x.total_profit <= 0).length
  return { totalProfit, totalTrades, profitable, unprofitable }
})

// Routes summary stats.
const routesSummary = computed(() => {
  const r = routes.value
  const totalProfit = r.reduce((s, x) => s + x.total_profit, 0)
  const totalTrades = r.reduce((s, x) => s + x.trade_count, 0)
  const profitable = r.filter(x => x.total_profit > 0).length
  const unprofitable = r.filter(x => x.total_profit <= 0).length
  return { totalProfit, totalTrades, profitable, unprofitable }
})

// Profit bar: max absolute profit for scaling bars.
function maxAbsProfit(items: { total_profit: number }[]): number {
  return Math.max(...items.map(i => Math.abs(i.total_profit)), 1)
}

function maxRevenue(items: { total_revenue: number }[]): number {
  return Math.max(...items.map(i => i.total_revenue), 1)
}

// Timeline sparkline: compute cumulative profit for each series.
function cumulativePoints(series: TimelineSeries): { time: string; cumulative: number }[] {
  let cum = 0
  return series.points.map(p => {
    cum += p.profit
    return { time: p.time, cumulative: cum }
  })
}

// Sparkline SVG path from points.
function sparklinePath(series: TimelineSeries, width: number, height: number): string {
  const pts = cumulativePoints(series)
  if (pts.length < 2) return ''

  const values = pts.map(p => p.cumulative)
  const min = Math.min(...values)
  const max = Math.max(...values)
  const range = max - min || 1

  return pts.map((p, i) => {
    const x = (i / (pts.length - 1)) * width
    const y = height - ((p.cumulative - min) / range) * height
    return `${i === 0 ? 'M' : 'L'} ${x.toFixed(1)} ${y.toFixed(1)}`
  }).join(' ')
}

// Top 10 timeline series for display.
const topTimeline = computed(() => {
  if (!timeline.value) return []
  return timeline.value.series.slice(0, 10)
})

// Color palette for timeline series.
const seriesColors = ['#10b981', '#3b82f6', '#f59e0b', '#ef4444', '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#6366f1']
</script>

<template>
  <div class="space-y-6">
    <!-- Header -->
    <div class="flex items-center justify-between">
      <div>
        <h2 class="text-3xl font-bold text-slate-100">Analytics</h2>
        <p class="text-sm text-slate-500 mt-1">Trade profitability by cargo, route, and time</p>
      </div>

      <div class="flex items-center gap-3">
        <!-- Time filter -->
        <div class="flex items-center gap-1 bg-slate-800 rounded-lg border border-slate-700 p-1">
          <button
            v-for="opt in timeOptions"
            :key="opt.label"
            class="px-3 py-1.5 text-xs font-medium rounded-md transition-colors"
            :class="timeFilter === opt.value ? 'bg-slate-600 text-slate-100' : 'text-slate-400 hover:text-slate-200'"
            @click="timeFilter = opt.value"
          >{{ opt.label }}</button>
        </div>

        <button
          class="flex items-center gap-2 text-sm text-slate-400 hover:text-slate-200 bg-slate-800 hover:bg-slate-700 border border-slate-700 rounded-lg px-4 py-2"
          @click="fetchGoods(timeFilter); fetchRoutes(timeFilter); fetchTimeline(timelineGroupBy, timelineHours); fetchPassengers(timeFilter)"
        >
          <Icon name="lucide:refresh-cw" class="text-sm" :class="loading ? 'animate-spin' : ''" />
          Refresh
        </button>
      </div>
    </div>

    <!-- Tab navigation -->
    <div class="flex items-center gap-1 bg-slate-800/50 rounded-lg border border-slate-700 p-1 w-fit">
      <button
        v-for="tab in [
          { id: 'goods' as const, label: 'Cargo Types', icon: 'lucide:package' },
          { id: 'routes' as const, label: 'Routes', icon: 'lucide:route' },
          { id: 'timeline' as const, label: 'Timeline', icon: 'lucide:trending-up' },
          { id: 'passengers' as const, label: 'Passengers', icon: 'lucide:users' },
        ]"
        :key="tab.id"
        class="flex items-center gap-2 px-4 py-2 text-sm font-medium rounded-md transition-colors"
        :class="activeTab === tab.id ? 'bg-slate-700 text-slate-100' : 'text-slate-400 hover:text-slate-200'"
        @click="activeTab = tab.id"
      >
        <Icon :name="tab.icon" class="text-base" />
        {{ tab.label }}
      </button>
    </div>

    <!-- GOODS TAB -->
    <template v-if="activeTab === 'goods'">
      <!-- Summary cards -->
      <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Total Cargo Profit</div>
          <div class="text-2xl font-bold font-mono" :class="profitColor(goodsSummary.totalProfit)">{{ formatCurrency(goodsSummary.totalProfit) }}g</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Completed Trades</div>
          <div class="text-2xl font-bold font-mono text-slate-200">{{ formatFull(goodsSummary.totalTrades) }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Profitable Goods</div>
          <div class="text-2xl font-bold font-mono text-emerald-400">{{ goodsSummary.profitable }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Unprofitable Goods</div>
          <div class="text-2xl font-bold font-mono text-rose-400">{{ goodsSummary.unprofitable }}</div>
        </div>
      </div>

      <!-- Goods table -->
      <div class="bg-slate-800/80 rounded-xl border border-slate-700 overflow-hidden">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-500 uppercase tracking-wider border-b border-slate-700">
              <th class="text-left py-3 px-4">Good</th>
              <th class="text-right py-3 px-4">Total Profit</th>
              <th class="text-right py-3 px-4 hidden lg:table-cell">Revenue</th>
              <th class="text-right py-3 px-4 hidden lg:table-cell">Cost</th>
              <th class="text-right py-3 px-4">Trades</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Qty</th>
              <th class="text-right py-3 px-4">Avg Profit</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Win Rate</th>
              <th class="text-right py-3 px-4 hidden xl:table-cell">Best</th>
              <th class="text-right py-3 px-4 hidden xl:table-cell">Worst</th>
              <th class="text-right py-3 px-4 hidden lg:table-cell">Last Trade</th>
              <th class="py-3 px-4 w-32">Profit Bar</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(g, i) in goods"
              :key="g.good_id"
              class="border-b border-slate-700/50 hover:bg-slate-700/30 transition-colors"
            >
              <td class="py-3 px-4">
                <div class="flex items-center gap-2">
                  <span class="text-xs text-slate-600 font-mono w-5">{{ i + 1 }}</span>
                  <span class="text-slate-200 font-medium">{{ g.good_name || g.good_id.slice(0, 8) }}</span>
                </div>
              </td>
              <td class="text-right py-3 px-4 font-mono font-bold" :class="profitColor(g.total_profit)">{{ formatCurrency(g.total_profit) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden lg:table-cell">{{ formatCurrency(g.total_revenue) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden lg:table-cell">{{ formatCurrency(g.total_cost) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(g.trade_count) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden md:table-cell">{{ formatFull(g.total_quantity) }}</td>
              <td class="text-right py-3 px-4 font-mono" :class="profitColor(g.avg_profit_per_trade)">{{ formatFull(Math.round(g.avg_profit_per_trade)) }}</td>
              <td class="text-right py-3 px-4 font-mono hidden md:table-cell" :class="g.win_rate >= 0.5 ? 'text-emerald-400' : 'text-rose-400'">{{ formatPct(g.win_rate) }}</td>
              <td class="text-right py-3 px-4 font-mono text-emerald-400/70 hidden xl:table-cell">{{ formatFull(g.best_profit) }}</td>
              <td class="text-right py-3 px-4 font-mono text-rose-400/70 hidden xl:table-cell">{{ formatFull(g.worst_profit) }}</td>
              <td class="text-right py-3 px-4 text-slate-500 hidden lg:table-cell">{{ timeAgo(g.last_trade) }}</td>
              <td class="py-3 px-4">
                <div class="w-full h-2 rounded-full bg-slate-700 overflow-hidden">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="g.total_profit >= 0 ? 'bg-emerald-500' : 'bg-rose-500'"
                    :style="{ width: Math.min(Math.abs(g.total_profit) / maxAbsProfit(goods) * 100, 100) + '%' }"
                  />
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div v-if="goods.length === 0" class="py-12 text-center text-slate-600">
          <Icon name="lucide:package" class="text-3xl mb-2" />
          <p>No trade data yet</p>
        </div>
      </div>
    </template>

    <!-- ROUTES TAB -->
    <template v-if="activeTab === 'routes'">
      <!-- Summary cards -->
      <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Total Route Profit</div>
          <div class="text-2xl font-bold font-mono" :class="profitColor(routesSummary.totalProfit)">{{ formatCurrency(routesSummary.totalProfit) }}g</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Completed Trades</div>
          <div class="text-2xl font-bold font-mono text-slate-200">{{ formatFull(routesSummary.totalTrades) }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Profitable Routes</div>
          <div class="text-2xl font-bold font-mono text-emerald-400">{{ routesSummary.profitable }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Losing Routes</div>
          <div class="text-2xl font-bold font-mono text-rose-400">{{ routesSummary.unprofitable }}</div>
        </div>
      </div>

      <!-- Routes table -->
      <div class="bg-slate-800/80 rounded-xl border border-slate-700 overflow-hidden">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-500 uppercase tracking-wider border-b border-slate-700">
              <th class="text-left py-3 px-4">Route</th>
              <th class="text-right py-3 px-4">Total Profit</th>
              <th class="text-right py-3 px-4">Trades</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Qty</th>
              <th class="text-right py-3 px-4">Avg Profit</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Win Rate</th>
              <th class="text-left py-3 px-4 hidden lg:table-cell">Top Good</th>
              <th class="text-right py-3 px-4 hidden lg:table-cell">Last Trade</th>
              <th class="py-3 px-4 w-32">Profit Bar</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(r, i) in routes"
              :key="r.from_port_id + r.to_port_id"
              class="border-b border-slate-700/50 hover:bg-slate-700/30 transition-colors"
            >
              <td class="py-3 px-4">
                <div class="flex items-center gap-2">
                  <span class="text-xs text-slate-600 font-mono w-5">{{ i + 1 }}</span>
                  <span class="text-slate-200">{{ r.from_port_name || r.from_port_id.slice(0, 8) }}</span>
                  <Icon name="lucide:arrow-right" class="text-slate-600 text-xs" />
                  <span class="text-slate-200">{{ r.to_port_name || r.to_port_id.slice(0, 8) }}</span>
                </div>
              </td>
              <td class="text-right py-3 px-4 font-mono font-bold" :class="profitColor(r.total_profit)">{{ formatCurrency(r.total_profit) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(r.trade_count) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden md:table-cell">{{ formatFull(r.total_quantity) }}</td>
              <td class="text-right py-3 px-4 font-mono" :class="profitColor(r.avg_profit_per_trade)">{{ formatFull(Math.round(r.avg_profit_per_trade)) }}</td>
              <td class="text-right py-3 px-4 font-mono hidden md:table-cell" :class="r.win_rate >= 0.5 ? 'text-emerald-400' : 'text-rose-400'">{{ formatPct(r.win_rate) }}</td>
              <td class="py-3 px-4 text-slate-400 hidden lg:table-cell">{{ r.top_good_name || '---' }}</td>
              <td class="text-right py-3 px-4 text-slate-500 hidden lg:table-cell">{{ timeAgo(r.last_trade) }}</td>
              <td class="py-3 px-4">
                <div class="w-full h-2 rounded-full bg-slate-700 overflow-hidden">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="r.total_profit >= 0 ? 'bg-emerald-500' : 'bg-rose-500'"
                    :style="{ width: Math.min(Math.abs(r.total_profit) / maxAbsProfit(routes) * 100, 100) + '%' }"
                  />
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div v-if="routes.length === 0" class="py-12 text-center text-slate-600">
          <Icon name="lucide:route" class="text-3xl mb-2" />
          <p>No route data yet</p>
        </div>
      </div>
    </template>

    <!-- TIMELINE TAB -->
    <template v-if="activeTab === 'timeline'">
      <!-- Timeline controls -->
      <div class="flex items-center gap-4">
        <div class="flex items-center gap-2">
          <span class="text-xs text-slate-500">Group by:</span>
          <div class="flex items-center gap-1 bg-slate-800 rounded-lg border border-slate-700 p-1">
            <button
              v-for="opt in [
                { label: 'Cargo', value: 'good' as const },
                { label: 'Route', value: 'route' as const },
                { label: 'Strategy', value: 'strategy' as const },
              ]"
              :key="opt.value"
              class="px-3 py-1 text-xs font-medium rounded-md transition-colors"
              :class="timelineGroupBy === opt.value ? 'bg-slate-600 text-slate-100' : 'text-slate-400 hover:text-slate-200'"
              @click="timelineGroupBy = opt.value"
            >{{ opt.label }}</button>
          </div>
        </div>

        <div class="flex items-center gap-2">
          <span class="text-xs text-slate-500">Period:</span>
          <div class="flex items-center gap-1 bg-slate-800 rounded-lg border border-slate-700 p-1">
            <button
              v-for="opt in [
                { label: '24h', value: 24 },
                { label: '3d', value: 72 },
                { label: '7d', value: 168 },
                { label: '30d', value: 720 },
              ]"
              :key="opt.value"
              class="px-3 py-1 text-xs font-medium rounded-md transition-colors"
              :class="timelineHours === opt.value ? 'bg-slate-600 text-slate-100' : 'text-slate-400 hover:text-slate-200'"
              @click="timelineHours = opt.value"
            >{{ opt.label }}</button>
          </div>
        </div>
      </div>

      <!-- Timeline series cards -->
      <div v-if="timeline && topTimeline.length > 0" class="space-y-3">
        <div
          v-for="(s, i) in topTimeline"
          :key="s.name"
          class="bg-slate-800/80 rounded-xl border border-slate-700 p-5"
        >
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-3">
              <span
                class="w-3 h-3 rounded-full flex-shrink-0"
                :style="{ backgroundColor: seriesColors[i % seriesColors.length] }"
              />
              <span class="text-slate-200 font-medium">{{ s.name }}</span>
              <span class="text-xs text-slate-500">{{ s.points.length }} {{ timeline?.bucket_size }}s</span>
            </div>
            <div class="flex items-center gap-4">
              <span class="text-xs text-slate-500">Total trades: <span class="text-slate-300 font-mono">{{ s.points.reduce((a, p) => a + p.count, 0) }}</span></span>
              <span class="font-mono font-bold" :class="profitColor(s.total_profit)">{{ formatCurrency(s.total_profit) }}g</span>
            </div>
          </div>

          <!-- Sparkline -->
          <div v-if="s.points.length >= 2" class="h-12">
            <svg class="w-full h-full" preserveAspectRatio="none" :viewBox="`0 0 400 48`">
              <!-- Zero line -->
              <line x1="0" x2="400" y1="24" y2="24" stroke="#334155" stroke-width="0.5" stroke-dasharray="4 4" />
              <!-- Profit line -->
              <path
                :d="sparklinePath(s, 400, 48)"
                fill="none"
                :stroke="seriesColors[i % seriesColors.length]"
                stroke-width="2"
                vector-effect="non-scaling-stroke"
              />
            </svg>
          </div>

          <!-- Per-bucket bars -->
          <div class="flex items-end gap-px mt-2 h-8">
            <div
              v-for="(p, j) in s.points"
              :key="j"
              class="flex-1 relative group"
            >
              <div
                class="w-full rounded-t-sm transition-all"
                :class="p.profit >= 0 ? 'bg-emerald-500/60' : 'bg-rose-500/60'"
                :style="{
                  height: Math.max(Math.abs(p.profit) / Math.max(...s.points.map(pp => Math.abs(pp.profit)), 1) * 32, 1) + 'px',
                }"
              />
              <!-- Tooltip on hover -->
              <div class="absolute bottom-full left-1/2 -translate-x-1/2 mb-1 hidden group-hover:block z-10 whitespace-nowrap bg-slate-900 border border-slate-600 rounded px-2 py-1 text-[10px] text-slate-300 shadow-lg">
                <div>{{ p.time }}</div>
                <div class="font-mono" :class="profitColor(p.profit)">{{ formatFull(p.profit) }}g</div>
                <div>{{ p.count }} trades, {{ formatFull(p.quantity) }} units</div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div v-else-if="!loading" class="py-12 text-center text-slate-600">
        <Icon name="lucide:trending-up" class="text-3xl mb-2" />
        <p>No timeline data yet</p>
      </div>
    </template>

    <!-- PASSENGERS TAB -->
    <template v-if="activeTab === 'passengers'">
      <!-- Summary cards -->
      <div v-if="passengers" class="grid grid-cols-2 md:grid-cols-5 gap-4">
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Total Revenue</div>
          <div class="text-2xl font-bold font-mono text-emerald-400">{{ formatCurrency(passengers.summary.total_revenue) }}g</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Passengers Boarded</div>
          <div class="text-2xl font-bold font-mono text-slate-200">{{ formatFull(passengers.summary.total_passengers) }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Total Snipes</div>
          <div class="text-2xl font-bold font-mono text-slate-200">{{ formatFull(passengers.summary.total_snipes) }}</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Avg Bid / Snipe</div>
          <div class="text-2xl font-bold font-mono text-slate-200">{{ formatFull(Math.round(passengers.summary.avg_bid_per_snipe)) }}g</div>
        </div>
        <div class="bg-slate-800 rounded-xl border border-slate-700 p-5">
          <div class="text-xs text-slate-500 mb-1">Top Ship</div>
          <div class="text-lg font-bold text-slate-200 truncate">{{ passengers.summary.top_ship_name || '---' }}</div>
          <div class="text-xs text-slate-500">{{ passengers.summary.top_ship_snipes }} snipes</div>
        </div>
      </div>

      <!-- Routes table -->
      <div class="bg-slate-800/80 rounded-xl border border-slate-700 overflow-hidden">
        <div class="px-4 py-3 border-b border-slate-700">
          <h3 class="text-sm font-semibold text-slate-300">Routes</h3>
        </div>
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-500 uppercase tracking-wider border-b border-slate-700">
              <th class="text-left py-3 px-4">Route</th>
              <th class="text-right py-3 px-4">Revenue</th>
              <th class="text-right py-3 px-4">Passengers</th>
              <th class="text-right py-3 px-4">Snipes</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Avg Bid</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Max Bid</th>
              <th class="text-right py-3 px-4 hidden lg:table-cell">Last Snipe</th>
              <th class="py-3 px-4 w-32">Revenue Bar</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(r, i) in passengers?.routes ?? []"
              :key="r.origin_port_id + r.destination_port_id"
              class="border-b border-slate-700/50 hover:bg-slate-700/30 transition-colors"
            >
              <td class="py-3 px-4">
                <div class="flex items-center gap-2">
                  <span class="text-xs text-slate-600 font-mono w-5">{{ i + 1 }}</span>
                  <span class="text-slate-200">{{ r.origin_port_name || r.origin_port_id.slice(0, 8) }}</span>
                  <Icon name="lucide:arrow-right" class="text-slate-600 text-xs" />
                  <span class="text-slate-200">{{ r.destination_port_name || r.destination_port_id.slice(0, 8) }}</span>
                </div>
              </td>
              <td class="text-right py-3 px-4 font-mono font-bold text-emerald-400">{{ formatCurrency(r.total_revenue) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(r.total_passengers) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(r.snipe_count) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden md:table-cell">{{ formatFull(Math.round(r.avg_bid)) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden md:table-cell">{{ formatFull(r.max_bid) }}</td>
              <td class="text-right py-3 px-4 text-slate-500 hidden lg:table-cell">{{ timeAgo(r.last_snipe) }}</td>
              <td class="py-3 px-4">
                <div class="w-full h-2 rounded-full bg-slate-700 overflow-hidden">
                  <div
                    class="h-full rounded-full bg-emerald-500 transition-all"
                    :style="{ width: Math.min(r.total_revenue / maxRevenue(passengers?.routes ?? []) * 100, 100) + '%' }"
                  />
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div v-if="!passengers?.routes?.length" class="py-12 text-center text-slate-600">
          <Icon name="lucide:users" class="text-3xl mb-2" />
          <p>No passenger data yet</p>
        </div>
      </div>

      <!-- Ships table -->
      <div class="bg-slate-800/80 rounded-xl border border-slate-700 overflow-hidden">
        <div class="px-4 py-3 border-b border-slate-700">
          <h3 class="text-sm font-semibold text-slate-300">Ship Performance</h3>
        </div>
        <table class="w-full text-sm">
          <thead>
            <tr class="text-xs text-slate-500 uppercase tracking-wider border-b border-slate-700">
              <th class="text-left py-3 px-4">Ship</th>
              <th class="text-right py-3 px-4">Revenue</th>
              <th class="text-right py-3 px-4">Passengers</th>
              <th class="text-right py-3 px-4">Snipes</th>
              <th class="text-right py-3 px-4 hidden md:table-cell">Avg Bid</th>
              <th class="py-3 px-4 w-32">Revenue Bar</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(sh, i) in passengers?.ships ?? []"
              :key="sh.ship_id"
              class="border-b border-slate-700/50 hover:bg-slate-700/30 transition-colors"
            >
              <td class="py-3 px-4">
                <div class="flex items-center gap-2">
                  <span class="text-xs text-slate-600 font-mono w-5">{{ i + 1 }}</span>
                  <span class="text-slate-200 font-medium">{{ sh.ship_name || sh.ship_id.slice(0, 8) }}</span>
                </div>
              </td>
              <td class="text-right py-3 px-4 font-mono font-bold text-emerald-400">{{ formatCurrency(sh.total_revenue) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(sh.total_passengers) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-300">{{ formatFull(sh.snipe_count) }}</td>
              <td class="text-right py-3 px-4 font-mono text-slate-400 hidden md:table-cell">{{ formatFull(Math.round(sh.avg_bid)) }}</td>
              <td class="py-3 px-4">
                <div class="w-full h-2 rounded-full bg-slate-700 overflow-hidden">
                  <div
                    class="h-full rounded-full bg-emerald-500 transition-all"
                    :style="{ width: Math.min(sh.total_revenue / maxRevenue(passengers?.ships ?? []) * 100, 100) + '%' }"
                  />
                </div>
              </td>
            </tr>
          </tbody>
        </table>

        <div v-if="!passengers?.ships?.length" class="py-12 text-center text-slate-600">
          <Icon name="lucide:ship" class="text-3xl mb-2" />
          <p>No ship data yet</p>
        </div>
      </div>
    </template>

    <!-- Loading overlay -->
    <div v-if="loading && goods.length === 0 && routes.length === 0" class="flex flex-col items-center justify-center py-20 text-slate-600">
      <Icon name="mdi:loading" class="animate-spin text-4xl mb-4" />
      <p>Loading analytics...</p>
    </div>
  </div>
</template>
