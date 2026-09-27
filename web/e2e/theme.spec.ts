/**
 * Light / dark theme: a per-person choice in the account menu, kept on the
 * account (PATCH /api/me/preferences) with a copy in the browser. Light until
 * someone opts in; "System" follows the OS live; the browser copy is applied
 * by index.html before the bundle runs, so a dark page never paints white
 * first; once /api/me answers, the account's choice wins.
 */
import { test, expect, type Page } from "@playwright/test";

const USER_ID = "00000000-0000-0000-0000-000000000009";

interface OpenOptions {
  /** What /api/me says the account has chosen. */
  accountTheme?: "light" | "dark" | "system";
  /** /api/me could not read the preferences: it sends null, not {}. */
  preferencesUnknown?: boolean;
  /** localStorage as this browser left it, before the page loads. */
  stored?: Record<string, string>;
  /** Resolves when /api/me may answer; until then it hangs. */
  meReleased?: Promise<void>;
  /** Answers the nth save (0-based); default 200 echoing the body. */
  answerSave?: (n: number) => Promise<number> | number;
  /** Do not wait for the shell (the caller checks what it needs). */
  noWait?: boolean;
}

/** Opens the console as a mocked developer. Every save to the account is
 *  captured, never sent: returns the list the PATCH bodies land in. */
async function open(page: Page, opts: OpenOptions = {}) {
  await page.addInitScript((stored) => {
    // Seed once: a reload must see what the app itself left behind.
    if (sessionStorage.getItem("seeded")) return;
    sessionStorage.setItem("seeded", "1");
    localStorage.setItem("dev_persona", "developer");
    for (const [k, v] of Object.entries(stored)) localStorage.setItem(k, v);
  }, opts.stored ?? {});
  const actor = {
    user_id: USER_ID,
    email: "signed.in@example.com",
    display_name: "Signed In Person",
    roles: ["developer"],
  };
  // The account as the server holds it: a successful save changes what the
  // next /api/me says, as it would.
  const account: Record<string, unknown> | null = opts.preferencesUnknown ? null : opts.accountTheme ? { theme: opts.accountTheme } : {};
  for (const path of ["**/api/me", "**/api/admin/me"]) {
    await page.route(path, async (route) => {
      if (opts.meReleased && path === "**/api/me") await opts.meReleased;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...actor, preferences: account }) });
    });
  }
  const saves: unknown[] = [];
  await page.route("**/api/me/preferences", async (route) => {
    const body = route.request().postDataJSON();
    const n = saves.push(body) - 1;
    const status = opts.answerSave ? await opts.answerSave(n) : 200;
    if (status === 200 && account) Object.assign(account, body);
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(status === 200 ? { preferences: body } : { error: "unavailable" }) });
  });
  await page.goto("/");
  if (!opts.noWait) await expect(page.locator(".mvx-app-shell__brand")).toBeVisible({ timeout: 15_000 });
  return saves;
}

function deferred() {
  let release!: () => void;
  const promise = new Promise<void>((r) => (release = r));
  return { promise, release };
}

const html = (page: Page) => page.locator("html");
const bodyBg = (page: Page) => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
const stored = (page: Page, key = "mvx-theme") => page.evaluate((k) => localStorage.getItem(k), key);
const choose = async (page: Page, name: "Light" | "Dark" | "System") => {
  if (!(await page.locator(".mvx-user-menu__popover").isVisible())) await page.getByRole("button", { name: /^Account/ }).click();
  await page.getByRole("menuitemradio", { name }).click();
};

test("light by default; Dark switches the whole page, is saved to the account and cached for first paint", async ({ page }) => {
  const saves = await open(page);
  await expect(html(page)).toHaveAttribute("data-theme", "light");
  expect(await bodyBg(page)).toBe("rgb(247, 248, 251)");
  expect(saves).toEqual([]);

  await choose(page, "Dark");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await expect(page.getByRole("menuitemradio", { name: "Dark" })).toHaveAttribute("aria-checked", "true");
  expect(await bodyBg(page)).toBe("rgb(11, 17, 32)");
  await expect.poll(() => saves).toEqual([{ theme: "dark" }]);

  // Applied before first paint: with the app bundle blocked, only index.html's
  // inline script can have set it.
  await page.route("**/src/main.tsx", (route) => route.abort());
  await page.reload();
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
});

test("the account's choice follows the person to a browser that has none", async ({ page }) => {
  const saves = await open(page, { accountTheme: "dark" });
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  expect(await stored(page)).toBe("dark");
  expect(saves).toEqual([]); // nothing to save: it came from the account

  // …and is now this browser's copy, so the next load paints dark at once.
  await page.route("**/src/main.tsx", (route) => route.abort());
  await page.reload();
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
});

