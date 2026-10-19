/**
 * The pre-flight quota guard, and the Jakarta day/month boundaries it and the
 * usage report share.
 *
 * Ported from internal/modules/llm/service/quota.go.
 */

import { ErrInvalidRequest, ErrQuotaExceeded, llmError } from "../domain/provider.ts";
import type { Service } from "./service.ts";

/** The first moment of the current day in Jakarta, which is where the customers are. */
export function dayStart(now: Date): Date {
  const parts = jakartaParts(now);
  return jakartaMidnight(parts.year, parts.month, parts.day);
}

/** The first moment of the current quota period: the start of the month in Jakarta. */
export function periodStart(now: Date): Date {
  const parts = jakartaParts(now);
  return jakartaMidnight(parts.year, parts.month, 1);
}

/** The Jakarta calendar date of an instant. WIB is a fixed +07:00, with no DST. */
export function jakartaParts(now: Date): { year: number; month: number; day: number } {
  const shifted = new Date(now.getTime() + 7 * 60 * 60 * 1000);
  return {
    year: shifted.getUTCFullYear(),
    month: shifted.getUTCMonth() + 1,
    day: shifted.getUTCDate(),
  };
}

/** The UTC instant of midnight in Jakarta on a calendar date. */
export function jakartaMidnight(year: number, month: number, day: number): Date {
  return new Date(Date.UTC(year, month - 1, day, 0, 0, 0) - 7 * 60 * 60 * 1000);
}

/**
 * Rejects a call when the workspace has spent its allowance. It is the
 * pre-flight guard: the provider is not called, so nothing is billed.
 *
 * Two bounds are asked, because the product sells two. The period allowance
 * stops a workspace from spending a month's tokens; the daily cost ceiling stops
 * one runaway task from spending a month in an afternoon. A zero on either means
 * unlimited, and quotaEnabled=false skips both for local development.
 */
export async function check(
  s: Service,
  ctx: AbortSignal | undefined,
  workspaceId: string,
): Promise<void> {
  if (!s.settings.quotaEnabled) return;
  if (!workspaceId) throw llmError(ErrInvalidRequest, "workspace is required");
  if (!s.deps.quota) {
    // Running with the check enabled but no policy would silently allow
    // everything, so it is an error instead.
    throw new Error("llm: quota is enabled but no policy is configured");
  }

  await checkPeriodAllowance(s, ctx, workspaceId);
  await checkDailyCost(s, ctx, workspaceId);
}

/** Enforces the token allowance of the billing period. */
async function checkPeriodAllowance(
  s: Service,
  ctx: AbortSignal | undefined,
  workspaceId: string,
): Promise<void> {
  const allowance = await s.deps.quota!.allowance(ctx, workspaceId);
  if (allowance <= 0) return;

  const spent = await spentTokens(s, ctx, workspaceId);
  if (spent >= allowance) {
    throw llmError(
      ErrQuotaExceeded,
      `${spent} dari ${allowance} token terpakai pada periode ini`,
    );
  }
}

/**
 * Enforces the day's cost ceiling.
 *
 * The ceiling is checked here as well as at the task boundary on purpose: a task
 * stops at a clean round boundary, and this is what stops the calls inside a
 * round once the day is spent.
 */
async function checkDailyCost(
  s: Service,
  ctx: AbortSignal | undefined,
  workspaceId: string,
): Promise<void> {
  const allowance = await s.deps.quota!.dailyCostAllowance(ctx, workspaceId);
  if (allowance <= 0) return;

  const spent = await spentToday(s, ctx, workspaceId);
  if (spent >= allowance) {
    throw llmError(
      ErrQuotaExceeded,
      `${spent} dari ${allowance} micro-rupiah terpakai hari ini`,
    );
  }
}

/**
 * The cost spent today: the counter in Redis is the fast path, and the daily
 * aggregation is the durable fallback, so an outage does not hand every
 * workspace a fresh ceiling.
 */
async function spentToday(
  s: Service,
  ctx: AbortSignal | undefined,
  workspaceId: string,
): Promise<number> {
  if (s.deps.counter) {
    try {
      return await s.deps.counter.spentCostToday(ctx, workspaceId);
    } catch (err) {
      if (!s.deps.reader) throw new Error(`llm: read today's cost: ${errorText(err)}`);
    }
  }

  if (!s.deps.reader) throw new Error("llm: no usage reader is configured");

  try {
    return await s.deps.reader.costSince(ctx, workspaceId, dayStart(s.now()));
  } catch (err) {
    throw new Error(`llm: sum the day's cost: ${errorText(err)}`);
  }
}

/**
 * The live token total: the counter in Redis is the fast path; when it is
 * unavailable the ledger answers instead.
 */
async function spentTokens(
  s: Service,
  ctx: AbortSignal | undefined,
  workspaceId: string,
): Promise<number> {
  if (s.deps.counter) {
    try {
      return await s.deps.counter.spent(ctx, workspaceId);
    } catch (err) {
      if (!s.deps.reader) throw new Error(`llm: read live spend: ${errorText(err)}`);
    }
  }

  if (!s.deps.reader) throw new Error("llm: no usage reader is configured");

  try {
    return await s.deps.reader.sumSince(ctx, workspaceId, periodStart(s.now()));
  } catch (err) {
    throw new Error(`llm: sum ledger usage: ${errorText(err)}`);
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
