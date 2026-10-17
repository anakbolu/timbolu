"use client";

/**
 * Integrasi: the page that names what the workspace has connected.
 *
 * The hub itself — the search, the categories, the connected and available
 * applications, the grants, the MCP servers — lives in the sheet the pill opens,
 * because acting on a connection is a detail of that connection rather than
 * something to scroll past. What stays here is what a person has to see without
 * asking: the pill with the marks of what is connected, and a connection that has
 * stopped working. The screen a provider's redirect lands on is this one, so it
 * also reads the outcome of a flow from the query string.
 */

import { useEffect, useState } from "react";

import { AppsPill, tileFor } from "./apps-pill";
import { IntegrationsSheet, STATUS_LABEL } from "./integrations-sheet";
import { useWorkspace } from "./workspace-data";

/** OUTCOME says what a finished authorization flow did. */
const OUTCOME: Record<string, string> = {
  connected: "Akun tersambung.",
  declined: "Kamu menolak aksesnya, jadi tidak ada yang tersambung.",
  invalid_state:
    "Tautan itu bukan milik alur yang dimulai dari sini, atau sudah dipakai. Coba sambungkan lagi.",
  exchange_failed: "Penyedia menolak kode itu. Biasanya karena terlalu lama di layar izin; coba lagi.",
  unknown_app: "Aplikasi itu tidak dikenal deployment ini.",
  failed: "Penyambungan gagal. Coba lagi.",
};

/**
 * readOutcome turns the query a provider's redirect left behind into a message.
 *
 * It runs only in the browser: the screen is a client component, but it is still
 * rendered once on the server, where there is no query string to read.
 */
function readOutcome(): string | null {
  if (typeof window === "undefined") {
    return null;
  }

  const params = new URLSearchParams(window.location.search);
  const outcome = params.get("integration");
  if (!outcome) {
    return null;
  }

  const message = OUTCOME[outcome] ?? OUTCOME.failed;
  const detail = params.get("detail");

  // A refusal's own words are worth showing; a success has nothing to add.
  if (detail && outcome !== "connected") {
    return `${message} (${detail})`;
  }
  return message;
}

export function IntegrationsPanel() {
  const { hub, reloadHub } = useWorkspace();
  const [open, setOpen] = useState(false);

  // The provider's redirect lands here with the outcome in the query string. It is
  // read once, when the screen first renders, and then cleared so a reload does not
  // repeat a message about a flow that already finished.
  const [outcome] = useState<string | null>(() => readOutcome());

  useEffect(() => {
    if (!outcome) return;
    window.history.replaceState({}, "", window.location.pathname);
    void reloadHub();
  }, [outcome, reloadHub]);

  const connections = hub.connections;
  const attention = connections.filter((connection) => connection.status !== "connected");

  return (
    <div className="flex flex-col gap-[22px] p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header className="flex flex-wrap items-center justify-between gap-3.5">
        <div>
          <h1 className="font-display text-[36px] font-bold leading-[1.1] max-[760px]:text-[28px]">
            Integrasi
          </h1>
          <p className="text-bolu-muted">
            Sambungkan akun yang dipakai tim Bolu, lalu atur siapa saja yang boleh memakainya. Token
            disimpan terenkripsi dan hanya dibuka saat dipakai.
          </p>
        </div>
        <AppsPill
          label="Hubungkan aplikasi"
          apps={connections.map((connection) => connection.app)}
          expanded={open}
          onClick={() => setOpen(true)}
        />
      </header>

      {outcome ? (
        <p role="status" className="rounded-md bg-bolu-live/10 px-4 py-3 text-[15px] text-bolu-lunas">
          {outcome}
        </p>
      ) : null}

      {/* A connection that needs a fresh login has to say so where it is seen: the
          pill only carries the marks, and the sheet is closed most of the time. */}
      {attention.length > 0 ? (
        <p role="status" className="rounded-md bg-bolu-flag/10 px-4 py-3 text-[15px] text-bolu-flag">
          Perlu ditangani:{" "}
          {attention
            .map(
              (connection) =>
                `${tileFor(connection.app).name} — ${
                  STATUS_LABEL[connection.status] ?? connection.status
                }`,
            )
            .join(", ")}
          .
        </p>
      ) : null}

      <IntegrationsSheet open={open} onOpenChange={setOpen} />
    </div>
  );
}
