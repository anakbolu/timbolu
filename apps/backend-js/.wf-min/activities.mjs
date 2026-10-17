// .wf-min/index.ts
import { CancellationScope, proxyActivities } from "@temporalio/workflow";
import { ApplicationFailure } from "@temporalio/common";
async function MinPlainFinally() {
  const acts = proxyActivities({ startToCloseTimeout: 1e4, retry: { maximumAttempts: 3 } });
  try {
    await acts.Fail();
  } catch {} finally {
    try {
      await acts.Fail();
    } catch {}
  }
  throw new Error("plain finally");
}
async function MinScopeNoThrow() {
  const acts = proxyActivities({ startToCloseTimeout: 1e4, retry: { maximumAttempts: 3 } });
  try {
    await acts.Fail();
  } catch {
    return "caught";
  } finally {
    await CancellationScope.nonCancellable(async () => {
      try {
        await acts.Fail();
      } catch {}
    });
  }
}
async function MinScopeThenThrow() {
  const acts = proxyActivities({ startToCloseTimeout: 1e4, retry: { maximumAttempts: 3 } });
  let caught = false;
  try {
    await acts.Fail();
  } catch {
    caught = true;
  }
  await CancellationScope.nonCancellable(async () => {
    try {
      await acts.Fail();
    } catch {}
  });
  throw new Error(`scope then throw caught=${caught}`);
}
async function FailActivity() {
  throw ApplicationFailure.nonRetryable("boom", "BoomType");
}
export {
  FailActivity,
  MinPlainFinally,
  MinScopeNoThrow,
  MinScopeThenThrow
};
