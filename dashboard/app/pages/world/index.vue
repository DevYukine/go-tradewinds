<script setup lang="ts">
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-center justify-between">
      <div>
        <h2 class="text-3xl font-bold text-slate-100">World Map</h2>
        <p class="text-sm text-slate-500 mt-1.5">Live ship positions and trade routes</p>
      </div>
      <NuxtLink
        to="/world/data"
        class="flex items-center gap-2 text-sm text-slate-400 hover:text-slate-200 transition-colors bg-slate-800 hover:bg-slate-700 border border-slate-700 rounded-lg px-4 py-2"
      >
        <Icon name="lucide:database" class="text-base" />
        World Data
      </NuxtLink>
    </div>

    <WorldMap />
  </div>
</template>
