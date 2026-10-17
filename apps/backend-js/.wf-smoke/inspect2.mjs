import { Client, Connection } from "@temporalio/client";
const client = new Client({ connection: await Connection.connect({ address: "localhost:7233" }) });
for await (const w of client.workflow.list({ query: "WorkflowType = 'SmokeWorkflow'" })) {
  const h = client.workflow.getHandle(w.workflowId, w.runId);
  const hist = await h.fetchHistory();
  const failed = hist.events.filter((e) => e.activityTaskFailedEventAttributes);
  if (!failed.length) continue;
  console.log(w.workflowId);
  for (const e of failed) console.log(" ", e.eventId, JSON.stringify(e.activityTaskFailedEventAttributes.failure).slice(0, 400));
  break;
}
process.exit(0);