test("the account wins over what this browser last had", async ({ page }) => {
  // Chosen Dark here, then Light on another device.
  const saves = await open(page, { accountTheme: "light", stored: { "mvx-theme": "dark", "mvx-theme-account": USER_ID } });
  await expect(html(page)).toHaveAttribute("data-theme", "light");
  expect(await stored(page)).toBe("light");
  expect(saves).toEqual([]);
});

test("a choice this browser made before the theme lived on the account is kept and saved to it", async ({ page }) => {
  const saves = await open(page, { stored: { "mvx-theme": "dark" } });
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await expect.poll(() => saves).toEqual([{ theme: "dark" }]);
  expect(await stored(page, "mvx-theme-account")).toBe(USER_ID);
});

test("on a shared browser, the previous person's choice is not handed to the next", async ({ page }) => {
  const saves = await open(page, { stored: { "mvx-theme": "dark", "mvx-theme-account": "someone-else" } });
  await expect(html(page)).toHaveAttribute("data-theme", "light");
  expect(await stored(page)).toBeNull();
  expect(await stored(page, "mvx-theme-account")).toBe(USER_ID);
  expect(saves).toEqual([]);
});

test("System follows the operating system, live", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "dark" });
  const saves = await open(page);
  // Not chosen yet: an OS in dark mode does not flip anyone's console.
  await expect(html(page)).toHaveAttribute("data-theme", "light");

  await choose(page, "System");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expect(html(page)).toHaveAttribute("data-theme", "light");

  await choose(page, "Light");
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(html(page)).toHaveAttribute("data-theme", "light");
  await expect.poll(() => saves).toEqual([{ theme: "system" }, { theme: "light" }]);
});

test("quick choices are saved one at a time, in the order they were made", async ({ page }) => {
  const first = deferred();
  const saves = await open(page, { answerSave: async (n) => { if (n === 0) await first.promise; return 200; } });
  await page.getByRole("button", { name: /^Account/ }).click();
  await page.getByRole("menuitemradio", { name: "System" }).click();
  await page.getByRole("menuitemradio", { name: "Light" }).click();
  // The second waits for the first to be answered, so it cannot overtake it.
  await page.waitForTimeout(300);
  expect(saves).toEqual([{ theme: "system" }]);
  first.release();
  await expect.poll(() => saves).toEqual([{ theme: "system" }, { theme: "light" }]);
});

test("when the account's preferences cannot be read, this browser's copy is neither saved over it nor dropped", async ({ page }) => {
  const saves = await open(page, { preferencesUnknown: true, stored: { "mvx-theme": "dark", "mvx-theme-account": USER_ID } });
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await page.waitForTimeout(300);
  expect(saves).toEqual([]);
  expect(await stored(page)).toBe("dark");
});

test("a choice made before /api/me answers is kept, and the answer still arrives", async ({ page }) => {
  const me = deferred();
  const saves = await open(page, { accountTheme: "light", meReleased: me.promise });
  // The console is up (the auth layer knows the person) but /api/me is not.
  await choose(page, "Dark");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  me.release();
  // The late answer (light, from before the click) does not undo it…
  await page.waitForTimeout(300);
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await expect.poll(() => saves.at(-1)).toEqual({ theme: "dark" });
  // …and it is not lost: the menu shows who is signed in.
  await expect(page.locator(".mvx-user-menu__popover")).toContainText("signed.in@example.com");
});

test("a save that failed is sent again on the next load, instead of the account's older value undoing it", async ({ page }) => {
  const saves = await open(page, { accountTheme: "light", answerSave: (n) => (n === 0 ? 503 : 200) });
  await choose(page, "Dark");
  await expect.poll(() => saves).toEqual([{ theme: "dark" }]);
  expect(await stored(page, "mvx-theme-unsaved")).toBe("1");

  // /api/me still says light: the account never got it.
  await page.reload();
  await expect(page.locator(".mvx-app-shell__brand")).toBeVisible({ timeout: 15_000 });
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  await expect.poll(() => saves).toEqual([{ theme: "dark" }, { theme: "dark" }]);
  await expect.poll(() => stored(page, "mvx-theme-unsaved")).toBeNull();
});

test("a tenant brand colour gets its own dark-theme shades", async ({ page }) => {
  await page.route("**/api/branding", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ product_name: "Acme", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "#0f766e", configured: true, source: "tenant" }) }),
  );
  await open(page);
  const token = (name: string) => page.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
  await expect.poll(() => token("--color-brand-500")).toBe("#0f766e");
  const lightTint = await token("--color-brand-50");

  await choose(page, "Dark");
  await expect(html(page)).toHaveAttribute("data-theme", "dark");
  // Still the tenant's colour, but tints are dark and text shades lighter.
  expect(await token("--color-brand-500")).toBe("#0f766e");
  expect(await token("--color-brand-50")).not.toBe(lightTint);
  expect(await token("--color-brand-50")).toBe("#112531");
  expect(await token("--color-brand-600")).toBe("#4b9892");
});
