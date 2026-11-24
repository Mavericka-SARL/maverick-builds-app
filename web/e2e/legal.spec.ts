/**
 * /terms and /privacy as the console renders them: behind a proxy that sends
 * those paths to the console rather than to the gateway (which serves them as
 * plain HTML itself, internal/gateway/legal_pages.go). The text is the
 * gateway's — GET /api/legal/documents/{doc}, whose Go tests check what it
 * says. What is worth holding still here: the documents render outside the
 * auth layer, as the server wrote them, and a deployment which has published
 * nothing says so instead of showing an unsigned policy.
 */
import { test, expect } from "@playwright/test";
import { mockApi, legalUnpublished } from "./mocks";

test("the terms render the server's document outside the auth layer", async ({ page }) => {
  await mockApi(page);
  await page.goto("/terms");
  const doc = page.getByTestId("legal-terms");
  await expect(doc.getByRole("heading", { name: "Terms of service", level: 1 })).toBeVisible();
  await expect(doc).toContainText("The agreement between you and Acme Software SARL for the use of maverickbuilds.app.");
  // The date comes from GET /api/legal, formatted for reading.
  await expect(doc).toContainText("In force from 18 September 2026");
  await expect(doc.getByRole("heading", { name: "4. What you may not do", level: 2 })).toBeVisible();
  // A list in the document is a list on the page.
  await expect(doc.getByRole("listitem")).toHaveCount(2);
  await expect(doc.getByRole("listitem").first()).toContainText("do not use it to store or process unlawful content");
  await expect(doc.getByRole("link", { name: "Privacy notice" })).toHaveAttribute("href", "/privacy");
});

test("the privacy notice renders the server's document", async ({ page }) => {
  await mockApi(page);
  await page.goto("/privacy");
  const doc = page.getByTestId("legal-privacy");
  await expect(doc.getByRole("heading", { name: "Privacy notice", level: 1 })).toBeVisible();
  await expect(doc).toContainText("is the controller of the personal data described here");
  await expect(doc.getByRole("link", { name: "Terms of service" })).toHaveAttribute("href", "/terms");
});

test("a document the server cannot send says so rather than loading forever", async ({ page }) => {
  await mockApi(page, { legalDocuments: {} });
  await page.goto("/terms");
  await expect(page.getByTestId("legal-terms")).toContainText("The document could not be loaded");
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
