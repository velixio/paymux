import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

const visible = (page: Page) =>
  page.locator("section.react-content:not([hidden])");
const openTabs = (page: Page) =>
  page.getByRole("tablist", { name: "Open pages" }).getByRole("tab");
const pane = (page: Page) =>
  page.getByRole("complementary", { name: "Record preview" });

test("a payment opens in the side pane, survives reload and closes with Escape", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await page.goto("/dashboard/payments");
  const tabsBefore = await openTabs(page).count();

  await visible(page)
    .locator("tbody tr")
    .first()
    .getByRole("link")
    .first()
    .click();
  await expect(pane(page)).toBeVisible();
  // A readable address: just the id, no encoded path.
  await expect(page).toHaveURL(
    /\/dashboard\/payments\?peek=pay_[A-Za-z0-9_]+$/,
  );
  await expect(openTabs(page)).toHaveCount(tabsBefore); // same tab
  await expect(visible(page).locator("tbody tr").first()).toBeVisible(); // the list stays
  await expect(pane(page).getByRole("tab", { name: "Ledger" })).toBeVisible();
  await pane(page).getByRole("tab", { name: "Ledger" }).click();
  await expect(pane(page).getByText("charge")).toBeVisible();

  // j moves the selection; the pane follows.
  const before = page.url();
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(page).not.toHaveURL(before);
  await expect(pane(page)).toBeVisible();

  await page.reload();
  await expect(pane(page)).toBeVisible();

  await page.keyboard.press("Escape");
  await expect(pane(page)).toBeHidden();
  await expect(page).toHaveURL(/\/dashboard\/payments$/);
});

test("Open in tab moves the record to a tab of its own", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await page.goto("/dashboard/payments");
  const before = await openTabs(page).count();
  await visible(page)
    .locator("tbody tr")
    .first()
    .getByRole("link")
    .first()
    .click();
  await pane(page).getByRole("button", { name: "Open in tab" }).click();
  await expect(openTabs(page)).toHaveCount(before + 1);
  await expect(page).toHaveURL(/\/dashboard\/payments\/pay_[^?]+$/);
  await expect(pane(page)).toHaveCount(0);
});

test("below 1100px there is no pane: the link navigates", async ({ page }) => {
  await page.setViewportSize({ width: 1000, height: 800 });
  await login(page);
  await page.goto("/dashboard/payments");
  await visible(page)
    .locator("tbody tr")
    .first()
    .getByRole("link")
    .first()
    .click();
  await expect(page).toHaveURL(/\/dashboard\/payments\/pay_[^?]+$/);
  await expect(pane(page)).toHaveCount(0);
});
