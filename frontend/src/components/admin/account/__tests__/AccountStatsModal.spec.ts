import { beforeEach, describe, expect, it, vi } from "vitest";
import { defineComponent, h } from "vue";
import { flushPromises, mount } from "@vue/test-utils";

import AccountStatsModal from "../AccountStatsModal.vue";

const { getStats, getImportSnapshot, revealImportSnapshot } = vi.hoisted(
  () => ({
    getStats: vi.fn(),
    getImportSnapshot: vi.fn(),
    revealImportSnapshot: vi.fn(),
  }),
);

vi.mock("@/api/admin", () => ({
  adminAPI: {
    accounts: { getStats, getImportSnapshot, revealImportSnapshot },
  },
}));

vi.mock("vue-chartjs", () => ({
  Line: defineComponent({ template: "<div />" }),
}));

vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) =>
        ({
          "admin.accounts.importSnapshot.title": "导入 JSON",
          "admin.accounts.importSnapshot.reveal": "查看完整 JSON",
          "admin.accounts.importSnapshot.hide": "隐藏完整 JSON",
          "admin.accounts.importSnapshot.confirmTitle": "确认查看完整 JSON",
          "admin.accounts.importSnapshot.confirmMessage":
            "完整内容可能包含敏感凭据。",
          "admin.accounts.importSnapshot.none": "无导入记录",
        })[key] || key,
    }),
  };
});

const ConfirmDialogStub = defineComponent({
  props: ["show", "title", "message"],
  emits: ["confirm", "cancel"],
  setup(props, { emit }) {
    return () =>
      props.show
        ? h("div", { class: "confirm-dialog" }, [
            h("span", props.title),
            h(
              "button",
              { class: "confirm-reveal", onClick: () => emit("confirm") },
              "确认",
            ),
          ])
        : null;
  },
});

const stats = {
  history: [],
  summary: {
    total_cost: 0,
    total_user_cost: 0,
    total_standard_cost: 0,
    total_requests: 0,
	 total_tokens: 0,
    avg_daily_cost: 0,
    avg_daily_user_cost: 0,
    avg_daily_requests: 0,
	 avg_daily_tokens: 0,
	 avg_duration_ms: 0,
    actual_days_used: 0,
    today: null,
    highest_cost_day: null,
    highest_request_day: null,
  },
  models: [],
  endpoints: [],
  upstream_endpoints: [],
};

const mountModal = () =>
  mount(AccountStatsModal, {
    props: {
      show: false,
      account: { id: 88, name: "账号 88", status: "active" } as any,
    },
    global: {
      stubs: {
        BaseDialog: { template: "<div><slot /></div>" },
        LoadingSpinner: true,
        ModelDistributionChart: true,
        EndpointDistributionChart: true,
        Icon: true,
        ConfirmDialog: ConfirmDialogStub,
      },
    },
  });

describe("AccountStatsModal import snapshot", () => {
  beforeEach(() => {
    getStats.mockReset().mockResolvedValue(stats);
    getImportSnapshot.mockReset().mockResolvedValue({
      exists: true,
      batch_id: "batch-88",
      imported_at: "2026-07-15T01:00:00Z",
      json: { credentials: { refresh_token: "sup***en" } },
    });
    revealImportSnapshot.mockReset().mockResolvedValue({
      exists: true,
      batch_id: "batch-88",
      imported_at: "2026-07-15T01:00:00Z",
      json: { credentials: { refresh_token: "super-secret-token" } },
    });
  });

  it("默认脱敏，确认后临时显示完整 JSON，并可清除", async () => {
    const wrapper = mountModal();
	await wrapper.setProps({ show: true });
    await flushPromises();

    expect(getImportSnapshot).toHaveBeenCalledWith(88);
    expect(wrapper.text()).toContain("sup***en");
    expect(wrapper.text()).not.toContain("super-secret-token");

    await wrapper
      .get('[data-testid="reveal-import-snapshot"]')
      .trigger("click");
    expect(wrapper.find(".confirm-dialog").exists()).toBe(true);
    await wrapper.get(".confirm-reveal").trigger("click");
    await flushPromises();

    expect(revealImportSnapshot).toHaveBeenCalledWith(88);
    expect(wrapper.text()).toContain("super-secret-token");

    await wrapper.get('[data-testid="hide-import-snapshot"]').trigger("click");
    expect(wrapper.text()).not.toContain("super-secret-token");

    await wrapper.setProps({ show: false });
    await flushPromises();
    expect(wrapper.text()).not.toContain("super-secret-token");
  });
});
