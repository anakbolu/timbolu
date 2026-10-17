import { Client, Connection } from "@temporalio/client";
const client = new Client({ connection: await Connection.connect({ address: "localhost:7233" }) });
for await (const w of client.workflow.list({ query: "WorkflowType = 'SmokeWorkflow'" })) {
  const h = client.workflow.getHandle(w.workflowId, w.runId);
  const hist = await h.fetchHistory();
  for (const e of hist.events) {
    const a = e.activityTaskFailedEventAttributes;
    if (!a) continue;
    const f = a.failure;
    console.log("event", String(e.eventId), "attempt", String(a.retryState), "nonRetryable:", f?.applicationFailureInfo?.nonRetryable, "type:", f?.applicationFailureInfo?.type, "msg:", f?.message);
    console.log("  cause:", f?.cause?.message, "causeNonRetryable:", f?.cause?.applicationFailureInfo?.nonRetryable);
  }
  break;
}
process.exit(0);
