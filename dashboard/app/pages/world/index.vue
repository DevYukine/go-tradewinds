<script setup lang="ts">
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-center justify-between">
      <div>
        <h2 class="text-2xl font-bold text-slate-100">World Map</h2>
        <p class="text-sm text-slate-500 mt-1">Live ship positions and trade routes</p>
      </div>
      <NuxtLink
        to="/world/data"
        class="flex items-center gap-1.5 text-sm text-slate-400 hover:text-slate-200 transition-colors bg-slate-800 hover:bg-slate-700 border border-slate-700 rounded-lg px-3 py-1.5"
      >
        <Icon name="lucide:database" class="text-sm" />
        World Data
      </NuxtLink>
    </div>

    <WorldMap />
  </div>
</template>
