import { redirect } from "next/navigation";

/**
 * The task history screen is gone: its columns and reasons are an operator's view of
 * the system, and a person meets one task from the dashboard card or the "Lihat
 * tugasnya" link in a conversation instead.
 *
 * The redirect is here so an old bookmark or a link typed from memory lands on Dasbor
 * rather than on a 404. One task's page (`/tasks/[taskID]`) stays: that is where those
 * links lead.
 */
export default function TasksPage() {
  redirect("/dashboard");
}
