/**
 * /terms and /privacy: the documents a visitor is asked to agree to on
 * /signup. Three things are worth holding still — that the documents render
 * outside the auth layer, that the terms quote the plan the server actually
 * offers rather than a number typed into the page, and that a deployment which
 * has published nothing says so instead of showing an unsigned policy.
 */
import { test, expect } from "@playwright/test";
import { mockApi, legalUnpublished, signupOptionsSmall } from "./mocks";

test("the terms name the operator and quote the server's plan", async ({ page }) => {
  await mockApi(page);
  await page.goto("/terms");
  const doc = page.getByTestId("legal-terms");
  await expect(doc.getByRole("heading", { name: "Terms of service", level: 1 })).toBeVisible();
  // The operator comes from GET /api/legal, never from the bundle.
  await expect(doc).toContainText("Acme Software SARL");
  await expect(doc).toContainText("1 Rue de Test, L-1000 Luxembourg");
  await expect(doc).toContainText("legal@acme.test");
  await expect(doc).toContainText("In force from 18 September 2026");
  // The plan clause is the plan from GET /api/signup/options: free, no
  // end date, 100 MB of data, read-only when the space is used up.
  await expect(doc).toContainText("Test workspace plan costs nothing and has no end date");
  await expect(doc).toContainText("limited to 100 MB of data");
  await expect(doc).toContainText("becomes read-only");
  // …and the plan's own way forward, never "a plan is agreed".
  await expect(doc).toContainText("run the platform on your own infrastructure");
  await expect(doc).not.toContainText("a plan is agreed");
  await expect(doc).toContainText("law of Luxembourg");
  await expect(doc.getByRole("link", { name: "Privacy notice" })).toHaveAttribute("href", "/privacy");
});

test("a plan without a note of its own still gets the plan clause, with no trial in it", async ({ page }) => {
  await mockApi(page, { signup: signupOptionsSmall });
  await page.goto("/terms");
  const doc = page.getByTestId("legal-terms");
  await expect(doc).toContainText("Small plan costs nothing and has no end date");
  await expect(doc).toContainText("5 users, 2 applications and 3 models");
  await expect(doc).not.toContainText(/trial/i);
});

test("the privacy notice states what is held and who else sees it", async ({ page }) => {
  await mockApi(page);
  await page.goto("/privacy");
  const doc = page.getByTestId("legal-privacy");
  await expect(doc.getByRole("heading", { name: "Privacy notice", level: 1 })).toBeVisible();
  await expect(doc).toContainText("is the controller of the personal data");
  await expect(doc).toContainText("no analytics, tracking or advertising technology");
  await expect(doc).toContainText("Hetzner Online GmbH (Germany)");
  await expect(doc).toContainText("kept for fourteen days");
  await expect(doc).toContainText("complain to a data-protection supervisory authority");
  await expect(doc.getByRole("link", { name: "Terms of service" })).toHaveAttribute("href", "/terms");
});

test("a deployment that publishes nothing says so, and /signup promises nothing", async ({ page }) => {
  await mockApi(page, { legal: legalUnpublished });
  await page.goto("/terms");
  await expect(page.getByTestId("legal-unpublished")).toContainText("has not published terms of service");

  await page.goto("/signup");
  await expect(page.getByTestId("sign-up")).toContainText("Create your workspace");
  await expect(page.getByTestId("sign-up-legal")).toHaveCount(0);
});

test("an operator's own documents elsewhere replace the shipped pages", async ({ page }) => {
  await mockApi(page, {
    legal: { ...legalUnpublished, published: true, terms_url: "https://acme.test/legal/terms", privacy_url: "https://acme.test/legal/privacy" },
  });
  await page.goto("/signup");
  const line = page.getByTestId("sign-up-legal");
  await expect(line.getByRole("link", { name: "terms of service" })).toHaveAttribute("href", "https://acme.test/legal/terms");
  await expect(line.getByRole("link", { name: "terms of service" })).toHaveAttribute("target", "_blank");
});
