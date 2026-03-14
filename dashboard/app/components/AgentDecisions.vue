<script setup lang="ts">
import type { AgentDecision } from '~/types'

const props = defineProps<{
  companyId: number
}>()

const { decisions, loading, fetchDecisions } = useAgent()
const expandedId = ref<number | null>(null)
let pollTimer: ReturnType<typeof setInterval> | null = null

const { world } = useWorld()

watch(
  () => props.companyId,
  (id) => {
    if (id) {
      fetchDecisions(id)
      if (pollTimer) clearInterval(pollTimer)
      pollTimer = setInterval(() => fetchDecisions(id), 15000)
    }
  },
  { immediate: true }
)

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

function toggleExpand(id: number) {
  expandedId.value = expandedId.value === id ? null : id
}

function decisionTypeIcon(type: AgentDecision['decision_type']): string {
  switch (type) {
    case 'trade': return 'lucide:arrow-left-right'
    case 'fleet': return 'mdi:ship'
    case 'market': return 'lucide:bar-chart-3'
    case 'strategy_eval': return 'lucide:brain'
    default: return 'lucide:activity'
  }
}

function decisionTypeColor(type: string): string {
  switch (type) {
    case 'trade': return 'bg-amber-500/20 text-amber-400'
    case 'fleet': return 'bg-sky-500/20 text-sky-400'
    case 'market': return 'bg-violet-500/20 text-violet-400'
    default: return 'bg-slate-500/20 text-slate-400'
  }
}

function confidenceColor(confidence: number): string {
  if (confidence >= 0.8) return 'text-emerald-400'
  if (confidence >= 0.5) return 'text-yellow-400'
  if (confidence > 0) return 'text-rose-400'
  return 'text-slate-600'
}

function confidenceBarColor(confidence: number): string {
  if (confidence >= 0.8) return 'bg-emerald-500'
  if (confidence >= 0.5) return 'bg-yellow-500'
  if (confidence > 0) return 'bg-rose-500'
  return 'bg-slate-600'
}

function confidenceLabel(confidence: number): string {
  if (confidence >= 0.8) return 'High'
  if (confidence >= 0.5) return 'Medium'
  if (confidence > 0) return 'Low'
  return '—'
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleTimeString('en-US', { hour12: false })
}

function formatCurrency(value: number): string {
  return new Intl.NumberFormat('en-US').format(value)
}

// Resolve UUID → name from world data
function portName(id: string): string {
  if (!id || !world.value) return id?.substring(0, 8) || '?'
  const port = world.value.ports.find(p => p.id === id)
  return port?.name || id.substring(0, 8)
}

function goodName(id: string): string {
  if (!id || !world.value) return id?.substring(0, 8) || '?'
  const good = world.value.goods.find(g => g.id === id)
  return good?.name || id.substring(0, 8)
}

function shipTypeName(id: string): string {
  if (!id || !world.value) return id?.substring(0, 8) || '?'
  const st = world.value.ship_types.find(s => s.id === id)
  return st?.name || id.substring(0, 8)
}

// Parse JSON response safely
function parseResponse(json: string): Record<string, any> | null {
  try { return JSON.parse(json) } catch { return null }
}

function parseRequest(json: string): Record<string, any> | null {
  try { return JSON.parse(json) } catch { return null }
}

// Extract a summary of actions from a trade response
function tradeActionSummary(resp: Record<string, any>): string[] {
  const actions: string[] = []
  if (resp.SellOrders?.length) {
    for (const o of resp.SellOrders) {
      actions.push(`Sell ${o.Quantity}× ${goodName(o.GoodID)}`)
    }
  }
  if (resp.BuyOrders?.length) {
    for (const o of resp.BuyOrders) {
      actions.push(`Buy ${o.Quantity}× ${goodName(o.GoodID)}`)
    }
  }
  if (resp.BoardPassengers?.length) {
    actions.push(`Board ${resp.BoardPassengers.length} passenger group${resp.BoardPassengers.length > 1 ? 's' : ''}`)
  }
  if (resp.SailTo) {
    actions.push(`Sail → ${portName(resp.SailTo)}`)
  }
  return actions
}

function fleetActionSummary(resp: Record<string, any>): string[] {
  const actions: string[] = []
  if (resp.BuyShips?.length) {
    for (const s of resp.BuyShips) {
      actions.push(`Buy ${shipTypeName(s.ShipTypeID)} at ${portName(s.PortID)}`)
    }
  }
  if (resp.SellShips?.length) {
    actions.push(`Sell ${resp.SellShips.length} ship${resp.SellShips.length > 1 ? 's' : ''}`)
  }
  if (resp.BuyWarehouses?.length) {
    for (const id of resp.BuyWarehouses) {
      actions.push(`Build warehouse at ${portName(id)}`)
    }
  }
  return actions
}

