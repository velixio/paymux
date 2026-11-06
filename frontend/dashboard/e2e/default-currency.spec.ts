import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

async function setDefaultCurrency(page: Page, value: string) {
  await page.goto("/dashboard/settings");
  await page.getByRole("tab", { name: "Reporting" }).click();
  const form = page.locator('form[action$="/settings/default_currency"]');
  await form.locator('select[name="value"]').selectOption(value);
  await form.getByRole("button", { name: "Save" }).click();
  // Settings changes go through the review dialog before anything is sent.
  const review = page.getByRole("dialog");
  await Promise.all([
    page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        r.url().endsWith("/settings/default_currency"),
    ),
    review.getByRole("button", { name: "Confirm change" }).click(),
  ]);
  // The stored value, not what was just picked in the list.
  await page.goto("/dashboard/settings");
  await expect(page.locator("#setting-default_currency .eff")).toHaveText(
    value,
  );
}

test("a default currency converts Overview revenue and the setting can be turned off", async ({
  page,
}) => {
  await login(page);

  await page.goto("/dashboard/fx");
  await page.getByLabel("Currency").fill("USD");
  await page.getByLabel("INR per unit").fill("80");
  await page.getByRole("button", { name: "Save rate" }).click();
  await expect(
    page.getByRole("cell", { name: "80", exact: true }),
  ).toBeVisible();

  await setDefaultCurrency(page, "USD");
  try {
    await page.goto("/dashboard/overview");
    const table = page.locator("#revenue-data");
    await expect(table).toHaveAttribute("data-currency", "USD");
    await expect(table).toHaveAttribute("data-converted", "true");
    await expect(table).toHaveAttribute("data-estimated", "true");
    await expect(
      page.getByText(/converted to USD at the stored daily rate/).first(),
    ).toBeVisible();

    if (process.env.PAYMUX_E2E_SCREENSHOT) {
      await page.screenshot({
        path: process.env.PAYMUX_E2E_SCREENSHOT,
        fullPage: true,
      });
    }

    // Picking one currency is still a native view.
    await page.goto("/dashboard/overview?currency=INR");
    await expect(page.locator("#revenue-data")).toHaveAttribute(
      "data-converted",
      "false",
    );
  } finally {
    await setDefaultCurrency(page, "off");
  }

  await page.goto("/dashboard/overview");
  await expect(page.locator("#revenue-data")).toHaveAttribute(
    "data-converted",
    "false",
  );
});

test("the market rate provider is chosen in Settings, under Reporting", async ({
  page,
}) => {
  await login(page);

  const choose = async (value: string) => {
    await page.goto("/dashboard/settings");
    await page.getByRole("tab", { name: "Reporting" }).click();
    const form = page.locator('form[action$="/settings/fx_provider"]');
    await form.locator('select[name="value"]').selectOption(value);
    await form.getByRole("button", { name: "Save" }).click();
    await Promise.all([
      page.waitForResponse(
        (r) =>
          r.request().method() === "POST" &&
          r.url().endsWith("/settings/fx_provider"),
      ),
      page
        .getByRole("dialog")
        .getByRole("button", { name: "Confirm change" })
        .click(),
    ]);
    await page.goto("/dashboard/settings");
    await expect(page.locator("#setting-fx_provider .eff")).toHaveText(value);
  };

  await choose("off");
  await choose("frankfurter");
});

test("the time zone is picked from a list in Settings, under Reporting", async ({
  page,
}) => {
  await login(page);

  const choose = async (value: string) => {
    await page.goto("/dashboard/settings");
    await page.getByRole("tab", { name: "Reporting" }).click();
    const form = page.locator('form[action$="/settings/time_zone"]');
    await form.locator('select[name="value"]').selectOption(value);
    await form.getByRole("button", { name: "Save" }).click();
    await Promise.all([
      page.waitForResponse(
        (r) =>
          r.request().method() === "POST" &&
          r.url().endsWith("/settings/time_zone"),
      ),
      page
        .getByRole("dialog")
        .getByRole("button", { name: "Confirm change" })
        .click(),
    ]);
    await page.goto("/dashboard/settings");
    await expect(page.locator("#setting-time_zone .eff")).toHaveText(value);
  };

  await choose("Asia/Tokyo");
  await choose("server");
});
