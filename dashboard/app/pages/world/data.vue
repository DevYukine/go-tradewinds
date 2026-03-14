<script setup lang="ts">
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-center justify-between">
      <div>
        <h2 class="text-2xl font-bold text-slate-100">World Data</h2>
        <p class="text-sm text-slate-500 mt-1">Ports, goods, routes, and ship types</p>
      </div>
      <NuxtLink
        to="/world"
        class="flex items-center gap-1.5 text-sm text-slate-400 hover:text-slate-200 transition-colors bg-slate-800 hover:bg-slate-700 border border-slate-700 rounded-lg px-3 py-1.5"
      >
        <Icon name="lucide:map" class="text-sm" />
        World Map
      </NuxtLink>
    </div>

    <WorldInfo />
  </div>
</template>
