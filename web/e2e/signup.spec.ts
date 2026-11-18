/**
 * /signup is the one page that exists before an account does. It shows the
 * server's terms, registers, and tells the person what happens next; when a
 * deployment keeps sign-up closed the page says so instead of a form.
 */
import { test, expect } from "@playwright/test";
import { mockApi, signupOptions } from "./mocks";

test("a visitor registers and is told to check their mail", async ({ page }) => {
  await mockApi(page);
  await page.goto("/signup");
  const form = page.getByTestId("sign-up");
  await expect(form).toContainText("Create your workspace");
  // The terms come from the server's plan, never from the page.
  const terms = page.getByTestId("sign-up-terms");
  await expect(terms).toContainText("Community");
  await expect(terms).toContainText("Up to 100 MB of data. No card needed.");
  // The agreement names documents that exist; /terms and /privacy render them.
  const legal = page.getByTestId("sign-up-legal");
  await expect(legal.getByRole("link", { name: "terms of service" })).toHaveAttribute("href", "/terms");
  await expect(legal.getByRole("link", { name: "privacy notice" })).toHaveAttribute("href", "/privacy");

  await expect(page.getByRole("button", { name: "Create workspace" })).toBeDisabled();
  await page.getByLabel("Company").fill("Acme Test");
  await page.getByLabel("First name").fill("Ann");
  await page.getByLabel("Last name").fill("Lee");
  await page.getByLabel("Work e-mail").fill("ann@acme.test");
  const posted = page.waitForRequest((r) => r.method() === "POST" && r.url().endsWith("/api/signup"));
  await page.getByRole("button", { name: "Create workspace" }).click();
  const body = (await posted).postDataJSON() as Record<string, string>;
  expect(body).toEqual({ company: "Acme Test", first_name: "Ann", last_name: "Lee", email: "ann@acme.test" });

  const done = page.getByTestId("sign-up-done");
  await expect(done).toContainText("Check your e-mail");
  await expect(done).toContainText("ann@acme.test");
  await expect(done).toContainText("Acme Test");
});

test("closed sign-up shows the reason and the way in", async ({ page }) => {
  await mockApi(page, { signup: { enabled: false, reason: "Self-service sign-up is not enabled on this deployment.", contact_url: "https://example.test/contact" } });
  await page.goto("/signup");
  const closed = page.getByTestId("sign-up-closed");
  await expect(closed).toContainText("not enabled on this deployment");
  await expect(closed.getByRole("link", { name: "Contact us" })).toHaveAttribute("href", "https://example.test/contact");
  await expect(closed.getByRole("link", { name: /Sign in/ })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create workspace" })).toHaveCount(0);
});

test("an address that already has an account is sent to sign in", async ({ page }) => {
  await mockApi(page);
  await page.route("**/api/signup", (route) => route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "an account with this e-mail address already exists — sign in instead" }) }));
  await page.goto("/signup");
  await page.getByLabel("Company").fill("Acme Test");
  await page.getByLabel("First name").fill("Ann");
  await page.getByLabel("Last name").fill("Lee");
  await page.getByLabel("Work e-mail").fill("ann@acme.test");
  await page.getByRole("button", { name: "Create workspace" }).click();
  const err = page.getByTestId("sign-up-error");
  await expect(err).toContainText("already exists");
  await expect(err.getByRole("link", { name: "Sign in" })).toBeVisible();
});

// Owner, 2026-11-18: Google sign-in, new sign-ups included. Where the realm
// offers Google the page offers it beside the form; the console then sends
// the person to Google (?sign_in=google) and makes the workspace on return.
test("sign up with Google: offered only where the realm has Google", async ({ page }) => {
  await mockApi(page);
  await page.goto("/signup");
  await expect(page.getByText("Create your workspace")).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign up with Google" })).toHaveCount(0);

  await mockApi(page, { signup: { ...signupOptions, google: true } });
  await page.goto("/signup");
  const google = page.getByRole("button", { name: "Sign up with Google" });
  await expect(google).toBeVisible();
  // Under the same agreement line as the form.
  await expect(page.getByTestId("sign-up-legal")).toBeVisible();
  await google.click();
  await page.waitForURL((u) => u.pathname === "/" && u.searchParams.get("sign_in") === "google");
});
