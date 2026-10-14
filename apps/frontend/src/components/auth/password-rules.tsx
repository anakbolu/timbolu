"use client";

/**
 * The password policy as a live checklist under the field, so the rules are
 * visible while typing instead of arriving afterwards as a rejection.
 *
 * These mirror `ValidatePassword` in the backend
 * (`internal/platform/authn/password.go`); a rule that is not enforced there
 * must not be listed here, or the list stops being trustworthy.
 */

import { Check, X } from "lucide-react";

type Rule = {
  label: string;
  test: (value: string) => boolean;
};

const RULES: Rule[] = [
  { label: "Minimal 8 karakter", test: (value) => [...value].length >= 8 },
  {
    label: "Ada huruf besar dan huruf kecil",
    // \p{Lu}/\p{Ll} are the Unicode categories the backend checks.
    test: (value) => /\p{Lu}/u.test(value) && /\p{Ll}/u.test(value),
  },
  { label: "Ada angka", test: (value) => /\p{Nd}/u.test(value) },
  { label: "Bukan hanya spasi", test: (value) => value.trim() !== "" },
];

/** Whether the value satisfies every rule, so a form can gate its submit. */
export function passwordMeetsPolicy(value: string): boolean {
  return RULES.every((rule) => rule.test(value));
}

export function PasswordRules({ value }: { value: string }) {
  return (
    <ul className="space-y-1 text-xs">
      {RULES.map((rule) => {
        const met = rule.test(value);
        return (
          <li
            key={rule.label}
            className={met ? "flex items-center gap-1.5 text-bolu-lunas" : "flex items-center gap-1.5 text-bolu-muted"}
          >
            {met ? (
              <Check aria-hidden className="size-3.5 flex-none" />
            ) : (
              <X aria-hidden className="size-3.5 flex-none" />
            )}
            <span>{rule.label}</span>
            <span className="sr-only">{met ? " — terpenuhi" : " — belum terpenuhi"}</span>
          </li>
        );
      })}
    </ul>
  );
}
