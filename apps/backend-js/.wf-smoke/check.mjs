import { Activities } from "./activities.mjs";
import { ApplicationFailure } from "@temporalio/common";
const a = new Activities({});
try { await a.EnsureSession({ bot_id: "b" }); } catch (err) {
  console.log("instanceof ApplicationFailure:", err instanceof ApplicationFailure);
  console.log("nonRetryable:", err.nonRetryable, "type:", err.type, "message:", err.message);
  console.log("keys:", Object.keys(err));
}
