/**
 * The sidebar brand block and the context bar sit side by side across the top
 * of every console, and each draws its own bottom border. Nothing structural
 * keeps them the same height — they live in different columns — so the two
 * rules only line up because both are sized from --context-bar-height.
 *
 * That is easy to break without noticing: raising the height of anything
 * inside the context bar (a taller chip, a button) makes its content exceed
 * the shared min-height, and the bar alone grows. It already happened once,
 * leaving a 3px step visible against the sidebar's rule.
 *
 * Reading the CSS does not catch it, because both rules genuinely name the
 * same token. Only the rendered boxes show the difference.
 */
import { test, expect, type Page } from "@playwright/test";

async function setPersona(page: Page, persona: string) {
  await page.addInitScript((p) => {
    if (!localStorage.getItem("dev_persona")) localStorage.setItem("dev_persona", p);
  }, persona);
}

// One per console, since each fills the context bar with different content and
// the tallest item is what pushes the bar past the shared height.
for (const persona of ["developer", "platform_admin", "finance", "dept_head"]) {
  test(`brand and context bar bottom borders align (${persona})`, async ({ page }) => {
    await setPersona(page, persona);
    await page.goto("/");

    const brand = page.locator(".mvx-app-shell__brand");
    const bar = page.locator(".mvx-context-bar");
    await expect(brand).toBeVisible({ timeout: 15_000 });
    await expect(bar).toBeVisible({ timeout: 15_000 });

    const b = await brand.boundingBox();
    const c = await bar.boundingBox();
    if (!b || !c) throw new Error("missing bounding box");

    const brandBottom = b.y + b.height;
    const barBottom = c.y + c.height;
    expect(
      Math.abs(brandBottom - barBottom),
      `brand bottom ${brandBottom}px vs context bar bottom ${barBottom}px — ` +
        "the two header rules no longer meet; something in the context bar is " +
        "taller than --context-bar-height allows",
    ).toBeLessThanOrEqual(1);
  });
}
