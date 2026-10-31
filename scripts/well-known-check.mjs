/**
 * Are the two files that make universal links work actually claiming the app?
 *
 * `/.well-known/apple-app-site-association` and `/.well-known/assetlinks.json`
 * answer 200 even when the deployment has no app identity, because an empty claim
 * is the honest answer when there is no identity to claim — see
 * `apps/frontend/src/lib/app-links.ts`. The cost of that honesty is that "200 OK"
 * reads like success: the files are live, the links still open a browser, and
 * nothing anywhere says so. This script is the one place that difference is
 * printed, and `--strict` turns it into a gate for the day the identity exists.
 *
 * Informative by default: today production has no identity, so failing would fail
 * every run for a reason nobody can fix from the repo.
 */

const DEFAULT_BASE = "https://timbolu.com";

const TARGETS = [
  {
    path: "/.well-known/apple-app-site-association",
    claims: (body) =>
      Array.isArray(body?.applinks?.details) && body.applinks.details.length > 0,
  },
  {
    path: "/.well-known/assetlinks.json",
    claims: (body) => Array.isArray(body) && body.length > 0,
  },
];

const args = process.argv.slice(2);
const strict = args.includes("--strict") || process.env.STRICT === "1";
const baseFlag = args.indexOf("--base");
const base = (
  baseFlag === -1 ? process.env.WELL_KNOWN_BASE || DEFAULT_BASE : args[baseFlag + 1] || DEFAULT_BASE
).replace(/\/+$/, "");

/** One request per file; `AbortSignal.timeout` so a hung host cannot hang the gate. */
async function inspect(target) {
  try {
    const response = await fetch(`${base}${target.path}`, {
      signal: AbortSignal.timeout(10_000),
      headers: { accept: "application/json" },
      redirect: "follow",
    });
    const raw = await response.text();
    let body;
    try {
      body = JSON.parse(raw);
    } catch {
      return { ok: false, status: response.status, contentType: response.headers.get("content-type"), note: "bukan JSON" };
    }
    const claimed = target.claims(body);
    return {
      ok: response.status === 200 && claimed,
      status: response.status,
      contentType: response.headers.get("content-type") || "-",
      note: claimed ? "klaim terisi" : "KLAIM KOSONG",
    };
  } catch (error) {
    return { ok: false, status: 0, contentType: "-", note: `gagal: ${error.message}` };
  }
}

const name = (path) => path.split("/").pop();

let failed = false;
for (const target of TARGETS) {
  const result = await inspect(target);
  if (!result.ok) failed = true;
  console.log(
    `${name(target.path).padEnd(28)} ${String(result.status).padEnd(4)} ${result.contentType.padEnd(22)} ${result.note}`,
  );
}

if (failed) {
  console.log(
    strict
      ? `\nFAIL: ${base} belum mengklaim app apa pun, jadi universal link tetap membuka browser. Isi identitas app di environment (lihat docs/deployment.md § Identitas universal link).`
      : `\nCatatan: ada berkas yang belum mengklaim app. Universal link belum hidup sampai identitas app diisi (docs/deployment.md). Jalankan dengan --strict untuk menjadikannya gate.`,
  );
}

process.exit(strict && failed ? 1 : 0);
