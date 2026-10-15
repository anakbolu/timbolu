import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * cn merges class names the way shadcn's components expect: later classes win
 * over conflicting earlier ones, so a component's own variant classes can be
 * overridden by whatever the caller passes.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
