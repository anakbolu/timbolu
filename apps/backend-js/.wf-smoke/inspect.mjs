import { Client, Connection } from "@temporalio/client";
const client = new Client({ connection: await Connection.connect({ address: "localhost:7233" }) });
const list = client.workflow.list({ query: "WorkflowType = 'SmokeWorkflow'" });
for await (const w of list) {
  const h = client.workflow.getHandle(w.workflowId, w.runId);
  const hist = await h.fetchHistory();
  const types = hist.events.map((e) => e.eventType);
  const failed = hist.events.find((e) => e.workflowExecutionFailedEventAttributes);
  console.log(w.workflowId, "|", types.join(","));
  if (failed) console.log("  failure:", JSON.stringify(failed.workflowExecutionFailedEventAttributes.failure).slice(0, 700));
}
process.exit(0);
