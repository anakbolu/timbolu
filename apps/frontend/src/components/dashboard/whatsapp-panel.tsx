"use client";

/**
 * WhatsApp: the number the shop actually answers on.
 *
 * The panel is one card in the Integrations screen, and it walks the same three
 * steps a person does: read the warning, choose who answers, scan the code. The
 * warning is not a formality and it is not a checkbox in a settings page — linking a
 * number over an unsupported route is a risk somebody takes, so the accept button is
 * what unblocks the QR and nothing else does.
 *
 * Two things are deliberate:
 * - the QR arrives on its own socket, not on the workspace stream. A QR is a
 *   capability: whoever scans it links the number, so it must reach the person who
 *   started the attempt and nobody else, and every member of the workspace receives
 *   the stream.
 * - the code is drawn from a matrix the server rendered, rather than from an image
 *   the browser fetched. There is no QR encoder here, and an `<img>` could not
 *   authenticate.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { Bolu } from "@/components/bolu/bolu";
import {
  type WhatsAppLink,
  type WhatsAppMessage,
  type WhatsAppNumber,
  type WhatsAppOutbox,
  acceptWhatsAppTerms,
  cancelWhatsAppLink,
  listWhatsAppMessages,
  listWhatsAppNumbers,
  listWhatsAppOutbox,
  prepareWhatsAppNumber,
  startWhatsAppLink,
  unlinkWhatsAppNumber,
  updateWhatsAppNumber,
  whatsAppLinkSocketURL,
  whatsAppPairCode,
} from "@/lib/api";
import { ALL_BOTS } from "@/lib/crew";
import { crewFor, expressionOf, relativeAge, stateOf } from "@/lib/dashboard-view";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-white";
const FIELD =
  "min-h-11 rounded-md border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-accent";
const LABEL = "flex flex-col gap-1.5 text-[14px] font-semibold";

/** STATUS_LABEL says what a number's state means, in the user's words. */
const STATUS_LABEL: Record<string, string> = {
  unlinked: "Belum tertaut",
  linking: "Sedang menautkan",
  connected: "Terhubung",
  needs_relink: "Perlu tautkan ulang",
  disconnected: "Terputus",
};

/** STATUS_DOT is the dot beside the state, matching the hub's own palette. */
const STATUS_DOT: Record<string, string> = {
  unlinked: "#8B88A0",
  linking: "#E2602B",
  connected: "#2FA65A",
  needs_relink: "#E2602B",
  disconnected: "#8B88A0",
};

/** OUTBOX_LABEL says what happened to a message this workspace asked for. */
const OUTBOX_LABEL: Record<string, string> = {
  queued: "Menunggu",
  sending: "Sedang dikirim",
  sent: "Terkirim",
  failed: "Gagal",
  held: "Ditahan batas jam",
};

/** Warn is shown before anything is linked, and it says what the risk is. */
const WARN = [
  "Penautan ini memakai jalur WhatsApp Web, bukan WhatsApp Business Platform resmi.",
  "WhatsApp bisa membatasi atau memblokir nomor yang dianggap mengirim terlalu banyak atau terlalu cepat. Karena itu tidak ada fitur blast di sini: tiap nomor punya batas kirim per jam dan jeda acak antar pesan.",
  "Balasan ke pelanggan tetap lewat persetujuan: Bolu menulis drafnya, kamu yang mengirim.",
];

/** QrCode draws a matrix the server rendered. */
function QrCode({ matrix }: { matrix: string[] }) {
  const size = matrix.length;
  if (size === 0) {
    return null;
  }

  // `module` is what one QR cell is called, and it is a reserved name in a Next.js
  // module, so it is spelled out here.
  const cell = Math.max(2, Math.floor(220 / size));
  const side = size * cell;

  return (
    <svg
      role="img"
      aria-label="Kode QR untuk menautkan nomor WhatsApp"
      viewBox={`0 0 ${side} ${side}`}
      width={side}
      height={side}
      className="rounded-md bg-white p-1"
    >
      {matrix.flatMap((row, y) =>
        [...row].map((dark, x) =>
          dark === "1" ? (
            <rect key={`${x}-${y}`} x={x * cell} y={y * cell} width={cell} height={cell} fill="#24292F" />
          ) : null,
        ),
      )}
    </svg>
  );
}

