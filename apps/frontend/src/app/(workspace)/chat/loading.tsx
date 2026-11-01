export default function ChatLoading() {
  return (
    <div className="flex h-full min-h-0 flex-1 animate-pulse overflow-hidden bg-bolu-bg">
      {/* Sidebar percakapan placeholder */}
      <div className="hidden w-72 flex-col border-r border-bolu-border bg-bolu-surface p-4 md:flex">
        <div className="h-9 w-full rounded-xl bg-bolu-panel" />
        <div className="mt-4 flex flex-col gap-3">
          <div className="h-14 w-full rounded-xl bg-bolu-panel/60" />
          <div className="h-14 w-full rounded-xl bg-bolu-panel/40" />
          <div className="h-14 w-full rounded-xl bg-bolu-panel/30" />
        </div>
      </div>

      {/* Area pesan */}
      <div className="flex flex-1 flex-col justify-between p-6">
        <div className="flex items-center gap-3 border-b border-bolu-border pb-4">
          <div className="size-10 rounded-full bg-bolu-panel" />
          <div className="flex flex-col gap-1.5">
            <div className="h-4 w-32 rounded bg-bolu-panel" />
            <div className="h-3 w-20 rounded bg-bolu-panel/60" />
          </div>
        </div>

        <div className="my-auto flex flex-col items-center justify-center gap-3 text-center">
          <div className="size-8 rounded-full border-2 border-bolu-accent border-t-transparent animate-spin" />
          <p className="text-xs font-medium text-bolu-muted">Memuat obrolan...</p>
        </div>

        <div className="h-14 w-full rounded-2xl border border-bolu-border bg-bolu-surface" />
      </div>
    </div>
  );
}
