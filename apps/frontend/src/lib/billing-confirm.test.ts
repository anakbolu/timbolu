import assert from "node:assert/strict";
import test from "node:test";

import type { BillingPlan, TopUpPackage } from "@/lib/api";
import {
  buildBillingConfirmation,
  buildPlanConfirmation,
  buildTopUpConfirmation,
  formatRupiah,
  formatTokens,
} from "./billing-confirm";

test("formatRupiah formats IDR currency correctly", () => {
  const formatted = formatRupiah(75000);
  assert.match(formatted, /Rp\s*75\.000/);
});

test("formatTokens formats token counts with thousand separators", () => {
  assert.equal(formatTokens(500000), "500.000");
  assert.equal(formatTokens(1000000), "1.000.000");
});

test("buildTopUpConfirmation includes formatted price, tokens, and non-empty labels", () => {
  const pack: TopUpPackage = {
    key: "tambahan-500k",
    name: "500 ribu token",
    token_amount: 500000,
    price_idr: 75000,
  };

  const confirmation = buildTopUpConfirmation(pack);

  assert.equal(confirmation.kind, "topup");
  assert.ok(confirmation.actionLabel.length > 0);
  assert.ok(confirmation.cancelLabel.length > 0);
  assert.ok(confirmation.title.length > 0);
  assert.ok(confirmation.description.length > 0);

  // Label contains formatted price and action verb
  assert.match(confirmation.actionLabel, /^Beli\s+/);
  assert.match(confirmation.actionLabel, /Rp\s*75\.000/);

  // Top-up mentions token amount and pack name
  assert.match(confirmation.actionLabel, /500 ribu token/);
  assert.match(confirmation.description, /500\.000 token/);
  assert.match(confirmation.description, /Rp\s*75\.000/);
  assert.match(confirmation.periodDescription, /periode berjalan/i);
});

test("buildTopUpConfirmation never returns empty labels even for empty input", () => {
  const confirmation = buildTopUpConfirmation({});

  assert.ok(confirmation.actionLabel.trim().length > 0);
  assert.ok(confirmation.cancelLabel.trim().length > 0);
  assert.ok(confirmation.title.trim().length > 0);
  assert.ok(confirmation.description.trim().length > 0);
  assert.match(confirmation.actionLabel, /^Beli\s+/);
  assert.match(confirmation.actionLabel, /Rp\s*0/);
});

test("buildPlanConfirmation includes formatted price, period, and trial effect", () => {
  const plan: BillingPlan = {
    key: "keluarga",
    name: "Paket Keluarga",
    monthly_price_idr: 149000,
    token_quota: 2000000,
    daily_cost_micros: 60000000,
    period_days: 30,
    max_agents: 6,
    max_integrations: 3,
    max_members: 6,
    is_trial: false,
  };

  const confirmation = buildPlanConfirmation(plan, "trial");

  assert.equal(confirmation.kind, "plan");
  assert.ok(confirmation.actionLabel.length > 0);
  assert.ok(confirmation.cancelLabel.length > 0);
  assert.ok(confirmation.title.length > 0);
  assert.ok(confirmation.description.length > 0);

  // Label contains action verb, plan name, and formatted price
  assert.match(confirmation.actionLabel, /^Pilih\s+Paket Keluarga/);
  assert.match(confirmation.actionLabel, /Rp\s*149\.000/);

  // Trial effect description: active immediately
  assert.match(confirmation.effectiveDescription, /langsung berlaku/i);
  assert.match(confirmation.description, /langsung berlaku/i);
  assert.match(confirmation.periodFormatted, /30 hari/);
});

test("buildPlanConfirmation describes next-period effect when not in trial", () => {
  const plan: BillingPlan = {
    key: "rombongan",
    name: "Paket Rombongan",
    monthly_price_idr: 449000,
    token_quota: 8000000,
    daily_cost_micros: 200000000,
    period_days: 30,
    max_agents: 20,
    max_integrations: 10,
    max_members: 25,
    is_trial: false,
  };

  const confirmation = buildPlanConfirmation(plan, "active");

  assert.match(confirmation.actionLabel, /^Pilih\s+Paket Rombongan/);
  assert.match(confirmation.actionLabel, /Rp\s*449\.000/);

  // Non-trial effect: effective next period after invoice paid
  assert.match(confirmation.effectiveDescription, /mulai periode berikutnya/i);
  assert.match(confirmation.effectiveDescription, /setelah tagihan dibayar/i);
  assert.match(confirmation.description, /mulai periode berikutnya/i);
});

test("buildPlanConfirmation never returns empty labels even for empty input", () => {
  const confirmation = buildPlanConfirmation({});

  assert.ok(confirmation.actionLabel.trim().length > 0);
  assert.ok(confirmation.cancelLabel.trim().length > 0);
  assert.ok(confirmation.title.trim().length > 0);
  assert.ok(confirmation.description.trim().length > 0);
  assert.match(confirmation.actionLabel, /^Pilih\s+/);
  assert.match(confirmation.actionLabel, /Rp\s*0/);
});

test("buildBillingConfirmation dispatches correctly between topup and plan", () => {
  const topUpConf = buildBillingConfirmation({
    kind: "topup",
    pack: {
      key: "tambahan-1jt",
      name: "Tambahan 1 juta token",
      token_amount: 1000000,
      price_idr: 99000,
    },
  });
  assert.equal(topUpConf.kind, "topup");
  assert.match(topUpConf.actionLabel, /Rp\s*99\.000/);

  const planConf = buildBillingConfirmation(
    {
      kind: "plan",
      plan: {
        key: "keluarga",
        name: "Paket Keluarga",
        monthly_price_idr: 149000,
        token_quota: 2000000,
        daily_cost_micros: 60000000,
        period_days: 30,
        max_agents: 6,
        max_integrations: 3,
        max_members: 6,
        is_trial: false,
      },
    },
    "trial",
  );
  assert.equal(planConf.kind, "plan");
  assert.match(planConf.actionLabel, /Rp\s*149\.000/);
});
