<script setup lang="ts">
</script>

<template>
  <div class="space-y-8">
    <div>
      <h2 class="text-3xl font-bold text-slate-100">Market</h2>
      <p class="text-sm text-slate-500 mt-1.5">Prices, strategies, and optimizer performance</p>
    </div>

    <!-- Global Win/Loss (full width) -->
    <GlobalPnL />

    <!-- Prices (full width) -->
    <PriceTable />

    <!-- Strategy + Optimizer (side by side) -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-8">
      <StrategyComparison />
      <OptimizerLog />
    </div>
  </div>
</template>
