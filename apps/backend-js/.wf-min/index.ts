import { CancellationScope, proxyActivities } from "@temporalio/workflow";
import { ApplicationFailure } from "@temporalio/common";

interface A { Fail(): Promise<void>; }

export async function MinPlainFinally(): Promise<string> {
  const acts = proxyActivities<A>({ startToCloseTimeout: 10_000, retry: { maximumAttempts: 3 } });
  try {
    await acts.Fail();
  } catch {
    // ignored
  } finally {
    try { await acts.Fail(); } catch { /* ignored */ }
  }
  throw new Error("plain finally");
}

export async function MinScopeNoThrow(): Promise<string> {
  const acts = proxyActivities<A>({ startToCloseTimeout: 10_000, retry: { maximumAttempts: 3 } });
  try {
    await acts.Fail();
  } catch {
    return "caught";
  } finally {
    await CancellationScope.nonCancellable(async () => {
      try { await acts.Fail(); } catch { /* ignored */ }
    });
  }
}

export async function MinScopeThenThrow(): Promise<string> {
  const acts = proxyActivities<A>({ startToCloseTimeout: 10_000, retry: { maximumAttempts: 3 } });
  let caught = false;
  try {
    await acts.Fail();
  } catch {
    caught = true;
  }
  await CancellationScope.nonCancellable(async () => {
    try { await acts.Fail(); } catch { /* ignored */ }
  });
  throw new Error(`scope then throw caught=${caught}`);
}

export async function FailActivity(): Promise<void> {
  throw ApplicationFailure.nonRetryable("boom", "BoomType");
}
