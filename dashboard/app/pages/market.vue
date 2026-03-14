<script setup lang="ts">
</script>

<template>
  <div class="space-y-6">
    <div>
      <h2 class="text-2xl font-bold text-slate-100">Market</h2>
      <p class="text-sm text-slate-500 mt-1">Prices, strategies, and optimizer performance</p>
    </div>

    <!-- Prices (full width) -->
    <PriceTable />

    <!-- Strategy + Optimizer (side by side) -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-6">
      <StrategyComparison />
      <OptimizerLog />
    </div>
  </div>
</template>
