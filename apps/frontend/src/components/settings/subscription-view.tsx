"use client";

/**
 * The subscription screen: which package the workspace is on, how much of it is spent,
 * and what it owes.
 *
 * Three things are deliberate here:
 *
 * - the usage figure and the warning state are read from the backend rather than
 *   computed from the period's dates. The backend derives both from the same times the
 *   quota check reads, and a screen that recomputed them would eventually disagree with
 *   the thing that refuses a model call.
 * - the bills are listed with what a person acts on to pay them, which for this product
 *   is a virtual account number rather than a link. A "Bayar sekarang" button that opened
 *   nothing would be worse than the number itself.
 * - changing a package says when it takes effect. It is the next period, and a screen
 *   that did not say so would look like it had ignored the click.
 */

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { useWorkspace } from "@/components/dashboard/workspace-data";
import {
  ApiResult,
  BillingPlan,
  BillingSnapshot,
  Invoice,
  billingSnapshot,
  buyTopUp,
  changeBillingPlan,
  listBillingPlans,
  payInvoice,
} from "@/lib/api";

const CARD = "rounded-md border border-bolu-border bg-bolu-surface";
/**
 * PAGE is the frame a screen inside the shell uses.
 *
 * It is the dashboard's own padding rather than the settings pages': 28px at the sides and
 * 24px at the top, which is what keeps a screen from pressing against the sidebar. The
 * settings screens use 20px, and that reads as cramped in this shell — the sidebar is
 * 270px of white against a white page, so the gap is the only thing separating them.
 */
const PAGE = "mx-auto w-full max-w-5xl p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]";
const HEADING = "font-display text-[17px] font-semibold";

const STATUS_LABEL: Record<string, string> = {
  trial: "Masa coba",
  active: "Aktif",
  past_due: "Menunggu pembayaran",
  canceled: "Dibatalkan",
};

const ENTITLEMENT_LABEL: Record<string, string> = {
  active: "Berjalan normal",
  grace: "Masa tenggang",
  read_only: "Mode baca saja",
};

const INVOICE_LABEL: Record<string, string> = {
  pending: "Menunggu pembayaran",
  paid: "Lunas",
  failed: "Gagal",
  expired: "Kedaluwarsa",
};

const KIND_LABEL: Record<string, string> = {
  subscription: "Langganan",
  topup: "Paket tambahan",
};

/** formatRupiah renders the rupiah the API reports. */
function formatRupiah(amount: number): string {
  return new Intl.NumberFormat("id-ID", {
    style: "currency",
    currency: "IDR",
    maximumFractionDigits: 0,
  }).format(amount);
}

/** formatTokens renders a token count with thousands separators. */
function formatTokens(tokens: number): string {
  return new Intl.NumberFormat("id-ID").format(tokens);
}

/** formatDay renders a date the way a person reads it. */
function formatDay(value: string): string {
  if (!value) {
    return "—";
  }
  return new Intl.DateTimeFormat("id-ID", {
    day: "numeric",
    month: "long",
    year: "numeric",
  }).format(new Date(value));
}

/**
 * packageLine describes a package in one line: what it costs, what it grants, and what
 * it allows. A zero on any of them is "tanpa batas" rather than "nol", because a package
 * that bounds nothing is written as a zero and reading it as a cap of none would be
 * wrong in the direction that matters.
 */
function packageLine(plan: BillingPlan): string {
  const parts: string[] = [];
  parts.push(plan.monthly_price_idr > 0 ? `${formatRupiah(plan.monthly_price_idr)} / ${plan.period_days} hari` : "Gratis");
  parts.push(plan.token_quota > 0 ? `${formatTokens(plan.token_quota)} token` : "token tanpa batas");
  parts.push(plan.max_agents > 0 ? `${plan.max_agents} Bolu` : "Bolu tanpa batas");
  parts.push(plan.max_integrations > 0 ? `${plan.max_integrations} integrasi` : "integrasi tanpa batas");
  parts.push(plan.max_members > 0 ? `${plan.max_members} anggota` : "anggota tanpa batas");
  return parts.join(" · ");
}

