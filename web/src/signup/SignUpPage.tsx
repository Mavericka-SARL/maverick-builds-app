import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { api, type ApiError, type SignupRequest } from "../api/client";
import { PublicPage } from "../public/PublicPage";
import { planAllowance } from "../public/planAllowance";
import { externalAttrs } from "../public/legalLinks";
import "../public/public.css";

const EMPTY: SignupRequest = { company: "", first_name: "", last_name: "", email: "" };

/**
 * /signup — public self-service registration. Rendered instead of the
 * console (main.tsx), before any sign-in: a visitor has no account yet.
 *
 * The page asks the server what it offers (GET /api/signup/options) and
 * shows exactly that — the plan's name and limits — so a deployment that
 * closes sign-up, or changes the plan, never has a page promising something
 * else. On success the person is told to check their
 * mail; on the dev stack, where there is no mail, they can open the console
 * as the new account right away.
 */
export function SignUpPage() {
  const { data: opts, isLoading } = useQuery({ queryKey: ["signup-options"], queryFn: api.signupOptions });
  // What this deployment publishes, so the line under the form claims
  // agreement to documents that exist and to nothing else.
  const { data: legal } = useQuery({ queryKey: ["legal"], queryFn: api.legal });
  const [form, setForm] = useState<SignupRequest>(EMPTY);
  const submit = useMutation({ mutationFn: () => api.signup(form) });
  const set = (patch: Partial<SignupRequest>) => setForm({ ...form, ...patch });
  const ready = form.company.trim().length >= 2 && form.first_name.trim() && form.last_name.trim() && form.email.includes("@");

  const allowance = planAllowance(opts?.plan?.limits);

  return (
    <PublicPage testId="sign-up">
      {isLoading && <p>Loading…</p>}

      {opts && !opts.enabled && (
        <div data-testid="sign-up-closed">
          <h1>Not open yet</h1>
          <p>{opts.reason ?? "Sign-up is not available."}</p>
          {opts.contact_url && (
            <p>
              <a href={opts.contact_url}>Contact us</a> to get started.
            </p>
          )}
          <p className="mvx-public__footer">
            <a href="/">Already have an account? Sign in</a>
          </p>
        </div>
      )}

      {opts?.enabled && submit.isSuccess && (
        <div data-testid="sign-up-done" role="status">
          {submit.data.invited ? (
            <>
              <h1>Check your e-mail</h1>
              <p>
                We sent an invitation to <strong style={{ color: "var(--public-text)" }}>{submit.data.email}</strong>. Follow the
                link to choose your password; it is valid for three days.
              </p>
              <p>
                Your workspace, <strong style={{ color: "var(--public-text)" }}>{form.company.trim()}</strong>, is ready with a
                short tour of the platform to walk through.
              </p>
              <p className="mvx-public__footer">
                <a href="/">Go to sign in</a>
              </p>
            </>
          ) : (
            <>
              <h1>Your workspace is ready</h1>
              <p>This deployment has no identity provider, so no invitation was sent. Open the console as the new account instead.</p>
              <button
                className="mvx-public__button"
                type="button"
                onClick={() => {
                  if (submit.data.dev_persona) localStorage.setItem("dev_persona", submit.data.dev_persona);
                  window.location.assign("/");
                }}
              >
                Open the console
              </button>
            </>
          )}
        </div>
      )}

      {opts?.enabled && !submit.isSuccess && (
        <>
          <h1>Create your workspace</h1>
          <p className="mvx-public__terms-line" data-testid="sign-up-terms">
            {opts.plan ? <span className="mvx-public__badge">{opts.plan.name}</span> : null}
            <span>{allowance ? `Up to ${allowance}. No card needed.` : "No card needed."}</span>
          </p>

          <form
            className="mvx-public__form"
            onSubmit={(e) => {
              e.preventDefault();
              if (ready) submit.mutate();
            }}
          >
            <input
              className="mvx-public__field"
              value={form.company}
              onChange={(e) => set({ company: e.target.value })}
              placeholder="Company"
              aria-label="Company"
              autoComplete="organization"
              maxLength={80}
            />
            <div className="mvx-public__row">
              <input
                className="mvx-public__field"
                value={form.first_name}
                onChange={(e) => set({ first_name: e.target.value })}
                placeholder="First name"
                aria-label="First name"
                autoComplete="given-name"
                maxLength={80}
              />
              <input
                className="mvx-public__field"
                value={form.last_name}
                onChange={(e) => set({ last_name: e.target.value })}
                placeholder="Last name"
                aria-label="Last name"
                autoComplete="family-name"
                maxLength={80}
              />
            </div>
            <input
              className="mvx-public__field"
              type="email"
              value={form.email}
              onChange={(e) => set({ email: e.target.value })}
              placeholder="you@company.com"
              aria-label="Work e-mail"
              autoComplete="email"
            />
            <button className="mvx-public__button" type="submit" disabled={!ready || submit.isPending}>
              {submit.isPending ? "Creating your workspace…" : "Create workspace"}
            </button>
          </form>

          {submit.isError && (
            <p role="alert" className="mvx-public__error" data-testid="sign-up-error">
              {friendlyError(submit.error as ApiError)}
              {(submit.error as ApiError).status === 409 && (
                <>
                  {" "}
                  <a href="/">Sign in</a>
                </>
              )}
            </p>
          )}

          {legal?.published && (
            <p className="mvx-public__terms" data-testid="sign-up-legal">
              By creating a workspace you agree to the{" "}
              <a href={legal.terms_url} {...externalAttrs(legal.terms_url)}>
                terms of service
              </a>{" "}
              and the{" "}
              <a href={legal.privacy_url} {...externalAttrs(legal.privacy_url)}>
                privacy notice
              </a>
              .
            </p>
          )}
          <p className="mvx-public__footer">
            <a href="/">Already have an account? Sign in</a>
          </p>
        </>
      )}
    </PublicPage>
  );
}

function friendlyError(err: ApiError): string {
  const msg = (err.body?.error as string | undefined) ?? err.message;
  return msg.replace(/^\d+: ?/, "");
}
