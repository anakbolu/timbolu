import { Client, Connection } from "@temporalio/client";
import { Worker, NativeConnection } from "@temporalio/worker";
import { Activities } from "./activities.mjs";

const ROOT = "/Users/robbyn/Documents/orca/timbolu/rewrite-to-js/apps/backend-js";
const queue = `vcomputer-verify-${Date.now()}`;
const native = await NativeConnection.connect({ address: "localhost:7233" });
const worker = await Worker.create({
  connection: native,
  taskQueue: queue,
  workflowsPath: `${ROOT}/.wf-smoke`,
  activities: new Activities({}),
});
const run = worker.run();
const client = new Client({ connection: await Connection.connect({ address: "localhost:7233" }) });
const handle = await client.workflow.start("SmokeWorkflow", {
  workflowId: `smoke-${Date.now()}`,
  taskQueue: queue,
  args: [{ bot_id: "b", url: "https://example.com", click_x: 1, click_y: 2, text: "hi", keep_session: false }],
  workflowExecutionTimeout: "30s",
});
try {
  await handle.result();
  console.log("UNEXPECTED success");
} catch (err) {
  console.log("failure:", err.type ?? err.name, "|", err.message);
}
await worker.shutdown();
await run.catch(() => {});
await native.close();
process.exit(0);
