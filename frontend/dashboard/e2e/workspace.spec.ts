import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

// Hidden tabs stay mounted (and repeat ids), so look only at the visible tab.
const visible = (page: Page) => page.locator("section.react-content:not([hidden])");

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

test("login, overview shows revenue table, switch to 12M shows monthly rows, CSV downloads", async ({ page }) => {
  await login(page);
  await page.goto("/dashboard/overview");

  const table = visible(page).locator("#revenue-data");
  await expect(table).toBeAttached();
  await expect(table).toHaveAttribute("data-currency", "INR");
  await expect(table).toHaveAttribute("data-bucket", "day"); // 30D is daily
  expect(await table.locator("tr[data-start]").count()).toBeGreaterThanOrEqual(30);

  await page.getByRole("link", { name: "12M", exact: true }).click();
  await expect(visible(page).locator("#revenue-data")).toHaveAttribute("data-bucket", "month");
  const rows = visible(page).locator("#revenue-data tr[data-start]");
  expect(await rows.count()).toBeGreaterThanOrEqual(12);
  // the seeded mid-month payments land in their own month, and USD is converted
  const gross = await rows.evaluateAll((trs) => trs.map((tr) => Number(tr.getAttribute("data-gross"))));
  expect(gross.filter((g) => g > 0).length).toBeGreaterThanOrEqual(11);
  await expect(visible(page).locator("#revenue-data")).toHaveAttribute("data-converted", "true");

  // The CSV link carries the current query (12M); fetch it with the session.
  const href = await visible(page).getByRole("link", { name: /download csv/i }).getAttribute("href");
  expect(href).toContain("range=12m");
  const res = await page.request.get(href!);
  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toContain("text/csv");
  expect(res.headers()["content-disposition"]).toContain("revenue-12m-INR.csv");
  const lines = (await res.text()).trim().split("\n");
  expect(lines[0]).toBe("start,label,currency,gross,refunds,net");
  expect(lines.length).toBeGreaterThanOrEqual(13);
});
