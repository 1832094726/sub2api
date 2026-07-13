import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import BulkHealthCheckModal from '../BulkHealthCheckModal.vue'

const { runAccountHealthChecks } = vi.hoisted(() => ({
  runAccountHealthChecks: vi.fn()
}))

vi.mock('@/services/accountHealthCheck', () => ({ runAccountHealthChecks }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('BulkHealthCheckModal', () => {
  beforeEach(() => {
    runAccountHealthChecks.mockReset()
    runAccountHealthChecks.mockResolvedValue({
      total: 2,
      completed: 2,
      success: 1,
      failed: 1,
      cancelled: false,
      results: []
    })
  })

  it('starts checks for the supplied account IDs and emits completion', async () => {
    const wrapper = mount(BulkHealthCheckModal, {
      props: { show: true, accountIds: [11, 22] },
      global: {
        stubs: {
          BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
          Icon: true
        }
      }
    })

    await wrapper.get('[data-test="start-health-check"]').trigger('click')
    await flushPromises()

    expect(runAccountHealthChecks).toHaveBeenCalledWith(
      [11, 22],
      expect.objectContaining({ concurrency: 4, onProgress: expect.any(Function) })
    )
    expect(wrapper.emitted('completed')).toHaveLength(1)
  })
})
