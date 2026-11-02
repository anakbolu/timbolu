/**
 * Knowledge base helpers and limits.
 */

/** KNOWLEDGE_LIST_LIMIT is the maximum chunks the backend will return in one page. */
export const KNOWLEDGE_LIST_LIMIT = 200;

/**
 * formatKnowledgeCount formats the knowledge count label.
 *
 * When the returned chunks reach the server limit, we must not pretend the
 * count is the complete total because older chunks exist on the server.
 */
export function formatKnowledgeCount(count: number, limit: number = KNOWLEDGE_LIST_LIMIT): string {
  if (count >= limit) {
    return `menampilkan ${count} potongan terbaru`;
  }
  return `${count} potongan`;
}
