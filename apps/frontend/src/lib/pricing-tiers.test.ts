/**
 * Test pagar angka /pricing terhadap seed plans database (issue #656).
 *
 * Memastikan angka yang dijual di `/pricing` (TIERS di `src/lib/pricing-tiers.ts`)
 * selalu selaras dengan sumber kebenaran penagihan di seed database:
 * `apps/backend-js/src/migrations/000016_billing.up.sql`.
 *
 * Dijalankan dengan:
 *   cd apps/frontend && bun test src/lib/pricing-tiers.test.ts
 */

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

import { TIERS } from "./pricing-tiers";

type SeedPlan = {
  key: string;
  name: string;
  monthly_price_idr: number;
  token_quota: number;
  max_agents: number;
  max_integrations: number;
  max_members: number;
  is_trial: boolean;
  sort_order: number;
  daily_cost_micros: number;
  period_days: number;
};

const NUMBER_WORDS_ID: Record<number, string> = {
  1: "Satu",
  2: "Dua",
  3: "Tiga",
  4: "Empat",
  5: "Lima",
  6: "Enam",
  7: "Tujuh",
  8: "Delapan",
  9: "Sembilan",
  10: "Sepuluh",
  20: "Dua puluh",
};

function formatIdrPrice(amount: number): string {
  const formatted = new Intl.NumberFormat("id-ID").format(amount);
  return `Rp ${formatted}`;
}

function formatTokens(tokenQuota: number): string {
  if (tokenQuota >= 1_000_000) {
    const millions = tokenQuota / 1_000_000;
    return `${millions} juta token`;
  }
  if (tokenQuota >= 1_000) {
    const thousands = tokenQuota / 1_000;
    return `${thousands} ribu token`;
  }
  return `${tokenQuota} token`;
}

