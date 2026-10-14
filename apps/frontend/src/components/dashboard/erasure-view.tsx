"use client";

/**
 * Data: what this deployment keeps, and how to ask for it back.
 *
 * The screen is four paragraphs and one button, and the proportions are the
 * decision. The policy is written out here rather than linked to a marketing page
 * because the person deciding needs it in front of them while they decide, and the
 * button is behind a typed confirmation because it is the only irreversible thing
 * in the product.
 *
 * What the screen does *not* do is pretend the erasure can be scheduled, undone, or
 * narrowed. There is no "delete my messages but keep the memory" switch, because the
 * backend has no such operation and a switch that did nothing would be worse than
 * the absence: a person asking for their data to be removed should not have to
 * guess which of their clicks had an effect.
 *
 * The evidence is downloadable, and that follows from the same reasoning: the
 * workspace does not exist after this call, so `GET /workspaces/current/erasure`
 * can no longer address it. The POST's own answer is the only thing the person can
 * keep, so it is offered as a file rather than as a paragraph to photograph.
 */

import { useCallback, useState } from "react";

import { Alert } from "@/components/auth/auth-shell";
import { ensureActiveWorkspace, eraseWorkspaceData } from "@/lib/api";

import { useWorkspace } from "./workspace-data";

const CARD = "rounded-md border border-bolu-border bg-white";
const HEADING = "font-display text-[22px] font-semibold";
const BUTTON =
  "min-h-11 rounded-full px-5 font-bold transition disabled:opacity-50";

/** The windows this deployment keeps, which is the policy in the user's words. */
const RETENTION = [
  {
    what: "Isi mentah langkah tugas",
    how: "30 hari",
    then: "Setelah itu yang tersisa hanya jenis langkah, alat, jumlah token, dan waktu.",
  },
  {
    what: "Lampiran",
    how: "Selama pesan yang membawanya",
    then: "Berkas ikut hilang saat pesannya dihapus atau workspace dihapus.",
  },
  {
    what: "Pesan, memori, dan dokumen",
    how: "Selama workspace ada",
    then: "Penghapusan di bawah ini menghapusnya lebih cepat.",
  },
  {
    what: "Jejak audit",
    how: "Selama workspace ada",
    then: "Ia yang menjawab \u201csiapa mengubah ini\u201d, jadi ia ikut hilang bersama workspace.",
  },
  {
    what: "Bukti penghapusan",
    how: "Selamanya",
    then: "Berisi id, nama, dan jumlah \u2014 bukan isi datamu.",
  },
];

