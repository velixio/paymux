import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

// Hidden tabs stay mounted, so scope every locator to the visible one.
const visible = (page: Page) =>
  page.locator("section.react-content:not([hidden])");
const tabs = (page: Page) => page.getByRole("tab");
const sidebar = (page: Page, label: string) =>
  page
    .getByRole("navigation", { name: "Workspace navigation" })
    .getByRole("link", { name: label, exact: true });

test("open payment in new tab with alt-click, switch tabs keeps filter, reload restores tabs", async ({
  page,
}) => {
  await login(page);
  await page.goto("/dashboard/overview");
  await expect(visible(page).locator("#revenue-data")).toBeAttached();
  const base = await tabs(page).count();

  // The pinned Overview tab opens Payments in a tab of its own.
  await sidebar(page, "Payments").click();
  await expect(tabs(page)).toHaveCount(base + 1);
  const search = visible(page).getByRole("searchbox", { name: "Search" });
  await search.fill("ORD-00");
  await search.press("Enter");
  await expect(visible(page).locator("tbody tr")).toHaveCount(9);

  // Alt-click a payment: it opens in a new tab and the filtered list stays put.
  await visible(page)
    .locator("tbody tr")
    .first()
    .getByRole("link")
    .first()
    .click({ modifiers: ["Alt"] });
  await expect(tabs(page)).toHaveCount(base + 2);
  await expect(page).toHaveURL(/\/dashboard\/payments\/pay_/);

  // Switching back to the list shows the same filter.
  await tabs(page).nth(base).click();
  await expect(
    visible(page).getByRole("searchbox", { name: "Search" }),
  ).toHaveValue("ORD-00");
  await expect(visible(page).locator("tbody tr")).toHaveCount(9);

  // A reload brings the same tabs back, with the same active one.
  await page.reload();
  await expect(tabs(page)).toHaveCount(base + 2);
  await expect(tabs(page).nth(base)).toHaveAttribute("aria-selected", "true");
  await expect(
    visible(page).getByRole("searchbox", { name: "Search" }),
  ).toHaveValue("ORD-00");
});

test("dirty form in hidden tab survives switching and blocks close", async ({
  page,
}) => {
  await login(page);
  await page.goto("/dashboard/overview");
  await sidebar(page, "Payment links").click();
  const description = visible(page).locator(
    'form[action="/dashboard/payment-links"] input[name="description"]',
  );
  await description.fill("Annual plan, unsaved");
  const linksTab = tabs(page).filter({ has: page.locator(".tab-dirty") });
  await expect(linksTab).toHaveCount(1);

  // Go to the pinned Overview tab and back: the edit is still there.
  await tabs(page).first().click();
  await expect(visible(page).locator("#revenue-data")).toBeAttached();
  await linksTab.click();
  await expect(
    visible(page).locator(
      'form[action="/dashboard/payment-links"] input[name="description"]',
    ),
  ).toHaveValue("Annual plan, unsaved");

  // Closing it asks first; keeping the edit leaves the tab open.
  await page.getByRole("button", { name: /^Close Payment links/ }).click();
  const dialog = page.getByRole("dialog", { name: "Discard unsaved changes?" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Keep editing" }).click();
  await expect(dialog).toBeHidden();
  await expect(linksTab).toHaveCount(1);

  // Discarding closes it.
  await page.getByRole("button", { name: /^Close Payment links/ }).click();
  await dialog.getByRole("button", { name: "Discard and close" }).click();
  await expect(tabs(page).filter({ hasText: "Payment links" })).toHaveCount(0);
});

test("test mode shows the top border and TEST badge", async ({ page }) => {
  await login(page);
  await page.goto("/dashboard/overview");
  const shell = page.locator(".app-shell");
  await expect(shell).toHaveAttribute("data-mode", "live");
  await expect(page.locator(".live-label")).toHaveText("LIVE");

  await page.getByRole("button", { name: "Account and mode" }).click();
  await page.getByRole("button", { name: /test/i }).click();
  await expect(shell).toHaveAttribute("data-mode", "test");
  await expect(page.locator(".test-label")).toHaveText("TEST");
  expect(
    await shell.evaluate((el) => getComputedStyle(el).borderTopWidth),
  ).toBe("3px");
  expect(
    await shell.evaluate((el) => getComputedStyle(el).borderTopStyle),
  ).toBe("solid");
});

test("phone width shows the open-tabs sheet", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  await page.goto("/dashboard/overview");
  await expect(page.getByRole("tablist")).toHaveCount(0); // the strip is replaced by one button
  const open = page.getByRole("button", { name: /^\d+ open$/ });
  await expect(open).toBeVisible();
  await open.click();
  const sheet = page.getByRole("dialog", { name: "Open pages" });
  await expect(sheet).toBeVisible();
  await expect(sheet.getByRole("listitem")).not.toHaveCount(0);
  await expect(sheet.getByRole("button", { name: "Overview" })).toBeVisible();
});

test("no request fires from a hidden tab for 60s", async ({ page }) => {
  await page.clock.install();
  await login(page);
  await page.goto("/dashboard/overview");
  await sidebar(page, "Payments").click(); // Overview is now hidden behind Payments
  await expect(page).toHaveURL(/\/dashboard\/payments/);
  await expect(visible(page).locator("tbody tr").first()).toBeVisible();

  const seen: string[] = [];
  page.on("request", (req) => {
    if (["document", "fetch", "xhr"].includes(req.resourceType()))
      seen.push(req.url());
  });
  await page.clock.fastForward(60_000);
  await page.clock.runFor(100);
  expect(seen.filter((u) => u.includes("/dashboard/overview"))).toEqual([]);
});
