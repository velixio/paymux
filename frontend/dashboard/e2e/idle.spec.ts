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
const sidebar = (page: Page, label: string) =>
  page
    .getByRole("navigation", { name: "Workspace navigation" })
    .getByRole("link", { name: label, exact: true });

// Idle budget: Overview with four more tabs open, left alone for a minute,
// makes no requests and runs no long tasks (nothing polls, nothing re-renders).
test("idle workspace with five tabs makes no requests and no long tasks for 60s", async ({
  page,
}) => {
  await page.clock.install();
  await login(page);
  await page.goto("/dashboard/overview");
  await expect(visible(page).locator("#revenue-data")).toBeAttached();

  for (const label of [
    "Payments",
    "Payment links",
    "Subscriptions",
    "Invoices",
  ]) {
    const before = await page.getByRole("tab").count();
    await sidebar(page, label).click();
    await expect(page.getByRole("tab")).toHaveCount(before + 1);
    await expect(visible(page).locator("table, form").first()).toBeVisible();
    // back to the pinned Overview, so the tab just opened is hidden
    await page.getByRole("tab").first().click();
    await expect(visible(page).locator("#revenue-data")).toBeAttached();
  }
  await expect(page.getByRole("tab")).toHaveCount(5);

  await page.evaluate(() => {
    const w = window as unknown as { __long: number[] };
    w.__long = [];
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__long.push(e.duration);
    }).observe({ entryTypes: ["longtask"] });
  });
  const requests: string[] = [];
  page.on("request", (req) => {
    if (!req.url().startsWith("data:")) requests.push(req.url());
  });

  for (let i = 0; i < 12; i++) await page.clock.runFor(5_000);
  await page.waitForTimeout(250); // let any queued entries and requests surface

  expect(requests).toEqual([]);
  expect(
    await page.evaluate(
      () => (window as unknown as { __long: number[] }).__long,
    ),
  ).toEqual([]);
});
