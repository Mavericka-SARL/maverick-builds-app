/**
 * /signup is the one page that exists before an account does. It shows the
 * server's terms, registers, and tells the person what happens next; when a
 * deployment keeps sign-up closed the page says so instead of a form.
 */
import { test, expect } from "@playwright/test";
import { mockApi } from "./mocks";

test("a visitor registers and is told to check their mail", async ({ page }) => {
  await mockApi(page);
  await page.goto("/signup");
  const form = page.getByTestId("sign-up");
  await expect(form).toContainText("Start with");
  await expect(page.getByTestId("sign-up-terms")).toContainText("Free for 14 days · up to 5 users, 2 applications, 3 models");

  await expect(page.getByRole("button", { name: "Create workspace" })).toBeDisabled();
  await page.getByLabel("Company").fill("Acme Trial");
  await page.getByLabel("First name").fill("Ann");
  await page.getByLabel("Last name").fill("Lee");
  await page.getByLabel("Work e-mail").fill("ann@acme.test");
  const posted = page.waitForRequest((r) => r.method() === "POST" && r.url().endsWith("/api/signup"));
  await page.getByRole("button", { name: "Create workspace" }).click();
  const body = (await posted).postDataJSON() as Record<string, string>;
  expect(body).toEqual({ company: "Acme Trial", first_name: "Ann", last_name: "Lee", email: "ann@acme.test" });

  const done = page.getByTestId("sign-up-done");
  await expect(done).toContainText("Check your e-mail");
  await expect(done).toContainText("ann@acme.test");
  await expect(done).toContainText("Acme Trial");
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
  await page.getByLabel("Company").fill("Acme Trial");
  await page.getByLabel("First name").fill("Ann");
  await page.getByLabel("Last name").fill("Lee");
  await page.getByLabel("Work e-mail").fill("ann@acme.test");
  await page.getByRole("button", { name: "Create workspace" }).click();
  const err = page.getByTestId("sign-up-error");
  await expect(err).toContainText("already exists");
  await expect(err.getByRole("link", { name: "Sign in" })).toBeVisible();
});
