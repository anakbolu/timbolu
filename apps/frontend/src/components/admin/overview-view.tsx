"use client";

/**
 * The operator dashboard: what the deployment's accounts add up to.
 *
 * Every number here is a count of accounts and nothing else. That is a boundary
 * rather than a thin first version: Bolus, tasks, conversations and money belong
 * to a workspace and stay behind the tenant policies, so a platform admin sees
 * how many accounts exist without becoming a reader of every customer's data.
 * The one thing the surface can do is promote an account, and the count of
 * admins is on this screen because that is what the promotion changes.
 */

import { UserCheck, Users } from "lucide-react";
import Link from "next/link";
import { useEffect, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { adminOverview, type AdminStats } from "@/lib/api";

/** One number with its label, which is what the whole screen is made of. */
function Stat({ label, value, hint }: { label: string; value: number; hint?: string }) {
  return (
    <div className="flex flex-col gap-1 rounded-md border border-border bg-card px-4 py-3">
      <span className="text-[12px] font-semibold text-muted-foreground">{label}</span>
      <span className="font-display text-2xl font-bold">{value.toLocaleString("id-ID")}</span>
      {hint ? <span className="text-[12px] text-muted-foreground">{hint}</span> : null}
    </div>
  );
}

export function AdminOverviewView() {
  const [stats, setStats] = useState<AdminStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The read is re-run by changing this rather than by calling a function that
  // sets state: an effect body may not set state synchronously, and a counter is
  // the smallest thing that makes the effect re-run on demand.
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let live = true;

    void (async () => {
      const result = await adminOverview();
      if (!live) return;

      if (result.ok) {
        setStats(result.data);
        setError(null);
      } else {
        setError(result.message);
      }
    })();

    return () => {
      live = false;
    };
  }, [attempt]);

  if (!stats && !error) {
    return (
      <div className="mx-auto flex w-full max-w-4xl flex-col gap-4 p-6">
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-5 p-6">
      {error ? (
        <p role="alert" className="rounded-md border border-destructive/30 bg-destructive/5 px-4 py-3 text-[15px] text-destructive">
          {error}{" "}
          <button
            type="button"
            onClick={() => setAttempt((count) => count + 1)}
            className="cursor-pointer font-semibold underline"
          >
            Coba lagi
          </button>
        </p>
      ) : null}

      {stats ? (
        <>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Stat label="Total akun" value={stats.total} />
            <Stat
              label="Admin"
              value={stats.admins}
              hint="Boleh membuka halaman ini"
            />
            <Stat label="Pengguna biasa" value={stats.users} />
            <Stat label="Baru 7 hari" value={stats.new_last_7_days} />
          </div>

          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2 font-display">
                <UserCheck className="size-4 text-muted-foreground" aria-hidden="true" />
                Keadaan akun
              </CardTitle>
              <CardDescription>
                Yang menentukan apakah sebuah akun bisa dipakai, bukan sekadar terdaftar.
              </CardDescription>
            </CardHeader>
            <CardContent className="flex flex-col gap-3">
              <div className="grid gap-4 sm:grid-cols-3">
                <Stat
                  label="Email terverifikasi"
                  value={stats.verified}
                  hint={`${stats.total - stats.verified} belum`}
                />
                <Stat
                  label="Sudah onboarding"
                  value={stats.onboarded}
                  hint={`${stats.total - stats.onboarded} belum punya workspace`}
                />
                <Stat label="Akun Google" value={stats.google_accounts} />
              </div>

              <div className="flex flex-wrap items-center gap-3 pt-1">
                <Badge variant="secondary" className="gap-1.5">
                  <Users className="size-3" aria-hidden="true" />
                  {stats.admins} dari {stats.total} akun adalah admin
                </Badge>
                <Button asChild size="sm" variant="outline">
                  <Link href="/admin/users">Kelola pengguna</Link>
                </Button>
              </div>
            </CardContent>
          </Card>
        </>
      ) : null}
    </div>
  );
}
