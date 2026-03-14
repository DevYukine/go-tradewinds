<script setup lang="ts">
const props = defineProps<{
  companyId: number
}>()

const { logs, paused, connected, connectSSE, disconnectSSE, togglePause } = useLogs()
const logContainer = ref<HTMLElement | null>(null)

watch(
  () => props.companyId,
  (id) => {
    if (id) {
      connectSSE(id)
    }
  },
  { immediate: true }
)

watch(
  () => logs.value.length,
  () => {
    if (!paused.value) {
      nextTick(() => {
        if (logContainer.value) {
          logContainer.value.scrollTop = logContainer.value.scrollHeight
        }
      })
    }
  }
)

onUnmounted(() => {
  disconnectSSE()
})

function levelColor(level: string): string {
  switch (level) {
    case 'info': return 'text-slate-400'
    case 'warn': return 'text-yellow-400'
    case 'error': return 'text-rose-400'
    case 'trade': return 'text-emerald-400'
    case 'event': return 'text-blue-400'
    case 'agent': return 'text-purple-400'
    case 'optimizer': return 'text-orange-400'
    default: return 'text-slate-400'
  }
}

function levelBgColor(level: string): string {
  switch (level) {
    case 'info': return 'bg-slate-500/10'
    case 'warn': return 'bg-yellow-500/10'
    case 'error': return 'bg-rose-500/10'
    case 'trade': return 'bg-emerald-500/10'
    case 'event': return 'bg-blue-500/10'
    case 'agent': return 'bg-purple-500/10'
    case 'optimizer': return 'bg-orange-500/10'
    default: return 'bg-slate-500/10'
  }
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr)
  return d.toLocaleTimeString('en-US', { hour12: false })
}
</script>

<template>
  <div class="bg-slate-800 rounded-lg shadow-lg border border-slate-700 p-5">
    <div class="flex items-center justify-between mb-3">
      <h3 class="text-sm font-semibold text-slate-300 flex items-center gap-2">
        <Icon name="lucide:terminal" class="text-emerald-400" />
        Live Logs
        <span
          class="w-2 h-2 rounded-full"
          :class="connected ? 'bg-emerald-500 animate-pulse' : 'bg-rose-500'"
        />
      </h3>
      <div class="flex items-center gap-2">
        <span class="text-xs text-slate-500">{{ logs.length }} entries</span>
        <button
          class="px-3 py-1 rounded text-xs font-medium transition-colors"
          :class="
            paused
              ? 'bg-emerald-500/20 text-emerald-400 hover:bg-emerald-500/30'
              : 'bg-yellow-500/20 text-yellow-400 hover:bg-yellow-500/30'
          "
          @click="togglePause"
        >
          <Icon :name="paused ? 'lucide:play' : 'lucide:pause'" class="mr-1" />
          {{ paused ? 'Resume' : 'Pause' }}
        </button>
      </div>
    </div>

    <div
      ref="logContainer"
      class="h-64 2xl:h-80 overflow-y-auto scroll-stable font-mono text-xs space-y-0.5 bg-slate-900/50 rounded-lg p-3"
    >
      <div
        v-for="(entry, i) in logs"
        :key="i"
        class="flex gap-2 py-0.5 px-1 rounded"
        :class="levelBgColor(entry.level)"
      >
        <span class="text-slate-600 flex-shrink-0">{{ formatTime(entry.created_at) }}</span>
        <span
          class="w-12 flex-shrink-0 uppercase font-semibold"
          :class="levelColor(entry.level)"
        >
          {{ entry.level }}
        </span>
        <span class="text-slate-300">{{ entry.message }}</span>
      </div>

      <div v-if="logs.length === 0" class="text-center text-slate-600 py-8">
        Waiting for log entries...
      </div>
    </div>
  </div>
</template>
