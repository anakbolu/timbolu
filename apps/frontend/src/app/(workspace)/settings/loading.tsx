export default function SettingsLoading() {
  return (
    <div className="mx-auto flex w-full max-w-[960px] animate-pulse flex-col gap-6 p-6">
      <div className="flex flex-col gap-2">
        <div className="h-8 w-48 rounded-md bg-bolu-panel" />
        <div className="h-4 w-80 rounded bg-bolu-panel/60" />
      </div>

      <div className="flex gap-2 border-b border-bolu-line pb-3">
        <div className="h-8 w-24 rounded-full bg-bolu-panel" />
        <div className="h-8 w-24 rounded-full bg-bolu-panel/50" />
        <div className="h-8 w-24 rounded-full bg-bolu-panel/50" />
        <div className="h-8 w-24 rounded-full bg-bolu-panel/50" />
      </div>

      <div className="flex flex-col gap-4 rounded-md border border-bolu-border bg-bolu-surface p-6">
        <div className="h-6 w-40 rounded bg-bolu-panel" />
        <div className="h-4 w-64 rounded bg-bolu-panel/60" />
        <div className="mt-4 flex flex-col gap-3">
          <div className="h-10 w-full rounded-md bg-bolu-panel/40" />
          <div className="h-10 w-full rounded-md bg-bolu-panel/40" />
        </div>
      </div>

      <div className="flex flex-col items-center justify-center py-6">
        <div className="size-6 rounded-full border-2 border-bolu-accent border-t-transparent animate-spin" />
        <p className="mt-3 text-xs font-medium text-bolu-muted">Memuat pengaturan...</p>
      </div>
    </div>
  );
}
