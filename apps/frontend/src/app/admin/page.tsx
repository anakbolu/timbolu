import { redirect } from "next/navigation";

/**
 * `/admin` is the surface's name, and `/admin/dashboard` is its front door: the
 * redirect is here so a person who types the shorter URL lands on the screen
 * rather than on a 404.
 */
export default function AdminIndexPage() {
  redirect("/admin/dashboard");
}
