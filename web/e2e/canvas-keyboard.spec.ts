import { test, expect } from "@playwright/test";
import { mockApi, loadAs } from "./mocks";

// The dashboard canvas is a drag-and-drop surface, so its keyboard path is
// the whole accessibility story. Widgets were already focusable and arrow
// keys already moved a SELECTED widget — but selection only ever happened on
// mouse click, so tabbing to a widget and pressing an arrow did nothing, and
// the container set `outline: none`, leaving a focusable element with no
// visible focus state. Resizing had no keyboard path at all.
test.describe("dashboard canvas keyboard access", () => {
  test.beforeEach(async ({ page }) => {
    await mockApi(page);
    await loadAs(page, "developer");
    await page.getByRole("button", { name: /dashboards/i }).first().click();
    await page.waitForTimeout(500);
    // Open the first dashboard's designer.
    await page.getByRole("button", { name: /design/i }).first().click();
    await page.waitForTimeout(500);
  });

  test("a widget can be reached, selected and moved with the keyboard alone", async ({ page }) => {
    const widget = page.locator(".mvx-canvas-widget").first();
    await expect(widget).toBeVisible();

    const before = await widget.boundingBox();
    await widget.focus();

    // Focusing selects — otherwise the arrow-key handler has no target.
    await expect(widget).toBeFocused();

    await page.keyboard.press("ArrowRight");
    await page.waitForTimeout(150);
    const after = await widget.boundingBox();

    expect(before && after).toBeTruthy();
    expect(after!.x).toBeGreaterThan(before!.x);
  });

  test("a widget reached by Tab shows a visible focus ring", async ({ page }) => {
    const widget = page.locator(".mvx-canvas-widget").first();
    await expect(widget).toBeVisible();

    // Tabbed to for real, not focused programmatically: :focus-visible is
    // driven by the browser's own keyboard heuristic, so element.focus() from
    // script wouldn't exercise the rule a keyboard user actually sees. This
    // also tests the more basic question — is the widget reachable by Tab at
    // all.
    let reached = false;
    for (let i = 0; i < 60 && !reached; i++) {
      await page.keyboard.press("Tab");
      reached = await widget.evaluate(el => el === document.activeElement);
    }
    expect(reached, "the canvas widget was never reached by tabbing").toBe(true);

    const outline = await widget.evaluate(el => getComputedStyle(el).outlineStyle);
    expect(outline).not.toBe("none");
  });

  test("resize handles are reachable and labelled distinctly", async ({ page }) => {
    for (const name of [/resize widget width$/i, /resize widget height$/i, /resize widget width and height/i]) {
      const handle = page.getByRole("button", { name }).first();
      await expect(handle).toBeVisible();
    }
  });

  test("a widget can be resized from the keyboard", async ({ page }) => {
    const widget = page.locator(".mvx-canvas-widget").first();
    const before = await widget.boundingBox();

    const handle = page.getByRole("button", { name: /resize widget width$/i }).first();
    await handle.focus();
    await page.keyboard.press("ArrowRight");
    await page.waitForTimeout(150);

    const after = await widget.boundingBox();
    expect(before && after).toBeTruthy();
    expect(after!.width).toBeGreaterThan(before!.width);
  });
});
