import { Client, Connection } from "@temporalio/client";
import { Worker, NativeConnection } from "@temporalio/worker";
import { FailActivity } from "./activities.mjs";
const ROOT = "/Users/robbyn/Documents/orca/timbolu/rewrite-to-js/apps/backend-js";
const queue = `min-${Date.now()}`;
const native = await NativeConnection.connect({ address: "localhost:7233" });
const worker = await Worker.create({ connection: native, taskQueue: queue, workflowsPath: `${ROOT}/.wf-min`, activities: { Fail: FailActivity } });
const run = worker.run();
const client = new Client({ connection: await Connection.connect({ address: "localhost:7233" }) });
for (const name of ["MinPlainFinally", "MinScopeNoThrow", "MinScopeThenThrow"]) {
  const h = await client.workflow.start(name, { workflowId: `${name}-${Date.now()}`, taskQueue: queue, args: [], workflowExecutionTimeout: "15s" });
  const t0 = Date.now();
  try { console.log(name, "UNEXPECTED", await h.result()); }
  catch (err) { console.log(name, "failed in", Date.now() - t0, "ms:", err.type ?? err.name, "|", err.message.split("\n")[0]); }
}
await worker.shutdown();
await run.catch(() => {});
await native.close();
process.exit(0);