/** allowanceLine is what the period grants, which is the package plus what was bought. */
function allowanceLine(snapshot: BillingSnapshot): string {
  const { subscription } = snapshot;
  const granted = subscription.tokens_granted + subscription.tokens_topped_up;
  if (granted <= 0) {
    return "Paket ini tidak membatasi token.";
  }
  const topped = subscription.tokens_topped_up > 0 ? ` (termasuk ${formatTokens(subscription.tokens_topped_up)} tambahan)` : "";
  return `${formatTokens(subscription.tokens_used)} dari ${formatTokens(granted)} token terpakai${topped}.`;
}

export function SubscriptionView() {
  // The workspace comes from the layout's store rather than from this screen's own
  // refresh, and that is not a convenience.
  //
  // The store has already restored the session and resolved the active workspace by the
  // time a screen below it renders. A screen that refreshed again would send a request
  // carrying the refresh token the first one had just rotated, and the server reads a
  // reused refresh token as a stolen one: it revokes the whole session. Three refreshes
  // on one mount is a signed-out screen that did nothing wrong — which is what this
  // screen did before it read the store instead.
  const { ready, workspaceID } = useWorkspace();

  const [snapshot, setSnapshot] = useState<BillingSnapshot | null>(null);
  const [plans, setPlans] = useState<BillingPlan[]>([]);
  const [busy, setBusy] = useState(false);
  // missing is the workspace that has no subscription at all, which the API answers
  // with a 404 rather than an empty snapshot. It is its own state because the server's
  // message for it names the domain error, and showing that would be a leak rather than
  // an explanation a person can act on.
  const [missing, setMissing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [paying, setPaying] = useState<Invoice | null>(null);

  const load = useCallback(async (workspaceId: string) => {
    const [snapshotResult, planResult] = await Promise.all([
      billingSnapshot(workspaceId),
      listBillingPlans(workspaceId),
    ]);

    if (!snapshotResult.ok) {
      // A workspace with no subscription is not a failure of this screen: the plan list
      // it was loaded beside still describes what could be chosen, so the screen offers
      // that and a retry instead of the server's sentence about a missing record.
      if (snapshotResult.status === 404 && snapshotResult.code === "billing_not_found") {
        setMissing(true);
        setError(null);
        setSnapshot(null);
        setPlans(planResult.ok ? planResult.data : []);
        return;
      }
      setError(snapshotResult.message);
      return;
    }

    setSnapshot(snapshotResult.data);
    setPlans(planResult.ok ? planResult.data : []);
    setMissing(false);

    // A load that worked clears whatever an earlier one said, so a refusal a retry has
    // already fixed never sits above a screen showing the package correctly.
    setError(null);
  }, []);

  useEffect(() => {
    if (!ready || !workspaceID) {
      return;
    }
    void (async () => {
      await load(workspaceID);
    })();
  }, [ready, workspaceID, load]);

  function report<T>(result: ApiResult<T>): result is Extract<ApiResult<T>, { ok: true }> {
    if (result.ok) {
      return true;
    }
    setError(result.message);
    return false;
  }

  function run(operation: () => Promise<void>) {
    void (async () => {
      setBusy(true);
      setError(null);
      setNotice(null);
      await operation();
      setBusy(false);
    })();
  }

  function choose(key: string) {
    if (!workspaceID || key === snapshot?.plan.key) {
      return;
    }
    run(async () => {
      const result = await changeBillingPlan(workspaceID, key);
      if (!report(result)) {
        return;
      }
      setSnapshot(result.data);
      setNotice(
        result.data.subscription.status === "trial"
          ? "Paket diubah. Masa coba berjalan dengan paket baru sampai periodenya habis."
          : "Paket diubah dan berlaku mulai periode berikutnya, setelah tagihannya dibayar.",
      );
      await load(workspaceID);
    });
  }

  function topUp(key: string) {
    if (!workspaceID) {
      return;
    }
    run(async () => {
      const result = await buyTopUp(workspaceID, key);
      if (!report(result)) {
        return;
      }
      setPaying(result.data);
      setNotice("Tagihan paket tambahan sudah terbit. Selesaikan pembayarannya supaya tokennya masuk.");
      await load(workspaceID);
    });
  }

  function pay(invoice: Invoice) {
    if (!workspaceID) {
      return;
    }
    run(async () => {
      const result = await payInvoice(workspaceID, invoice.id);
      if (!report(result)) {
        return;
      }
      setPaying(result.data);
      if (result.data.payment_instructions) {
        setNotice("Ikuti instruksi pembayaran di bawah, lalu tunggu konfirmasi dari penyedia pembayaran.");
      } else {
        setNotice(
          "Deployment ini belum punya penyedia pembayaran online: transfer ke rekening usaha lalu minta konfirmasi.",
        );
      }
      await load(workspaceID);
    });
  }

  if (error && !snapshot) {
    return (
      <main className={PAGE}>
        <Alert tone="error">{error}</Alert>
        <Link href="/settings" className="text-bolu-muted underline">
          Balik ke pengaturan
        </Link>
      </main>
    );
  }

  if (missing) {
    return (
      <main className={PAGE}>
        <section className={`${CARD} p-[22px]`}>
          <h1 className={HEADING}>Belum ada langganan aktif</h1>
          <p className="mt-2 text-bolu-muted">
            Workspace ini belum punya paket aktif. Paket percobaan biasanya dibuat otomatis saat workspace dibuat,
            jadi kalau layar ini muncul terus, muat ulang dulu.
          </p>

          {plans.length > 0 ? (
            <>
              <h2 className="mt-5 text-[13px] text-bolu-muted">Paket yang tersedia</h2>
              <ul className="mt-2 list-none space-y-2 p-0">
                {plans.map((plan) => (
                  <li key={plan.key} className="rounded-md border border-bolu-border p-4">
                    <div className="font-semibold">{plan.name}</div>
                    <div className="mt-1 text-[13px] text-bolu-muted">{packageLine(plan)}</div>
                  </li>
                ))}
              </ul>
            </>
          ) : null}

          <button
            type="button"
            disabled={busy}
            onClick={() => {
              if (!workspaceID) {
                return;
              }
              run(async () => {
                await load(workspaceID);
              });
            }}
            className="mt-5 min-h-10 rounded-full border-2 border-bolu-ink px-4 font-bold text-bolu-ink disabled:opacity-50"
          >
            Muat ulang
          </button>
        </section>
      </main>
    );
  }

  if (!snapshot) {
    return (
      <main className={PAGE}>
        <p className="text-bolu-muted">
          {ready ? "Memuat langganan…" : "Menyiapkan workspace…"}
        </p>
      </main>
    );
  }

  const { subscription } = snapshot;
  const owed = snapshot.outstanding;

  return (
    <main className={PAGE}>
      <div>
        <h1 className="font-display text-3xl font-semibold">Langganan</h1>
        <p className="mt-1 text-bolu-muted">
          Paket workspace, pemakaian token periode ini, dan tagihan yang harus dibayar.
        </p>
      </div>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {notice ? <Alert tone="success">{notice}</Alert> : null}

      {subscription.read_only ? (
        <section className={`${CARD} border-bolu-belum/40 p-[22px]`}>
          <h2 className={HEADING}>Workspace sekarang mode baca saja</h2>
          <p className="mt-2 text-bolu-muted">
            Masa tenggang sudah lewat, jadi perubahan dan tugas baru ditolak sampai tagihannya dibayar. Membaca dan
            mengekspor data tetap jalan, dan tidak ada data yang dihapus.
          </p>
        </section>
      ) : null}

      <section className={`${CARD} p-[22px]`}>
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <div>
            <h2 className={HEADING}>{snapshot.plan.name}</h2>
            <p className="mt-1 text-bolu-muted">{packageLine(snapshot.plan)}</p>
          </div>
          <span className="rounded-full border border-bolu-border px-3 py-1 text-[12px] text-bolu-muted">
            {STATUS_LABEL[subscription.status] ?? subscription.status} · {ENTITLEMENT_LABEL[snapshot.entitlement]}
          </span>
        </div>

        <dl className="mt-5 grid list-none grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-4 p-0">
          <div>
            <dt className="text-[13px] text-bolu-muted">Periode ini</dt>
            <dd className="mt-1">
              {formatDay(subscription.current_period_start)} – {formatDay(subscription.current_period_end)}
            </dd>
          </div>
          <div>
            <dt className="text-[13px] text-bolu-muted">Pemakaian token</dt>
            <dd className="mt-1">{allowanceLine(snapshot)}</dd>
          </div>
          <div>
            <dt className="text-[13px] text-bolu-muted">Yang harus dibayar</dt>
            <dd className="mt-1">{owed.length === 0 ? "Tidak ada" : formatRupiah(subscription.outstanding_idr)}</dd>
          </div>
          <div>
            <dt className="text-[13px] text-bolu-muted">Batas anggota</dt>
            <dd className="mt-1">
              {subscription.max_members > 0 ? `${subscription.max_members} orang` : "tanpa batas"} ·{" "}
              {subscription.max_agents > 0 ? `${subscription.max_agents} Bolu` : "Bolu tanpa batas"}
            </dd>
          </div>
        </dl>

        {subscription.tokens_granted + subscription.tokens_topped_up > 0 ? (
          <div className="mt-5">
            <div className="h-2 w-full overflow-hidden rounded-full bg-bolu-panel">
              <div
                className={`h-full rounded-full ${subscription.read_only ? "bg-bolu-belum" : "bg-bolu-solid"}`}
                style={{ width: `${Math.min(subscription.used_percent, 100)}%` }}
              />
            </div>
            <p className="mt-2 text-[13px] text-bolu-muted">
              {subscription.used_percent}% terpakai
              {subscription.tokens_remaining >= 0 ? ` · sisa ${formatTokens(subscription.tokens_remaining)} token` : ""}
            </p>
          </div>
        ) : null}

        {snapshot.plan.daily_cost_micros > 0 ? (
          <p className="mt-3 text-[13px] text-bolu-muted">
            Batas biaya harian {formatRupiah(snapshot.plan.daily_cost_micros / 1_000_000)}; hari ini terpakai{" "}
            {formatRupiah(subscription.spent_today_micros / 1_000_000)}.
          </p>
        ) : null}

        {subscription.pending_plan_name ? (
          <p className="mt-3 text-[13px] text-bolu-muted">
            Paket berikutnya: <strong>{subscription.pending_plan_name}</strong>, berlaku setelah periode ini berakhir.
          </p>
        ) : null}
      </section>

      {owed.length > 0 ? (
        <section className={`${CARD} p-[22px]`}>
          <h2 className={HEADING}>Tagihan yang menunggu</h2>
          <ul className="mt-4 list-none space-y-3 p-0">
            {owed.map((invoice) => (
              <li key={invoice.id} className="rounded-md border border-bolu-border p-4">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <div>
                    <div className="font-semibold">
                      {KIND_LABEL[invoice.kind] ?? invoice.kind} · {formatRupiah(invoice.amount_idr)}
                    </div>
                    <div className="mt-1 text-[13px] text-bolu-muted">
                      Periode {formatDay(invoice.period_start)} – {formatDay(invoice.period_end)}
                      {invoice.expires_at ? ` · bayar sebelum ${formatDay(invoice.expires_at)}` : ""}
                    </div>
                  </div>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => pay(invoice)}
                    className="min-h-10 rounded-full bg-bolu-solid px-4 font-bold text-bolu-solid-ink disabled:opacity-50"
                  >
                    {invoice.payment_instructions ? "Tampilkan cara bayar" : "Minta cara bayar"}
                  </button>
                </div>
                {invoice.payment_instructions ? (
                  <p className="mt-3 rounded-md bg-bolu-panel p-3 font-mono text-[13px]">
                    {invoice.payment_instructions}
                  </p>
                ) : null}
                {invoice.last_error ? (
                  <p className="mt-2 text-[13px] text-bolu-belum">
                    Percobaan terakhir gagal: {invoice.last_error}
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {paying?.payment_instructions ? (
        <section className={`${CARD} p-[22px]`}>
          <h2 className={HEADING}>Cara bayar</h2>
          <p className="mt-2 text-bolu-muted">
            Selesaikan pembayaran {formatRupiah(paying.amount_idr)} untuk{" "}
            {KIND_LABEL[paying.kind] ?? paying.kind}. Statusnya berubah sendiri setelah penyedia pembayaran
            mengonfirmasi.
          </p>
          <p className="mt-3 rounded-md bg-bolu-panel p-3 font-mono text-[14px]">{paying.payment_instructions}</p>
        </section>
      ) : null}

      {snapshot.top_ups.length > 0 ? (
        <section className={`${CARD} p-[22px]`}>
          <h2 className={HEADING}>Paket tambahan token</h2>
          <p className="mt-2 text-bolu-muted">
            Token tambahan dipakai di periode ini dan tidak terbawa ke periode berikutnya.
          </p>
          <ul className="mt-4 grid list-none grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-3 p-0">
            {snapshot.top_ups.map((pack) => (
              <li key={pack.key} className="rounded-md border border-bolu-border p-4">
                <div className="font-semibold">{pack.name}</div>
                <div className="mt-1 text-[13px] text-bolu-muted">{formatRupiah(pack.price_idr)}</div>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => topUp(pack.key)}
                  className="mt-3 min-h-10 w-full rounded-full border-2 border-bolu-ink px-4 font-bold text-bolu-ink disabled:opacity-50"
                >
                  Beli
                </button>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <section className={`${CARD} p-[22px]`}>
        <h2 className={HEADING}>Paket yang tersedia</h2>
        <ul className="mt-4 list-none space-y-3 p-0">
          {plans.map((plan) => {
            const current = plan.key === snapshot.plan.key;
            const next = plan.key === subscription.pending_plan_key;
            return (
              <li
                key={plan.key}
                className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-bolu-border p-4"
              >
                <div>
                  <div className="font-semibold">
                    {plan.name}
                    {current ? " · sekarang" : ""}
                    {next ? " · berikutnya" : ""}
                  </div>
                  <div className="mt-1 text-[13px] text-bolu-muted">{packageLine(plan)}</div>
                </div>
                <button
                  type="button"
                  disabled={busy || current}
                  onClick={() => choose(plan.key)}
                  className="min-h-10 rounded-full border-2 border-bolu-ink px-4 font-bold text-bolu-ink disabled:opacity-40"
                >
                  {current ? "Dipakai" : "Pilih"}
                </button>
              </li>
            );
          })}
        </ul>
      </section>

      <section className={`${CARD} p-[22px]`}>
        <h2 className={HEADING}>Riwayat tagihan</h2>
        {snapshot.invoices.length === 0 ? (
          <p className="mt-2 text-bolu-muted">Belum ada tagihan.</p>
        ) : (
          <ul className="mt-4 list-none space-y-2 p-0">
            {snapshot.invoices.map((invoice) => (
              <li key={invoice.id} className="flex flex-wrap items-baseline justify-between gap-2 border-b border-bolu-border pb-2 last:border-0">
                <div>
                  <span className="font-semibold">{formatRupiah(invoice.amount_idr)}</span>
                  <span className="ml-2 text-[13px] text-bolu-muted">
                    {KIND_LABEL[invoice.kind] ?? invoice.kind} · {formatDay(invoice.period_start)} –{" "}
                    {formatDay(invoice.period_end)}
                  </span>
                </div>
                <span className="text-[13px] text-bolu-muted">
                  {INVOICE_LABEL[invoice.status] ?? invoice.status}
                  {invoice.paid_at ? ` · ${formatDay(invoice.paid_at)}` : ""}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </main>
  );
}