function actionSummary(decision: AgentDecision): string[] {
  const resp = parseResponse(decision.response)
  if (!resp) return []
  if (decision.decision_type === 'trade') return tradeActionSummary(resp)
  if (decision.decision_type === 'fleet') return fleetActionSummary(resp)
  if (decision.decision_type === 'market') return tradeActionSummary(resp)
  return []
}

// Get ship name from request
function shipFromRequest(decision: AgentDecision): string | null {
  const req = parseRequest(decision.request)
  return req?.Ship?.Name || null
}

// Get port name from request (current port)
function portFromRequest(decision: AgentDecision): string | null {
  const req = parseRequest(decision.request)
  if (!req?.Ship?.PortID) return null
  return portName(req.Ship.PortID)
}

// Get action label from response
function actionLabel(decision: AgentDecision): string | null {
  const resp = parseResponse(decision.response)
  if (!resp?.Action) return null
  return resp.Action.replace(/_/g, ' ')
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-4">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:brain" class="text-purple-400" />
        Agent Decisions
      </h3>
      <button
        class="text-xs text-slate-500 hover:text-slate-300 transition-colors"
        @click="fetchDecisions(companyId)"
      >
        <Icon name="lucide:refresh-cw" class="mr-1" />
        Refresh
      </button>
    </div>

    <div v-if="loading" class="flex items-center justify-center py-8">
      <Icon name="mdi:loading" class="animate-spin text-2xl text-slate-500" />
    </div>

    <div v-else-if="decisions.length === 0" class="text-center text-slate-600 text-sm py-8">
      No agent decisions recorded
    </div>

    <div v-else class="space-y-2 max-h-80 2xl:max-h-[28rem] overflow-y-auto scroll-stable">
      <div
        v-for="decision in decisions"
        :key="decision.id"
        class="bg-slate-900/50 rounded-lg border border-slate-700/50"
      >
        <!-- Collapsed Header -->
        <button
          class="w-full p-3 text-left"
          @click="toggleExpand(decision.id)"
        >
          <!-- Row 1: Agent + Type + Time -->
          <div class="flex items-center gap-2 mb-1.5">
            <Icon
              :name="decisionTypeIcon(decision.decision_type)"
              class="text-slate-400 flex-shrink-0"
            />
            <span class="text-sm font-medium text-slate-200">{{ decision.agent_name }}</span>
            <span
              class="px-1.5 py-0.5 rounded text-[10px] font-medium"
              :class="decisionTypeColor(decision.decision_type)"
            >
              {{ decision.decision_type }}
            </span>
            <span
              v-if="actionLabel(decision)"
              class="px-1.5 py-0.5 rounded text-[10px] font-medium bg-slate-700/50 text-slate-300"
            >
              {{ actionLabel(decision) }}
            </span>
            <span class="ml-auto text-[10px] text-slate-600 font-mono">{{ formatTime(decision.created_at) }}</span>
            <Icon
              name="lucide:chevron-down"
              class="text-slate-500 transition-transform flex-shrink-0"
              :class="expandedId === decision.id ? 'rotate-180' : ''"
            />
          </div>

          <!-- Row 2: Context (ship @ port) -->
          <div class="flex items-center gap-2 mb-1.5 text-xs">
            <template v-if="shipFromRequest(decision)">
              <Icon name="mdi:sail-boat" class="text-slate-500 text-[11px]" />
              <span class="text-slate-400">{{ shipFromRequest(decision) }}</span>
            </template>
            <template v-if="portFromRequest(decision)">
              <Icon name="lucide:anchor" class="text-slate-500 text-[11px]" />
              <span class="text-slate-400">{{ portFromRequest(decision) }}</span>
            </template>
          </div>

          <!-- Row 3: Reasoning -->
          <p class="text-xs text-slate-400 mb-2 leading-relaxed">{{ decision.reasoning }}</p>

          <!-- Row 4: Actions + Confidence -->
          <div class="flex items-center justify-between gap-3">
            <!-- Action pills -->
            <div class="flex flex-wrap gap-1 flex-1 min-w-0">
              <span
                v-for="(action, i) in actionSummary(decision).slice(0, 4)"
                :key="i"
                class="inline-flex items-center px-2 py-0.5 rounded-full text-[10px] font-medium bg-slate-800 border border-slate-700/50"
                :class="action.startsWith('Sell') ? 'text-emerald-400' :
                         action.startsWith('Buy') ? 'text-sky-400' :
                         action.startsWith('Board') ? 'text-purple-400' :
                         action.startsWith('Sail') ? 'text-amber-400' : 'text-slate-400'"
              >
                {{ action }}
              </span>
              <span
                v-if="actionSummary(decision).length === 0"
                class="text-[10px] text-slate-600 italic"
              >
                no actions
              </span>
            </div>

            <!-- Confidence -->
            <div class="flex items-center gap-2 flex-shrink-0">
              <div class="text-right">
                <div class="text-[10px] text-slate-500">Confidence</div>
                <div class="flex items-center gap-1.5">
                  <div class="w-12 h-1.5 rounded-full bg-slate-700 overflow-hidden">
                    <div
                      class="h-full rounded-full transition-all"
                      :class="confidenceBarColor(decision.confidence)"
                      :style="{ width: `${decision.confidence * 100}%` }"
                    />
                  </div>
                  <span
                    class="text-xs font-mono font-medium"
                    :class="confidenceColor(decision.confidence)"
                  >
                    {{ decision.confidence > 0 ? `${(decision.confidence * 100).toFixed(0)}%` : '—' }}
                  </span>
                </div>
              </div>
              <div v-if="decision.latency_ms > 0" class="text-right">
                <div class="text-[10px] text-slate-500">Latency</div>
                <span class="text-xs text-slate-400 font-mono">{{ decision.latency_ms }}ms</span>
              </div>
            </div>
          </div>
        </button>

        <!-- Expanded Details -->
        <div
          v-if="expandedId === decision.id"
          class="px-3 pb-3 border-t border-slate-700/50"
        >
          <div class="pt-3 space-y-3">
            <!-- Full reasoning -->
            <div>
              <div class="text-[10px] text-slate-500 uppercase tracking-wide mb-1">Reasoning</div>
              <p class="text-sm text-slate-300">{{ decision.reasoning }}</p>
            </div>

            <!-- All actions -->
            <div v-if="actionSummary(decision).length > 0">
              <div class="text-[10px] text-slate-500 uppercase tracking-wide mb-1">Actions Taken</div>
              <div class="space-y-1">
                <div
                  v-for="(action, i) in actionSummary(decision)"
                  :key="i"
                  class="flex items-center gap-2 text-xs"
                >
                  <span
                    class="w-1.5 h-1.5 rounded-full flex-shrink-0"
                    :class="action.startsWith('Sell') ? 'bg-emerald-400' :
                             action.startsWith('Buy') ? 'bg-sky-400' :
                             action.startsWith('Board') ? 'bg-purple-400' :
                             action.startsWith('Sail') ? 'bg-amber-400' : 'bg-slate-400'"
                  />
                  <span class="text-slate-300">{{ action }}</span>
                </div>
              </div>
            </div>

            <!-- Context from request -->
            <div v-if="parseRequest(decision.request)" class="grid grid-cols-2 gap-2 text-xs">
              <div v-if="parseRequest(decision.request)?.Company?.Treasury != null">
                <span class="text-slate-500">Treasury: </span>
                <span class="text-amber-400 font-mono">{{ formatCurrency(parseRequest(decision.request)!.Company.Treasury) }}</span>
              </div>
              <div v-if="parseRequest(decision.request)?.Ship?.Cargo?.length > 0">
                <span class="text-slate-500">Cargo: </span>
                <span class="text-slate-300">{{ parseRequest(decision.request)!.Ship.Cargo.length }} item(s)</span>
              </div>
              <div v-if="parseRequest(decision.request)?.Constraints?.MaxSpend != null">
                <span class="text-slate-500">Max spend: </span>
                <span class="text-slate-300 font-mono">{{ formatCurrency(parseRequest(decision.request)!.Constraints.MaxSpend) }}</span>
              </div>
              <div v-if="parseRequest(decision.request)?.AvailablePassengers?.length > 0">
                <span class="text-slate-500">Passengers avail: </span>
                <span class="text-purple-400">{{ parseRequest(decision.request)!.AvailablePassengers.length }} group(s)</span>
              </div>
            </div>

            <!-- Metadata -->
            <div class="flex flex-wrap gap-x-4 gap-y-1 text-[10px] text-slate-600 pt-1 border-t border-slate-700/30">
              <span>{{ formatTime(decision.created_at) }}</span>
              <span v-if="decision.latency_ms > 0">{{ decision.latency_ms }}ms</span>
              <span>Confidence: {{ confidenceLabel(decision.confidence) }} ({{ (decision.confidence * 100).toFixed(0) }}%)</span>
              <span v-if="decision.outcome && decision.outcome !== 'pending'">
                Outcome: {{ decision.outcome }}
                <template v-if="decision.outcome_value !== 0">
                  ({{ decision.outcome_value > 0 ? '+' : '' }}{{ formatCurrency(decision.outcome_value) }})
                </template>
              </span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
