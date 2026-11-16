<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Workflows</h1>
    <UAlert color="neutral" variant="soft" description="Dagger-native workflows are discovered from .dagger/workflows/*.yaml on every commit." />
    <UCard class="mt-4">
      <h3 class="font-semibold">Run a workflow</h3>
      <p class="mt-1 text-[13px] text-muted">
        Manual runs use typed workflow_dispatch inputs. Protected environments pause in the approval queue.
      </p>
    </UCard>
  </div>
</template>
