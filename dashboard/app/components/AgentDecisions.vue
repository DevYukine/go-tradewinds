<script setup lang="ts">
import type { AgentDecision } from '~/types'

const props = defineProps<{
  companyId: number
}>()

const { decisions, loading, fetchDecisions } = useAgent()
const expandedId = ref<number | null>(null)
let pollTimer: ReturnType<typeof setInterval> | null = null

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

function outcomeClasses(outcome: AgentDecision['outcome']): string {
  switch (outcome) {
    case 'profit': return 'bg-emerald-500/20 text-emerald-400'
    case 'loss': return 'bg-rose-500/20 text-rose-400'
    case 'neutral': return 'bg-slate-500/20 text-slate-400'
    case 'pending': return 'bg-yellow-500/20 text-yellow-400'
    default: return 'bg-slate-500/20 text-slate-400'
  }
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

function confidenceColor(confidence: number): string {
  if (confidence >= 0.8) return 'bg-emerald-500'
  if (confidence >= 0.5) return 'bg-yellow-500'
  return 'bg-rose-500'
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleTimeString('en-US', { hour12: false })
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
        <button
          class="w-full flex items-center gap-3 p-3 text-left"
          @click="toggleExpand(decision.id)"
        >
          <Icon
            :name="decisionTypeIcon(decision.decision_type)"
            class="text-slate-400 flex-shrink-0"
          />

          <div class="flex-1 min-w-0">
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium text-slate-200">{{ decision.agent_name }}</span>
              <span class="text-xs text-slate-500">{{ decision.decision_type }}</span>
            </div>
            <p class="text-xs text-slate-500 truncate">{{ decision.reasoning }}</p>
          </div>

          <div class="flex items-center gap-3 flex-shrink-0">
            <div class="w-16">
              <div class="h-1.5 rounded-full bg-slate-700 overflow-hidden">
                <div
                  class="h-full rounded-full transition-all"
                  :class="confidenceColor(decision.confidence)"
                  :style="{ width: `${decision.confidence * 100}%` }"
                />
              </div>
              <span class="text-[10px] text-slate-500">{{ (decision.confidence * 100).toFixed(0) }}%</span>
            </div>

            <span class="text-xs text-slate-500 font-mono">{{ decision.latency_ms }}ms</span>

            <span
              class="px-2 py-0.5 rounded-full text-[10px] font-medium"
              :class="outcomeClasses(decision.outcome)"
            >
              {{ decision.outcome }}
            </span>

            <Icon
              name="lucide:chevron-down"
              class="text-slate-500 transition-transform"
              :class="expandedId === decision.id ? 'rotate-180' : ''"
            />
          </div>
        </button>

        <div
          v-if="expandedId === decision.id"
          class="px-3 pb-3 border-t border-slate-700/50"
        >
          <div class="pt-3 space-y-2">
            <div>
              <span class="text-xs text-slate-500">Full Reasoning</span>
              <p class="text-sm text-slate-300 mt-1">{{ decision.reasoning }}</p>
            </div>
            <div class="flex gap-4 text-xs text-slate-500">
              <span>Outcome Value: <span class="text-slate-300 font-mono">{{ decision.outcome_value }}</span></span>
              <span>Time: {{ formatTime(decision.created_at) }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
