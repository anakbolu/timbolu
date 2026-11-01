export default function TaskLoading() {
  return (
    <div className="mx-auto flex w-full max-w-[960px] animate-pulse flex-col gap-6 p-6">
      <div className="flex flex-col gap-2">
        <div className="h-4 w-28 rounded bg-bolu-panel/80" />
        <div className="h-8 w-64 rounded-md bg-bolu-panel" />
      </div>

      <div className="rounded-md border border-bolu-border bg-bolu-surface p-6">
        <div className="flex items-center justify-between border-b border-bolu-line pb-4">
          <div className="flex items-center gap-3">
            <div className="size-3 rounded-full bg-bolu-track" />
            <div className="h-5 w-40 rounded bg-bolu-panel" />
          </div>
          <div className="h-8 w-24 rounded bg-bolu-panel/70" />
        </div>

        <div className="mt-6 flex flex-col gap-4">
          <div className="h-4 w-3/4 rounded bg-bolu-panel/60" />
          <div className="h-4 w-1/2 rounded bg-bolu-panel/50" />
          <div className="h-4 w-2/3 rounded bg-bolu-panel/40" />
        </div>
      </div>

      <div className="flex flex-col items-center justify-center py-8">
        <div className="size-6 rounded-full border-2 border-bolu-accent border-t-transparent animate-spin" />
        <p className="mt-3 text-xs font-medium text-bolu-muted">Memuat detail tugas...</p>
      </div>
    </div>
  );
}
