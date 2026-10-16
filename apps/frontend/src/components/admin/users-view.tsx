"use client";

/**
 * The account list: every account on the deployment, with the one action the
 * surface offers.
 *
 * The two refusals the API can answer with — the last admin, and an admin
 * demoting themselves — are shown on the row that asked for the change rather
 * than as a page-level error, because they are facts about that row. The button
 * for a self-demotion is disabled outright: the API would refuse it, and a
 * button that always fails is worse than one that says why it cannot be pressed.
 */

import { Search, ShieldCheck, ShieldMinus } from "lucide-react";
import { useEffect, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  adminSetRole,
  adminUsers,
  currentUser,
  type AdminAccount,
  type AdminPage,
} from "@/lib/api";

/** The page size the list asks for. */
const PAGE_SIZE = 25;

/** The role filters the surface offers, in the order they narrow the list. */
const ROLE_FILTERS = [
  { value: "", label: "Semua" },
  { value: "admin", label: "Admin" },
  { value: "user", label: "Pengguna" },
];

function formatDate(value: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleDateString("id-ID", { day: "numeric", month: "short", year: "numeric" });
}

export function AdminUsersView() {
  const [page, setPage] = useState<AdminPage | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [role, setRole] = useState("");
  const [offset, setOffset] = useState(0);

  // The read is re-run by changing this rather than by calling a function that
  // sets state: an effect body may not set state synchronously, and a counter is
  // the smallest thing that makes the effect re-run on demand.
  const [attempt, setAttempt] = useState(0);

  const [selfID, setSelfID] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [rowError, setRowError] = useState<{ id: string; message: string } | null>(null);

  useEffect(() => {
    let live = true;

    void (async () => {
      const account = await currentUser();
      if (live && account.ok) setSelfID(account.data.id);
    })();

    return () => {
      live = false;
    };
  }, []);

  useEffect(() => {
    let live = true;

    void (async () => {
      const result = await adminUsers({ search: query, role, limit: PAGE_SIZE, offset });
      if (!live) return;

      if (result.ok) {
        setPage(result.data);
        setError(null);
      } else {
        setError(result.message);
      }
    })();

    return () => {
      live = false;
    };
  }, [query, role, offset, attempt]);

  const changeRole = async (account: AdminAccount) => {
    const next = account.role === "admin" ? "user" : "admin";
    setBusy(account.id);
    setRowError(null);

    const result = await adminSetRole(account.id, next);
    setBusy(null);

    if (!result.ok) {
      setRowError({ id: account.id, message: result.message });
      return;
    }

    // The row is replaced rather than the list reloaded: the answer is the row
    // the list would show, and reloading would throw away the filter and the
    // page the person is on.
    setPage((current) =>
      current
        ? {
            ...current,
            accounts: current.accounts.map((entry) =>
              entry.id === result.data.id ? result.data : entry,
            ),
          }
        : current,
    );
  };

  const total = page?.total ?? 0;
  const shown = page?.accounts.length ?? 0;
  const from = total === 0 ? 0 : offset + 1;
  const to = offset + shown;

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-5 p-6">
      <Card>
        <CardHeader>
          <CardTitle className="font-display">Akun</CardTitle>
          <CardDescription>
            Setiap akun di deployment ini. Admin boleh membuka halaman admin; pengguna biasa
            hanya memakai workspace-nya.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              setOffset(0);
              setQuery(search.trim());
            }}
          >
            <div className="relative min-w-[220px] flex-1">
              <Search
                className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
                aria-hidden="true"
              />
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder="Cari alamat email"
                aria-label="Cari alamat email"
                className="pl-8"
              />
            </div>
            <Button type="submit" variant="secondary">
              Cari
            </Button>
            <div className="flex items-center gap-1">
              {ROLE_FILTERS.map((filter) => (
                <Button
                  key={filter.value}
                  type="button"
                  size="sm"
                  variant={role === filter.value ? "default" : "outline"}
                  onClick={() => {
                    setRole(filter.value);
                    setOffset(0);
                  }}
                >
                  {filter.label}
                </Button>
              ))}
            </div>
          </form>

          {error ? (
            <p
              role="alert"
              className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-[14px] text-destructive"
            >
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

          {!page && !error ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Email</TableHead>
                  <TableHead>Peran</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Workspace</TableHead>
                  <TableHead>Dibuat</TableHead>
                  <TableHead className="text-right">Aksi</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(page?.accounts ?? []).map((account) => {
                  const isAdmin = account.role === "admin";
                  const isSelf = account.id === selfID;
                  return (
                    <TableRow key={account.id}>
                      <TableCell className="font-medium">
                        <div className="flex flex-col">
                          <span>{account.email}</span>
                          {rowError?.id === account.id ? (
                            <span role="alert" className="text-[12px] text-destructive">
                              {rowError.message}
                            </span>
                          ) : null}
                        </div>
                      </TableCell>
                      <TableCell>
                        <Badge variant={isAdmin ? "default" : "secondary"}>{account.role}</Badge>
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {account.email_verified ? (
                            <Badge variant="outline">terverifikasi</Badge>
                          ) : (
                            <Badge variant="outline" className="text-muted-foreground">
                              belum verifikasi
                            </Badge>
                          )}
                          {account.has_google ? <Badge variant="outline">google</Badge> : null}
                          {!account.onboarded ? (
                            <Badge variant="outline" className="text-muted-foreground">
                              tanpa workspace
                            </Badge>
                          ) : null}
                        </div>
                      </TableCell>
                      <TableCell className="text-right">{account.workspaces}</TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatDate(account.created_at)}
                      </TableCell>
                      <TableCell className="text-right">
                        <Button
                          size="sm"
                          variant={isAdmin ? "outline" : "default"}
                          disabled={busy === account.id || (isAdmin && isSelf)}
                          title={isAdmin && isSelf ? "Tidak bisa menurunkan peran sendiri" : undefined}
                          onClick={() => void changeRole(account)}
                        >
                          {isAdmin ? (
                            <>
                              <ShieldMinus aria-hidden="true" />
                              Jadikan pengguna
                            </>
                          ) : (
                            <>
                              <ShieldCheck aria-hidden="true" />
                              Jadikan admin
                            </>
                          )}
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
                {page && page.accounts.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={6} className="py-6 text-center text-muted-foreground">
                      Tidak ada akun yang cocok dengan filter ini.
                    </TableCell>
                  </TableRow>
                ) : null}
              </TableBody>
            </Table>
          )}

          <div className="flex flex-wrap items-center justify-between gap-3 text-[13px] text-muted-foreground">
            <span>{total === 0 ? "0 akun" : `${from}–${to} dari ${total} akun`}</span>
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="outline"
                disabled={offset === 0}
                onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              >
                Sebelumnya
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={to >= total}
                onClick={() => setOffset(offset + PAGE_SIZE)}
              >
                Berikutnya
              </Button>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