function parseSeedPlans(sqlContent: string): SeedPlan[] {
  // Cari blok INSERT INTO plans (...) VALUES (...) ON CONFLICT ...
  const insertRegex =
    /INSERT\s+INTO\s+plans\s*\(([^)]+)\)\s*VALUES\s*([\s\S]*?)(?:ON\s+CONFLICT|;)/i;
  const match = sqlContent.match(insertRegex);
  if (!match) {
    throw new Error("Gagal menemukan pernyataan INSERT INTO plans pada berkas migrasi SQL");
  }

  const columns = match[1]
    .split(",")
    .map((col) => col.trim().replace(/["`]/g, ""));

  const valuesText = match[2].trim();
  // Tangkap baris tuple bernilai: ('keluarga', 'Paket Keluarga', 149000, ...)
  const tupleRegex = /\(([^)]+)\)/g;
  const plans: SeedPlan[] = [];
  let tupleMatch: RegExpExecArray | null;

  while ((tupleMatch = tupleRegex.exec(valuesText)) !== null) {
    const rawFields = tupleMatch[1].split(",").map((f) => f.trim());
    if (rawFields.length !== columns.length) {
      throw new Error(
        `Jumlah kolom tidak cocok dalam tuple: diharapkan ${columns.length}, didapat ${rawFields.length}`,
      );
    }

    const row: Record<string, unknown> = {};
    for (let i = 0; i < columns.length; i++) {
      const colName = columns[i];
      const val = rawFields[i];
      if (val.startsWith("'") && val.endsWith("'")) {
        row[colName] = val.slice(1, -1);
      } else if (val.toLowerCase() === "false") {
        row[colName] = false;
      } else if (val.toLowerCase() === "true") {
        row[colName] = true;
      } else {
        const num = Number(val);
        if (Number.isNaN(num)) {
          throw new Error(`Gagal mem-parse nilai numerik '${val}' untuk kolom '${colName}'`);
        }
        row[colName] = num;
      }
    }

    plans.push(row as unknown as SeedPlan);
  }

  if (plans.length === 0) {
    throw new Error("Tidak ada baris data plan yang ditemukan dari pernyataan INSERT");
  }

  return plans;
}

test("pricing tiers pagar: verifikasi terhadap seed plans billing.up.sql", () => {
  const migrationPath = path.resolve(
    import.meta.dirname,
    "../../../../apps/backend-js/src/migrations/000016_billing.up.sql",
  );

  assert.ok(
    fs.existsSync(migrationPath),
    `Berkas migrasi tidak ditemukan di ${migrationPath}`,
  );

  const sqlContent = fs.readFileSync(migrationPath, "utf-8");
  const seedPlans = parseSeedPlans(sqlContent);

  // Verifikasi hanya ada 2 tier berbayar yang cocok di seed
  assert.equal(
    seedPlans.length,
    2,
    `Diharapkan tepat 2 seed plans berbayar pada migrasi 000016, didapat ${seedPlans.length}`,
  );

  // Petakan per key tier
  const seedByKey = new Map<string, SeedPlan>();
  for (const plan of seedPlans) {
    seedByKey.set(plan.key.toLowerCase(), plan);
  }

  // Tier gratis tidak ada di seed plans (karena uji coba gratis 14 hari tidak ditagih di seed plans INSERT baru)
  const paidTiers = TIERS.filter((t) => t.price !== "Rp 0");
  const freeTiers = TIERS.filter((t) => t.price === "Rp 0");

  assert.equal(freeTiers.length, 1, "Diharapkan tepat 1 tier gratis ('Rp 0') pada TIERS");
  assert.equal(
    paidTiers.length,
    seedPlans.length,
    `Jumlah tier berbayar pada TIERS (${paidTiers.length}) harus sama dengan jumlah seed plans (${seedPlans.length})`,
  );

  for (const tier of paidTiers) {
    const key = tier.name.toLowerCase();
    const seed = seedByKey.get(key);

    assert.ok(
      seed !== undefined,
      `Tier berbayar '${tier.name}' (key: '${key}') tidak ditemukan di seed plans migrasi`,
    );

    // 1. monthly_price_idr -> string harga di tier ("Rp 149.000", format id-ID tanpa desimal)
    const expectedPrice = formatIdrPrice(seed.monthly_price_idr);
    assert.equal(
      tier.price,
      expectedPrice,
      `Harga tier '${tier.name}' tidak cocok: halaman '${tier.price}' vs seed '${expectedPrice}'`,
    );

    // 2. max_members -> jumlah "anggota tim"
    // Contoh feature: "6 anggota tim dan 6 Bolu" atau "25 anggota tim dan 20 Bolu"
    const membersPattern = new RegExp(`\\b${seed.max_members}\\s+anggota\\s+tim\\b`, "i");
    const hasMembers = tier.features.some((f) => membersPattern.test(f));
    assert.ok(
      hasMembers,
      `Tier '${tier.name}' harus mencantumkan '${seed.max_members} anggota tim'. Fitur yang ada: ${JSON.stringify(tier.features)}`,
    );

    // 3. max_agents -> jumlah "Bolu"
    // Contoh feature: "6 anggota tim dan 6 Bolu" atau "25 anggota tim dan 20 Bolu"
    const agentsPattern = new RegExp(`\\b${seed.max_agents}\\s+Bolu\\b`, "i");
    const hasAgents = tier.features.some((f) => agentsPattern.test(f));
    assert.ok(
      hasAgents,
      `Tier '${tier.name}' harus mencantumkan '${seed.max_agents} Bolu'. Fitur yang ada: ${JSON.stringify(tier.features)}`,
    );

    // 4. token_quota -> jumlah token ("2 juta token", "8 juta token")
    const expectedTokens = formatTokens(seed.token_quota);
    const hasTokens = tier.features.some((f) => f.toLowerCase().includes(expectedTokens.toLowerCase()));
    assert.ok(
      hasTokens,
      `Tier '${tier.name}' harus mencantumkan '${expectedTokens}'. Fitur yang ada: ${JSON.stringify(tier.features)}`,
    );

    // 5. max_integrations -> angka kata Indonesia ("Tiga aplikasi terhubung", "Sepuluh aplikasi terhubung")
    const word = NUMBER_WORDS_ID[seed.max_integrations];
    assert.ok(
      word !== undefined,
      `Kata Indonesia untuk angka ${seed.max_integrations} belum didefinisikan di pemetaan`,
    );
    const integrationPattern = new RegExp(`^${word}\\s+aplikasi\\s+terhubung$`, "i");
    const hasIntegration = tier.features.some((f) => integrationPattern.test(f));
    assert.ok(
      hasIntegration,
      `Tier '${tier.name}' harus mencantumkan '${word} aplikasi terhubung'. Fitur yang ada: ${JSON.stringify(tier.features)}`,
    );
  }
});
