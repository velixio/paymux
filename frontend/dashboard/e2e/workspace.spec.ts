import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

// The workspace tabs only: record pages also have inner section tabs.
const openTabs = (page: Page) =>
  page.getByRole("tablist", { name: "Open pages" }).getByRole("tab");

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

test("login, overview shows revenue table, switch to 12M shows monthly rows, CSV downloads", async ({
  page,
}) => {
  await login(page);
  await page.goto("/dashboard/overview");

  const table = page.locator("#revenue-data");
  await expect(table).toBeAttached();
  await expect(table).toHaveAttribute("data-currency", "INR");
  await expect(table).toHaveAttribute("data-bucket", "day"); // 30D is daily
  expect(await table.locator("tr[data-start]").count()).toBeGreaterThanOrEqual(
    30,
  );

  await page.getByRole("link", { name: "12M", exact: true }).click();
  await expect(page.locator("#revenue-data")).toHaveAttribute(
    "data-bucket",
    "month",
  );
  const rows = page.locator("#revenue-data tr[data-start]");
  expect(await rows.count()).toBeGreaterThanOrEqual(12);
  // the seeded mid-month payments land in their own month, and USD is converted
  const gross = await rows.evaluateAll((trs) =>
    trs.map((tr) => Number(tr.getAttribute("data-gross"))),
  );
  expect(gross.filter((g) => g > 0).length).toBeGreaterThanOrEqual(11);
  await expect(page.locator("#revenue-data")).toHaveAttribute(
    "data-converted",
    "true",
  );

  // The CSV link carries the current query (12M); fetch it with the session.
  const href = await page
    .getByRole("link", { name: /download csv/i })
    .getAttribute("href");
  expect(href).toContain("range=12m");
  const res = await page.request.get(href!);
  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toContain("text/csv");
  expect(res.headers()["content-disposition"]).toContain("revenue-12m-INR.csv");
  const lines = (await res.text()).trim().split("\n");
  expect(lines[0]).toBe("start,label,currency,gross,refunds,net");
  expect(lines.length).toBeGreaterThanOrEqual(13);
});

// Only the visible tab's content counts: hidden tabs stay mounted.
const visible = (page: Page) =>
  page.locator("section.react-content:not([hidden])");

test("overview shows chart", async ({ page }) => {
  await login(page);
  await page.goto("/dashboard/overview");
  const chart = visible(page).locator(".revenue-chart");
  await expect(chart.locator("svg")).toBeVisible();
  const rowCount = await visible(page)
    .locator("#revenue-data tr[data-start]")
    .count();
  await expect(chart.locator("[data-bar]")).toHaveCount(rowCount);
  // The table is behind the toggle but still in the page.
  await expect(visible(page).locator("#revenue-data")).toBeHidden();
  await visible(page).getByRole("button", { name: "View as table" }).click();
  await expect(visible(page).locator("#revenue-data")).toBeVisible();
});

test("switch to 12M shows monthly bars", async ({ page }) => {
  await login(page);
  await page.goto("/dashboard/overview");
  await visible(page).getByRole("link", { name: "12M", exact: true }).click();
  await expect(visible(page).locator("#revenue-data")).toHaveAttribute(
    "data-bucket",
    "month",
  );
  const bars = visible(page).locator(".revenue-chart [data-bar]");
  expect(await bars.count()).toBeGreaterThanOrEqual(12);
  await expect(bars.first()).toHaveAttribute("aria-label", /net/i);
  // The pinned Overview tab was updated in place, not duplicated.
  await expect(page.getByRole("tab")).toHaveCount(1);
  await bars.nth((await bars.count()) - 1).focus();
  await expect(visible(page).getByRole("tooltip")).toContainText("Gross");
});

test("ctrl+k finds a payment id and opens it", async ({ page }) => {
  await login(page);
  await page.goto("/dashboard/overview");
  await expect(openTabs(page)).toHaveCount(1);
  await page.keyboard.press("Control+k");
  const box = page.getByRole("combobox", { name: /search pages/i });
  await expect(box).toBeFocused();
  await box.fill("pay_e2e_033");
  await expect(page.getByRole("option").first()).toContainText(
    "Open payment pay_e2e_033",
  );
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/dashboard\/payments\/pay_e2e_033$/);
  await expect(visible(page).getByText("pay_e2e_033").first()).toBeVisible();
  // The pinned Overview stays; the payment opened beside it.
  await expect(openTabs(page)).toHaveCount(2);
});

test("reload shows no classic page; it appears only when the app script fails", async ({
  page,
}) => {
  await login(page);
  // Normal load: the server page is invisible until React replaces it.
  await page.goto("/dashboard/overview");
  await expect(page.locator(".revenue-chart")).toBeVisible();
  await expect(page.locator("#paymux-server-page")).toHaveCount(0);

  // App script blocked: the server page is hidden at first, then shown.
  await page.route("**/assets/dashboard/main.js", (r) => r.abort());
  await page.goto("/dashboard/overview");
  const server = page.locator("#paymux-server-page");
  await expect(server).toBeHidden();
  await expect(server).toBeVisible({ timeout: 4000 });
  await expect(server.getByRole("heading", { name: /Overview/ })).toBeVisible();
});

// The dashboard fetches each page and parses it before rendering it. A <style>
// with the fetched page's nonce, left in that parsed copy, made the browser
// report a Content Security Policy violation on every navigation.
test("navigating the workspace reports no Content Security Policy violation", async ({
  page,
}) => {
  const violations: string[] = [];
  page.on("console", (m) => {
    if (/Content Security Policy/i.test(m.text())) violations.push(m.text());
  });
  await login(page);
  for (const name of ["Payments", "Settings", "Overview"]) {
    const link = page.getByRole("link", { name, exact: true }).first();
    if (await link.count()) {
      await link.click();
      await page.waitForLoadState("networkidle");
    }
  }
  expect(violations).toEqual([]);
});
