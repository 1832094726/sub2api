<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.healthCheck.title')"
    width="wide"
    @close="handleClose"
  >
    <div class="space-y-5">
      <div class="flex items-start justify-between gap-4">
        <div>
          <p class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.healthCheck.targetCount', { count: accountIds.length }) }}
          </p>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.healthCheck.description') }}
          </p>
        </div>
        <span
          class="inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-medium"
          :class="statusClass"
        >
          <Icon :name="running ? 'refresh' : completed ? 'check' : 'play'" size="xs" :class="running ? 'animate-spin' : ''" />
          {{ statusLabel }}
        </span>
      </div>

      <div class="space-y-2">
        <div class="flex items-center justify-between text-xs text-gray-500 dark:text-gray-400">
          <span>{{ t('admin.accounts.healthCheck.progress') }}</span>
          <span class="font-mono">{{ summary.completed }} / {{ summary.total }}</span>
        </div>
        <div class="h-2 overflow-hidden rounded bg-gray-200 dark:bg-dark-600">
          <div
            class="h-full bg-primary-500 transition-[width] duration-300"
            :style="{ width: `${progressPercent}%` }"
          />
        </div>
      </div>

      <div class="grid grid-cols-3 gap-3">
        <div class="rounded-md border border-gray-200 px-3 py-3 dark:border-dark-600">
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.healthCheck.completed') }}</div>
          <div class="mt-1 text-xl font-semibold text-gray-900 dark:text-white">{{ summary.completed }}</div>
        </div>
        <div class="rounded-md border border-emerald-200 px-3 py-3 dark:border-emerald-800/60">
          <div class="text-xs text-emerald-600 dark:text-emerald-400">{{ t('admin.accounts.healthCheck.success') }}</div>
          <div class="mt-1 text-xl font-semibold text-emerald-700 dark:text-emerald-300">{{ summary.success }}</div>
        </div>
        <div class="rounded-md border border-red-200 px-3 py-3 dark:border-red-800/60">
          <div class="text-xs text-red-600 dark:text-red-400">{{ t('admin.accounts.healthCheck.failed') }}</div>
          <div class="mt-1 text-xl font-semibold text-red-700 dark:text-red-300">{{ summary.failed }}</div>
        </div>
      </div>

      <div v-if="summary.results.length" class="overflow-hidden rounded-md border border-gray-200 dark:border-dark-600">
        <div class="max-h-64 overflow-y-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-600">
            <thead class="sticky top-0 bg-gray-50 dark:bg-dark-700">
              <tr>
                <th class="px-3 py-2 text-left text-xs font-medium text-gray-500">{{ t('admin.accounts.healthCheck.account') }}</th>
                <th class="px-3 py-2 text-left text-xs font-medium text-gray-500">{{ t('admin.accounts.healthCheck.result') }}</th>
                <th class="px-3 py-2 text-left text-xs font-medium text-gray-500">{{ t('admin.accounts.healthCheck.detail') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-800">
              <tr v-for="result in [...summary.results].reverse()" :key="result.accountId">
                <td class="whitespace-nowrap px-3 py-2 font-mono text-xs text-gray-600 dark:text-gray-300">#{{ result.accountId }}</td>
                <td class="px-3 py-2">
                  <span :class="result.success ? 'text-emerald-600 dark:text-emerald-400' : 'text-red-600 dark:text-red-400'">
                    {{ result.success ? t('admin.accounts.healthCheck.success') : t('admin.accounts.healthCheck.failed') }}
                  </span>
                </td>
                <td class="max-w-sm truncate px-3 py-2 text-xs text-gray-500 dark:text-gray-400" :title="result.error">
                  {{ result.error || '-' }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <button v-if="running" class="btn btn-secondary" @click="cancel">
          {{ t('common.cancel') }}
        </button>
        <button v-else class="btn btn-secondary" @click="handleClose">
          {{ t('common.close') }}
        </button>
        <button
          data-test="start-health-check"
          class="btn btn-primary"
          :disabled="running || accountIds.length === 0"
          @click="start"
        >
          <Icon name="play" size="sm" class="mr-1.5" />
          {{ completed ? t('admin.accounts.healthCheck.runAgain') : t('admin.accounts.healthCheck.start') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import BaseDialog from '@/components/common/BaseDialog.vue'
import { Icon } from '@/components/icons'
import {
  runAccountHealthChecks,
  type AccountHealthCheckSummary
} from '@/services/accountHealthCheck'

const props = defineProps<{
  show: boolean
  accountIds: number[]
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'completed', summary: AccountHealthCheckSummary): void
}>()

const { t } = useI18n()
const running = ref(false)
const completed = ref(false)
let controller: AbortController | null = null
const summary = reactive<AccountHealthCheckSummary>(emptySummary())

function emptySummary(): AccountHealthCheckSummary {
  return {
    total: props.accountIds.length,
    completed: 0,
    success: 0,
    failed: 0,
    cancelled: false,
    results: []
  }
}

function reset() {
  Object.assign(summary, emptySummary())
  completed.value = false
}

watch(() => props.show, show => {
  if (show && !running.value) reset()
})
watch(() => props.accountIds, () => {
  if (!running.value) reset()
})

const progressPercent = computed(() => summary.total ? Math.round((summary.completed / summary.total) * 100) : 0)
const statusLabel = computed(() => {
  if (running.value) return t('admin.accounts.healthCheck.running')
  if (completed.value) return t('admin.accounts.healthCheck.finished')
  return t('admin.accounts.healthCheck.ready')
})
const statusClass = computed(() => {
  if (running.value) return 'bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
  if (completed.value) return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
})

async function start() {
  reset()
  running.value = true
  controller = new AbortController()
  const finalSummary = await runAccountHealthChecks(props.accountIds, {
    concurrency: 4,
    signal: controller.signal,
    onProgress: (_result, current) => Object.assign(summary, current)
  })
  Object.assign(summary, finalSummary)
  running.value = false
  completed.value = !finalSummary.cancelled
  controller = null
  emit('completed', finalSummary)
}

function cancel() {
  controller?.abort()
}

function handleClose() {
  if (running.value) cancel()
  emit('close')
}
</script>