/**
 * useLinkStream follows one attempt over its socket.
 *
 * It falls back to nothing rather than to polling: the panel's own
 * `refresh` reads the attempt once when the screen loads, and a dropped socket is
 * re-opened by the effect. A QR is only valid for a few seconds at a time, so
 * polling it into view would be slower than reconnecting.
 */
function useLinkStream(workspaceId: string | null, linkId: string | null, onFrame: (link: WhatsAppLink) => void) {
  // The handler is kept in a ref so a re-render does not close and reopen the
  // socket: a QR that flickers out while somebody is scanning it is worse than a
  // stale closure, and the ref is synced in an effect rather than during render
  // because writing a ref while rendering is what React forbids.
  const handler = useRef(onFrame);
  useEffect(() => {
    handler.current = onFrame;
  }, [onFrame]);

  useEffect(() => {
    if (!workspaceId || !linkId) {
      return;
    }

    const socket = new WebSocket(whatsAppLinkSocketURL(workspaceId, linkId));
    socket.onmessage = (event) => {
      try {
        handler.current(JSON.parse(String(event.data)) as WhatsAppLink);
      } catch {
        // A frame that is not JSON is dropped rather than rendered as a
        // half-built object.
      }
    };

    return () => socket.close();
  }, [workspaceId, linkId]);
}

