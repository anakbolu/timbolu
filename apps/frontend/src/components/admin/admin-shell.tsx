"use client";

/**
 * The operator shell: the frame /admin's screens live in.
 *
 * It is a second shell rather than a mode of the workspace one, because the two
 * answer different questions. The workspace shell is built around a tenant — the
 * roster, the groups, the pending drafts — and every one of those reads a
 * workspace. This one has no workspace at all: an admin administers accounts,
 * and an account that belongs to nothing is still on the list. Sharing a shell
 * would mean carrying a roster into a screen that has no use for one.
 *
 * The frame itself is shadcn's sidebar, which is why the layout is the registry's
 * own: a collapsible rail on the desktop, a sheet on a phone, and a header with
 * the trigger that moves between them.
 */

import { LayoutGrid, ShieldCheck, Users } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";

import { Bolu } from "@/components/bolu/bolu";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import { ThemeToggle } from "@/components/ui/theme-toggle";
import type { User } from "@/lib/api";
import { LOGO } from "@/lib/crew";

/** The two screens the surface has, in the order a person meets them. */
const NAV = [
  {
    href: "/admin/dashboard",
    label: "Ringkasan",
    description: "Jumlah akun dan yang baru masuk",
    icon: LayoutGrid,
  },
  {
    href: "/admin/users",
    label: "Pengguna",
    description: "Daftar akun dan perannya",
    icon: Users,
  },
];

/** The title the mobile bar and the page header show for a route. */
const TITLES: Record<string, string> = {
  "/admin/dashboard": "Ringkasan",
  "/admin/users": "Pengguna",
};

export function AdminShell({ user, children }: { user: User; children: React.ReactNode }) {
  const pathname = usePathname();
  const title = TITLES[pathname] ?? "Admin";

  return (
    <SidebarProvider>
      <Sidebar>
        <SidebarHeader className="border-b border-sidebar-border">
          <div className="flex items-center gap-2.5 px-1 py-1.5">
            <Bolu member={LOGO} size={30} />
            <div className="flex min-w-0 flex-col leading-tight">
              <span className="font-display text-lg font-bold">bolu</span>
              <span className="text-[11px] font-semibold tracking-wide text-muted-foreground uppercase">
                Admin
              </span>
            </div>
          </div>
        </SidebarHeader>

        <SidebarContent>
          <SidebarGroup>
            <SidebarGroupLabel>Deployment</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {NAV.map((item) => (
                  <SidebarMenuItem key={item.href}>
                    <SidebarMenuButton
                      asChild
                      isActive={pathname === item.href}
                      tooltip={item.label}
                      className="h-auto py-2"
                    >
                      <Link href={item.href}>
                        <item.icon aria-hidden="true" />
                        <span className="flex min-w-0 flex-col text-left">
                          <span className="font-semibold">{item.label}</span>
                          <span className="truncate text-[11px] text-muted-foreground">
                            {item.description}
                          </span>
                        </span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        </SidebarContent>

        <SidebarFooter className="border-t border-sidebar-border">
          <SidebarMenu>
            <SidebarMenuItem>
              <SidebarMenuButton asChild size="lg">
                <Link href="/dashboard">
                  <ShieldCheck aria-hidden="true" />
                  <span className="flex min-w-0 flex-col text-left">
                    <span className="truncate text-[13px] font-semibold">{user.email}</span>
                    <span className="text-[11px] text-muted-foreground">Kembali ke workspace</span>
                  </span>
                </Link>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarFooter>
      </Sidebar>

      <SidebarInset>
        <header className="sticky top-0 z-10 flex h-14 flex-none items-center gap-2 border-b border-border bg-background/85 px-4 backdrop-blur">
          <SidebarTrigger />
          <h1 className="font-display text-lg font-bold">{title}</h1>
          <ThemeToggle className="ml-auto" />
        </header>
        <div className="flex-1 bg-bolu-bg">{children}</div>
      </SidebarInset>
    </SidebarProvider>
  );
}
