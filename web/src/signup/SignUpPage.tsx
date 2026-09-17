import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { api, type ApiError, type SignupRequest } from "../api/client";
import { useBrand } from "../branding/brand";

const EMPTY: SignupRequest = { company: "", first_name: "", last_name: "", email: "" };

/**
 * /signup — public self-service registration. Rendered instead of the
 * console (main.tsx), before any sign-in: a visitor has no account yet.
 *
 * The page asks the server what it offers (GET /api/signup/options) and
 * shows exactly that — the plan's name, trial length and limits — so a
 * deployment that closes sign-up, or changes the trial, never has a page
 * promising something else. On success the person is told to check their
 * mail; on the dev stack, where there is no mail, they can open the console
 * as the new account right away.
 */
export function SignUpPage() {
  const brand = useBrand();
  const { data: opts, isLoading } = useQuery({ queryKey: ["signup-options"], queryFn: api.signupOptions });
  const [form, setForm] = useState<SignupRequest>(EMPTY);
  const submit = useMutation({ mutationFn: () => api.signup(form) });
  const set = (patch: Partial<SignupRequest>) => setForm({ ...form, ...patch });
  const ready = form.company.trim().length >= 2 && form.first_name.trim() && form.last_name.trim() && form.email.includes("@");

  const limits = opts?.plan?.limits;
  const terms = opts?.plan
    ? [
        opts.plan.trial_days > 0 ? `Free for ${opts.plan.trial_days} days` : opts.plan.name,
        limits && (limits.max_users || limits.max_applications || limits.max_models)
          ? "up to " + [
              limits.max_users ? `${limits.max_users} users` : "",
              limits.max_applications ? `${limits.max_applications} application${limits.max_applications === 1 ? "" : "s"}` : "",
              limits.max_models ? `${limits.max_models} model${limits.max_models === 1 ? "" : "s"}` : "",
            ].filter(Boolean).join(", ")
          : "",
      ].filter(Boolean).join(" · ")
    : "";

  return (
    <div className="mvx-signin" style={{ maxWidth: 440, margin: "10vh auto", padding: 24 }} data-testid="sign-up">
      {brand.configured && brand.logo_data_url && <img src={brand.logo_data_url} alt={brand.name} style={{ height: 36, marginBottom: 12 }} />}
      <h1 style={{ fontSize: 22, marginBottom: 4 }}>Start with {brand.name}</h1>

      {isLoading && <p style={{ color: "var(--color-text-muted)" }}>Loading…</p>}

      {opts && !opts.enabled && (
        <div data-testid="sign-up-closed">
          <p style={{ color: "var(--color-text-muted)" }}>{opts.reason ?? "Sign-up is not available."}</p>
          {opts.contact_url && <p><a href={opts.contact_url}>Contact us</a> to get started.</p>}
          <p><a href="/">Already have an account? Sign in</a></p>
        </div>
      )}

      {opts?.enabled && submit.isSuccess && (
        <div data-testid="sign-up-done" role="status">
          {submit.data.invited ? (
            <>
              <h2 style={{ fontSize: 17 }}>Check your e-mail</h2>
              <p>
                We sent an invitation to <strong>{submit.data.email}</strong>. Follow the link to choose your password;
                it is valid for three days. Your workspace, <em>{form.company.trim()}</em>, is ready with a starter model to explore.
              </p>
              <p><a href="/">Go to sign in</a></p>
            </>
          ) : (
            <>
              <h2 style={{ fontSize: 17 }}>Your workspace is ready</h2>
              <p>
                This deployment has no identity provider, so no invitation was sent. Open the console as the new account instead.
              </p>
              <button className="mvx-button mvx-button--primary" type="button"
                onClick={() => { if (submit.data.dev_persona) localStorage.setItem("dev_persona", submit.data.dev_persona); window.location.assign("/"); }}>
                Open the console
              </button>
            </>
          )}
        </div>
      )}

      {opts?.enabled && !submit.isSuccess && (
        <>
          <p style={{ color: "var(--color-text-muted)", marginTop: 0 }} data-testid="sign-up-terms">
            {terms || "Create your workspace."} No card needed.
          </p>
          <form onSubmit={(e) => { e.preventDefault(); if (ready) submit.mutate(); }} style={{ display: "grid", gap: 10 }}>
            <input className="mvx-input" value={form.company} onChange={(e) => set({ company: e.target.value })}
              placeholder="Company" aria-label="Company" autoComplete="organization" maxLength={80} />
            <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10 }}>
              <input className="mvx-input" value={form.first_name} onChange={(e) => set({ first_name: e.target.value })}
                placeholder="First name" aria-label="First name" autoComplete="given-name" maxLength={80} />
              <input className="mvx-input" value={form.last_name} onChange={(e) => set({ last_name: e.target.value })}
                placeholder="Last name" aria-label="Last name" autoComplete="family-name" maxLength={80} />
            </div>
            <input className="mvx-input" type="email" value={form.email} onChange={(e) => set({ email: e.target.value })}
              placeholder="you@company.com" aria-label="Work e-mail" autoComplete="email" />
            <button className="mvx-button mvx-button--primary" type="submit" disabled={!ready || submit.isPending}>
              {submit.isPending ? "Creating your workspace…" : "Create workspace"}
            </button>
          </form>
          {submit.isError && (
            <p role="alert" style={{ color: "var(--color-danger-600, #b42318)" }} data-testid="sign-up-error">
              {friendlyError(submit.error as ApiError)}
              {(submit.error as ApiError).status === 409 && <> <a href="/">Sign in</a></>}
            </p>
          )}
          <p style={{ color: "var(--color-text-muted)" }}>
            By creating a workspace you agree to the terms of the {brand.name} service.
            {" "}<a href="/">Already have an account? Sign in</a>
          </p>
        </>
      )}
    </div>
  );
}

function friendlyError(err: ApiError): string {
  const msg = (err.body?.error as string | undefined) ?? err.message;
  return msg.replace(/^\d+: ?/, "");
}