export function WhatsAppPanel() {
  const store = useWorkspace();
  const { agents, workspaceID } = store;

  const [numbers, setNumbers] = useState<WhatsAppNumber[]>([]);
  const [selected, setSelected] = useState<string>("");
  const [link, setLink] = useState<WhatsAppLink | null>(null);
  // `ended` is how an attempt finished, kept after the socket closes: a failure
  // that goes back to the plain button reads as "nothing happened", and the person
  // has no idea whether to try again or to check something.
  const [ended, setEnded] = useState<string | null>(null);
  const [phone, setPhone] = useState("");
  const [agentID, setAgentID] = useState("");
  const [accepting, setAccepting] = useState(false);
  const [pairing, setPairing] = useState(false);
  const [messages, setMessages] = useState<WhatsAppMessage[]>([]);
  const [outbox, setOutbox] = useState<WhatsAppOutbox[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const current = useMemo(
    () => numbers.find((item) => item.id === selected) ?? numbers[0] ?? null,
    [numbers, selected],
  );

  const refresh = useCallback(async () => {
    if (!workspaceID) {
      return;
    }

    const listed = await listWhatsAppNumbers(workspaceID);
    if (!listed.ok) {
      setError(listed.message);
      setLoading(false);
      return;
    }

    setNumbers(listed.data);
    setSelected((previous) => previous || listed.data[0]?.id || "");
    setLoading(false);
  }, [workspaceID]);

  useEffect(() => {
    void (async () => {
      await refresh();
    })();
  }, [refresh]);

  // The form starts on the first Bolu, because a number with nobody answering it
  // is a number that loses customers. It is derived rather than seeded into state,
  // so a roster that arrives after the first render becomes the default without an
  // effect that writes state.
  const chosenAgent = agentID || agents[0]?.id || "";

  // What the customers wrote and what this workspace asked to send. They are read
  // together because they are one conversation seen from two sides.
  const loadHistory = useCallback(async () => {
    if (!workspaceID || !current) {
      setMessages([]);
      setOutbox([]);
      return;
    }

    const [written, queued] = await Promise.all([
      listWhatsAppMessages(workspaceID, current.id, { limit: 20 }),
      listWhatsAppOutbox(workspaceID, current.id, { limit: 20 }),
    ]);
    setMessages(written.ok ? written.data : []);
    setOutbox(queued.ok ? queued.data : []);
  }, [workspaceID, current]);

  useEffect(() => {
    void (async () => {
      await loadHistory();
    })();
  }, [loadHistory]);

  useLinkStream(workspaceID, link?.link_id ?? null, (frame) => {
    setLink(frame);
    if (frame.state === "paired") {
      setNotice("Nomor tersambung.");
      setPairing(false);
      void refresh();
    }
    if (frame.state === "expired" || frame.state === "failed") {
      setPairing(false);
      setEnded(frame.reason || (frame.state === "expired" ? "Kode penautan kedaluwarsa." : "Penautan gagal."));
    }
  });

  const prepare = useCallback(async () => {
    if (!workspaceID) return;
    if (!phone.trim()) {
      setError("Isi dulu nomor WhatsApp-nya.");
      return;
    }

    setBusy(true);
    const result = await prepareWhatsAppNumber(workspaceID, { phone: phone.trim(), agent_id: chosenAgent });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    setNotice("Nomor dicatat. Baca peringatannya, lalu tautkan.");
    setSelected(result.data.id);
    setPhone("");
    await refresh();
  }, [workspaceID, phone, chosenAgent, refresh]);

  const accept = useCallback(async () => {
    if (!workspaceID || !current) return;

    setAccepting(true);
    const result = await acceptWhatsAppTerms(workspaceID, current.id);
    setAccepting(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    await refresh();
  }, [workspaceID, current, refresh]);

  const start = useCallback(async () => {
    if (!workspaceID || !current) return;

    setBusy(true);
    const result = await startWhatsAppLink(workspaceID, current.id, { phone: current.phone });
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    setNotice(null);
    setEnded(null);
    setLink(result.data);
    setPairing(true);
    await refresh();
  }, [workspaceID, current, refresh]);

  const pair = useCallback(async () => {
    if (!workspaceID || !link) return;

    setBusy(true);
    const result = await whatsAppPairCode(workspaceID, link.link_id, current?.phone ?? "");
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setError(null);
    setLink(result.data);
  }, [workspaceID, link, current]);

  const cancel = useCallback(async () => {
    if (!workspaceID || !link) return;

    await cancelWhatsAppLink(workspaceID, link.link_id);
    setLink(null);
    setPairing(false);
    await refresh();
  }, [workspaceID, link, refresh]);

  const unlink = useCallback(async () => {
    if (!workspaceID || !current) return;

    setBusy(true);
    const result = await unlinkWhatsAppNumber(workspaceID, current.id);
    setBusy(false);

    if (!result.ok) {
      setError(result.message);
      return;
    }

    setLink(null);
    setNotice("Nomor dilepas. Riwayat pesannya tetap tersimpan.");
    await refresh();
  }, [workspaceID, current, refresh]);

  const changeAgent = useCallback(
    async (next: string) => {
      if (!workspaceID || !current || !next) return;
      const result = await updateWhatsAppNumber(workspaceID, current.id, { agent_id: next });
      if (!result.ok) {
        setError(result.message);
        return;
      }
      setError(null);
      await refresh();
    },
    [workspaceID, current, refresh],
  );

  const changeLimit = useCallback(
    async (next: number) => {
      if (!workspaceID || !current) return;
      const result = await updateWhatsAppNumber(workspaceID, current.id, { hourly_limit: next });
      if (!result.ok) {
        setError(result.message);
        return;
      }
      setError(null);
      await refresh();
    },
    [workspaceID, current, refresh],
  );

  const bolu = useMemo(() => agents.find((agent) => agent.id === current?.agent_id) ?? null, [agents, current]);

  return (
    <section className={`${CARD} flex flex-col gap-4 p-[20px]`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <span
            aria-hidden
            className="flex size-10 flex-none items-center justify-center rounded-md font-display text-[15px] font-bold text-white"
            style={{ backgroundColor: "#1E7D4A" }}
          >
            WA
          </span>
          <div>
            <div className="font-display text-[17px] font-semibold">WhatsApp</div>
            <div className="text-[13px] text-bolu-muted">
              Jalur tidak resmi: nomor tertaut lewat WhatsApp Web, dengan batas kirim per jam.
            </div>
          </div>
        </div>
        {current ? (
          <span className="flex items-center gap-2 text-[13px] font-semibold">
            <span
              aria-hidden
              className="size-2 rounded-full"
              style={{ backgroundColor: STATUS_DOT[current.status] ?? STATUS_DOT.unlinked }}
            />
            {STATUS_LABEL[current.status] ?? current.status}
          </span>
        ) : null}
      </div>

      {error ? <Alert tone="error">{error}</Alert> : null}
      {ended && !error && !pairing ? <Alert tone="info">{ended}</Alert> : null}
      {notice && !error ? <Alert tone="success">{notice}</Alert> : null}
      {current?.reason && current.status !== "connected" ? <Alert tone="info">{current.reason}</Alert> : null}

      {loading ? (
        <p className="text-[14px] text-bolu-muted">Memuat…</p>
      ) : numbers.length === 0 ? (
        <div className="flex flex-col gap-3">
          {/* The warning is read before anything is linked, and what it says is the
              reason the product has no blast feature. */}
          <div className="rounded-md border border-bolu-border bg-bolu-panel/60 p-[14px_16px]">
            <div className="text-[14px] font-semibold">Sebelum menautkan nomor</div>
            <ul className="mt-2 flex list-disc flex-col gap-1.5 pl-5 text-[13px] text-bolu-muted">
              {WARN.map((line) => (
                <li key={line}>{line}</li>
              ))}
            </ul>
          </div>

          <div className="flex flex-wrap gap-3.5">
            <label className={`${LABEL} min-w-[200px] flex-1`}>
              Nomor WhatsApp
              <input
                type="tel"
                value={phone}
                placeholder="0812-3456-789"
                onChange={(event) => setPhone(event.target.value)}
                className={FIELD}
              />
              <span className="text-[13px] font-normal text-bolu-muted">
                Nomor toko yang akan menjawab pelanggan.
              </span>
            </label>
            <label className={`${LABEL} min-w-[180px] flex-[0_1_240px]`}>
              Dijawab oleh
              <select
                value={chosenAgent}
                onChange={(event) => setAgentID(event.target.value)}
                className={`${FIELD} cursor-pointer`}
              >
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </label>
          </div>
          <div>
            <button
              type="button"
              onClick={() => void prepare()}
              disabled={busy || agents.length === 0}
              className="min-h-11 rounded-full bg-bolu-ink px-5 text-[14px] font-bold text-white disabled:opacity-60"
            >
              {busy ? "Menyiapkan…" : "Hubungkan WhatsApp"}
            </button>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          {numbers.length > 1 ? (
            <label className={LABEL}>
              Nomor
              <select
                value={current?.id ?? ""}
                onChange={(event) => {
                  setSelected(event.target.value);
                  setLink(null);
                  setPairing(false);
                }}
                className={`${FIELD} cursor-pointer`}
              >
                {numbers.map((number) => (
                  <option key={number.id} value={number.id}>
                    {number.label} · {number.phone}
                  </option>
                ))}
              </select>
            </label>
          ) : null}

          <div className="flex flex-wrap items-center justify-between gap-3 rounded-md bg-bolu-panel/60 p-[14px_16px]">
            <div className="min-w-0">
              <div className="font-display text-[16px] font-semibold">{current?.label}</div>
              <div className="text-[13px] text-bolu-muted">
                {current?.phone}
                {current?.last_seen_at ? ` · terakhir terlihat ${relativeAge(current.last_seen_at)}` : ""}
              </div>
            </div>
            <div className="flex items-center gap-2.5">
              {bolu ? (
                <Bolu member={crewFor(bolu)} expression={expressionOf(stateOf(bolu))} color={bolu.color || undefined} size={30} />
              ) : (
                <Bolu member={ALL_BOTS[0]} expression="mikir" size={30} />
              )}
              <select
                value={current?.agent_id ?? ""}
                onChange={(event) => void changeAgent(event.target.value)}
                className="min-h-9 cursor-pointer rounded-full border border-bolu-border bg-white px-3 text-[13px] font-semibold text-bolu-ink outline-none"
              >
                <option value="">Belum dipilih</option>
                {agents.map((agent) => (
                  <option key={agent.id} value={agent.id}>
                    {agent.name}
                  </option>
                ))}
              </select>
            </div>
          </div>

          {/* The unlinked case is where the warning lives, and accepting it is what
              unblocks the QR. Nothing else does: a number linked without somebody
              reading this is a risk nobody took. */}
          {current && !current.terms_accepted ? (
            <div className="flex flex-col gap-3 rounded-md border border-bolu-border bg-bolu-panel/60 p-[14px_16px]">
              <div className="text-[14px] font-semibold">Baca dulu sebelum menautkan</div>
              <ul className="flex list-disc flex-col gap-1.5 pl-5 text-[13px] text-bolu-muted">
                {WARN.map((line) => (
                  <li key={line}>{line}</li>
                ))}
              </ul>
              <div>
                <button
                  type="button"
                  onClick={() => void accept()}
                  disabled={accepting}
                  className="min-h-11 rounded-full bg-bolu-ink px-5 text-[14px] font-bold text-white disabled:opacity-60"
                >
                  {accepting ? "Menyimpan…" : "Saya mengerti, lanjutkan"}
                </button>
              </div>
            </div>
          ) : null}

          {current?.terms_accepted && !current.linked ? (
            <div className="flex flex-col gap-3 rounded-md border border-bolu-border p-[14px_16px]">
              {pairing && link ? (
                <>
                  {link.state === "code" && link.qr_matrix ? (
                    <div className="flex flex-wrap items-start gap-5">
                      <div className="flex-none">
                        <QrCode matrix={link.qr_matrix} />
                      </div>
                      <div className="min-w-[220px] flex-1 text-[14px]">
                        <div className="font-semibold">Pindai dari HP</div>
                        <p className="mt-1 text-[13px] text-bolu-muted">
                          Buka WhatsApp → Perangkat tertaut → Tautkan perangkat, lalu pindai kode ini. Kodenya
                          berganti sendiri kalau lewat waktunya.
                        </p>
                        {link.pair_code ? (
                          <p className="mt-2 text-[13px]">
                            Atau masukkan kode ini di HP: <span className="font-display font-bold">{link.pair_code}</span>
                          </p>
                        ) : (
                          <button
                            type="button"
                            onClick={() => void pair()}
                            className="mt-2 min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-ink"
                          >
                            Tidak bisa memindai? Pakai kode
                          </button>
                        )}
                      </div>
                    </div>
                  ) : (
                    <p className="text-[14px] text-bolu-muted">
                      {link.state === "waiting"
                        ? "Menyiapkan kode… kalau WhatsApp tidak mengirimkannya, penautan ini berhenti sendiri dan kamu bisa memintanya lagi."
                        : link.reason || "Menunggu HP-nya…"}
                    </p>
                  )}
                  <div className="flex flex-wrap gap-2.5">
                    <button
                      type="button"
                      onClick={() => void cancel()}
                      className="min-h-9 rounded-full border border-bolu-border px-4 text-[13px] font-semibold text-bolu-ink"
                    >
                      Batalkan
                    </button>
                    {link.state === "expired" || link.state === "failed" ? (
                      <button
                        type="button"
                        onClick={() => void start()}
                        className="min-h-9 rounded-full bg-bolu-ink px-4 text-[13px] font-semibold text-white"
                      >
                        Minta kode baru
                      </button>
                    ) : null}
                  </div>
                </>
              ) : (
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <div className="text-[14px]">
                    <div className="font-semibold">Nomornya belum tertaut</div>
                    <p className="text-[13px] text-bolu-muted">
                      Bolu hanya bisa menjawab setelah nomornya tertaut dari HP.
                    </p>
                  </div>
                  <button
                    type="button"
                    onClick={() => void start()}
                    disabled={busy}
                    className="min-h-11 rounded-full bg-bolu-ink px-5 text-[14px] font-bold text-white disabled:opacity-60"
                  >
                    {busy ? "Memulai…" : "Tautkan nomor"}
                  </button>
                </div>
              )}
            </div>
          ) : null}

          {current?.linked ? (
            <div className="flex flex-col gap-3">
              <div className="flex flex-wrap items-end gap-3.5">
                <label className={`${LABEL} min-w-[160px] flex-[0_1_200px]`}>
                  Batas kirim per jam
                  <input
                    type="number"
                    min={1}
                    max={500}
                    value={current.hourly_limit}
                    onChange={(event) => void changeLimit(Number(event.target.value))}
                    className={FIELD}
                  />
                  <span className="text-[13px] font-normal text-bolu-muted">
                    Tidak ada pengiriman massal: pesan berlebih menunggu, bukan hilang.
                  </span>
                </label>
                <button
                  type="button"
                  onClick={() => void unlink()}
                  disabled={busy}
                  className="min-h-11 rounded-full border border-bolu-border px-5 text-[14px] font-bold text-bolu-belum disabled:opacity-60"
                >
                  Lepas nomor
                </button>
              </div>

              {/* The two sides of one conversation: what a customer wrote, and what
                  this workspace asked to send. */}
              <div className="grid gap-3.5 sm:grid-cols-2">
                <div className="flex flex-col gap-2">
                  <div className="text-[14px] font-semibold">Pesan masuk</div>
                  {messages.length === 0 ? (
                    <p className="text-[13px] text-bolu-muted">Belum ada pesan.</p>
                  ) : (
                    <ul className="flex flex-col">
                      {messages.slice(0, 8).map((message) => (
                        <li key={message.id} className="border-t border-bolu-line p-[8px_0] text-[13px]">
                          <div className="flex items-center justify-between gap-2">
                            <span className="font-semibold">
                              {message.ours ? "Kita" : message.name || message.from || message.chat}
                            </span>
                            <span className="flex-none text-bolu-muted">{relativeAge(message.received_at)}</span>
                          </div>
                          <div className="text-bolu-muted">
                            {message.text ||
                              (message.kind === "other" ? "pesan yang belum bisa dibaca" : `lampiran ${message.kind}`)}
                            {message.media_status === "pending" ? " (lampiran sedang diunduh)" : ""}
                            {message.media_status === "failed" ? " (lampiran gagal diunduh)" : ""}
                          </div>
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
                <div className="flex flex-col gap-2">
                  <div className="text-[14px] font-semibold">Pesan keluar</div>
                  {outbox.length === 0 ? (
                    <p className="text-[13px] text-bolu-muted">Belum ada yang dikirim.</p>
                  ) : (
                    <ul className="flex flex-col">
                      {outbox.slice(0, 8).map((item) => (
                        <li key={item.id} className="border-t border-bolu-line p-[8px_0] text-[13px]">
                          <div className="flex items-center justify-between gap-2">
                            <span className="font-semibold">{item.to}</span>
                            <span className="flex-none text-bolu-muted">
                              {OUTBOX_LABEL[item.status] ?? item.status}
                            </span>
                          </div>
                          <div className="text-bolu-muted">{item.text}</div>
                          {item.last_error ? <div className="text-bolu-belum">{item.last_error}</div> : null}
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              </div>
            </div>
          ) : null}
        </div>
      )}
    </section>
  );
}
