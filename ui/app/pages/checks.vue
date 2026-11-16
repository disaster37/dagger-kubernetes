<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Cloud Checks</h1>
    <UAlert color="neutral" variant="soft" description="Check runs are dispatched by forge webhooks. Fork PRs wait for approval and never receive secrets." />
    <UCard class="mt-4">
      <h3 class="font-semibold">Enable checks</h3>
      <p class="mt-1 text-[13px] text-muted">
        Run <code>dagger cloud checks on</code> in a repository, or configure an integration under Organizations.
      </p>
    </UCard>
  </div>
</template>