/** ErasureView is the data policy and the erasure request. */
export function ErasureView() {
  const { workspace, user } = useWorkspace();

  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  // evidence is what the erasure answered, which is the record a person keeps.
  const [evidence, setEvidence] = useState<ErasureEvidence | null>(null);

  const name = workspace?.name ?? "";
  const armed = name !== "" && confirmation.trim() === name;

  // The name and the address are kept before the call, because after it the workspace they came
  // from no longer exists and the store has nothing left to read them off.
  const storedName = name;
  const requestedBy = user?.email ?? "";

  const erase = useCallback(async () => {
    setBusy(true);
    setFailure(null);
    setNotice(null);

    const id = workspace?.id ?? (await ensureActiveWorkspace());
    if (!id) {
      setBusy(false);
      setFailure("Tidak ada workspace aktif untuk dihapus.");

      return;
    }

    const result = await eraseWorkspaceData(id);
    setBusy(false);

    if (!result.ok) {
      // The backend distinguishes "the path is not configured" from "the erasure failed", and
      // both arrive here as a message: what the person needs to know is whether anything was
      // removed, and the failure text says so.
      setFailure(result.message);

      return;
    }

    setEvidence({
      workspace_id: result.data.workspace_id,
      workspace_name: name || storedName,
      objects_deleted: result.data.objects,
      rows_deleted: result.data.rows,
      erased_at: new Date().toISOString(),
      requested_by_email: requestedBy,
    });
    setNotice(
      `Data workspace dihapus: ${result.data.rows} baris dan ${result.data.objects} berkas. ` +
        "Unduh buktinya di bawah: halaman ini tidak bisa dibuka lagi karena workspace-nya sudah tidak ada.",
    );
  }, [workspace?.id, name, storedName, requestedBy]);

  return (
    <div className="flex flex-col gap-6 p-[24px_28px_48px] max-[760px]:p-[18px_14px_36px]">
      <header>
        <h1 className={HEADING}>Data</h1>
        <p className="mt-1 text-[15px] text-bolu-muted">
          Apa yang kami simpan, berapa lama, dan bagaimana memintanya dihapus.
        </p>
      </header>

      {notice ? <Alert tone="success">{notice}</Alert> : null}
      {failure ? <Alert tone="error">{failure}</Alert> : null}

      {evidence ? (
        <section className={`${CARD} p-6`}>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <h2 className="font-display text-[17px] font-semibold">Bukti penghapusan</h2>
            <button
              type="button"
              onClick={() => downloadEvidence(evidence)}
              className={`${BUTTON} border-2 border-bolu-ink text-bolu-ink hover:border-bolu-accent`}
            >
              Unduh bukti (JSON)
            </button>
          </div>

          <dl className="mt-3 grid grid-cols-2 gap-x-6 gap-y-3 text-[14px] md:grid-cols-3">
            <Evidence label="Nama workspace" value={evidence.workspace_name} />
            <Evidence label="Id workspace" value={evidence.workspace_id} />
            <Evidence label="Diminta oleh" value={evidence.requested_by_email || "—"} />
            <Evidence label="Baris dihapus" value={String(evidence.rows_deleted)} />
            <Evidence label="Berkas dihapus" value={String(evidence.objects_deleted)} />
            <Evidence label="Waktu" value={new Date(evidence.erased_at).toLocaleString("id-ID")} />
          </dl>
        </section>
      ) : null}

      <section className={`${CARD} p-6`}>
        <h2 className="font-display text-[17px] font-semibold">Berapa lama kami menyimpan</h2>
        <table className="mt-3 w-full text-[14px]">
          <thead className="text-left text-bolu-muted">
            <tr>
              <th className="py-2 font-medium">Data</th>
              <th className="py-2 font-medium">Berapa lama</th>
              <th className="py-2 font-medium">Setelah itu</th>
            </tr>
          </thead>
          <tbody>
            {RETENTION.map((row) => (
              <tr key={row.what} className="border-t border-bolu-line align-top">
                <td className="py-2 pr-4">{row.what}</td>
                <td className="py-2 pr-4 font-semibold">{row.how}</td>
                <td className="py-2 text-bolu-muted">{row.then}</td>
              </tr>
            ))}
          </tbody>
        </table>

        <p className="mt-4 text-[14px] text-bolu-muted">
          Data tersimpan di Indonesia (Jakarta). Mesin yang menjalankan tugas hanya menerima pengenal
          seperti nomor tugas, bukan isi pesan. Saat Bolu memanggil model, prompt dikirim ke penyedia
          yang kamu konfigurasi di Pengaturan → Model, dan hanya sebanyak yang dibutuhkan satu
          panggilan. Isi pesan tidak pernah masuk ke log atau ke jejak telemetri.
        </p>
      </section>

      <section className={`${CARD} border-bolu-belum/40 p-6`}>
        <h2 className="font-display text-[17px] font-semibold text-bolu-belum">
          Hapus semua data workspace
        </h2>

        <p className="mt-2 text-[14px] text-bolu-muted">
          Menghapus workspace <strong>beserta seluruh isinya</strong>: percakapan, memori dan
          embedding, lampiran, tugas, konfigurasi, dan jejak audit. Ini{" "}
          <strong>tidak bisa dibatalkan</strong> dan tidak ada masa tunggu — permintaan dijalankan
          seketika. Anggota lain kehilangan akses karena workspace-nya sudah tidak ada. Hanya
          workspace ini yang terpengaruh; workspace lain milikmu tidak tersentuh.
        </p>

        <label className="mt-4 flex max-w-md flex-col gap-1.5 text-[14px] font-semibold">
          Ketik nama workspace untuk mengaktifkan tombolnya
          <input
            type="text"
            value={confirmation}
            onChange={(event) => setConfirmation(event.target.value)}
            placeholder={name || "nama workspace"}
            aria-label="Konfirmasi nama workspace"
            className="min-h-11 rounded-md border border-bolu-border bg-white px-3.5 text-[15px] font-normal text-bolu-ink outline-none focus:border-bolu-belum"
          />
        </label>

        <button
          type="button"
          disabled={!armed || busy}
          onClick={() => void erase()}
          className={`${BUTTON} mt-4 bg-bolu-belum text-white hover:opacity-90`}
        >
          {busy ? "Menghapus…" : "Hapus semua data workspace"}
        </button>

        <p className="mt-3 text-[13px] text-bolu-muted">
          Setelah selesai, kamu menerima jumlah baris dan berkas yang dihapus sebagai bukti, dan
          dapat mengunduhnya sebagai berkas. Simpan berkas itu: halaman ini tidak bisa dibuka lagi
          karena workspace-nya sudah tidak ada.
        </p>
      </section>
    </div>
  );
}

/** ErasureEvidence is what the erasure answered, which is what the person keeps. */
type ErasureEvidence = {
  workspace_id: string;
  workspace_name: string;
  objects_deleted: number;
  rows_deleted: number;
  erased_at: string;
  requested_by_email: string;
};

/**
 * downloadEvidence saves the record as a file, which is the artifact the screen promises.
 *
 * It is built in the browser from the response rather than fetched from an endpoint, and that is
 * forced by the erasure itself: the workspace does not exist afterwards, so a read addressed to it
 * cannot answer. The POST's own answer is the whole of the evidence a person can keep.
 */
function downloadEvidence(evidence: ErasureEvidence): void {
  const blob = new Blob([JSON.stringify(evidence, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `bukti-hapus-${evidence.workspace_id}.json`;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

/** Evidence is one labelled fact on the record. */
function Evidence({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-[13px] text-bolu-muted">{label}</dt>
      <dd className="break-all font-medium">{value}</dd>
    </div>
  );
}
