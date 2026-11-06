import { expect, test } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

test("the payments list badge opens the risk modal", async ({ page }) => {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);

  await page.goto("/dashboard/payments");
  const row = page.getByRole("row").filter({ hasText: "19 Low" });
  const dialog = page.getByRole("dialog", { name: "Risk review" });
  await expect(dialog).toBeHidden();

  const badge = row.getByRole("button", { name: /19 Low/ });
  await badge.click();
  await expect(dialog).toBeVisible();
  await expect(page).toHaveURL(/\/dashboard\/payments$/);
  // The button looks like the same pill as the modal's, not a dashboard button.
  const pill = dialog.locator(".risk-score");
  for (const property of ["background-color", "font-size", "border-radius"]) {
    const read = (el: Element, name: string) =>
      getComputedStyle(el).getPropertyValue(name);
    expect(await badge.evaluate(read, property)).toBe(
      await pill.evaluate(read, property),
    );
  }
  await expect(dialog).toContainText("Testville, Sample State, US");
  await expect(dialog).toContainText("203.0.113.24");
  await expect(dialog).toContainText("Reported by Whop");
  // Readable in the app's theme: the same green as the Succeeded status pill.
  const color = (el: Element) => getComputedStyle(el).color;
  expect(await badge.evaluate(color)).toBe(
    await row.locator(".status").evaluate(color),
  );
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(dialog).toBeHidden();
});

test("the payment page shows the risk review in its panel, the score in its tile", async ({
  page,
}) => {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);

  await page.goto("/dashboard/payments");
  const row = page.getByRole("row").filter({ hasText: "19 Low" });
  await expect(row).toHaveCount(1);
  await row.getByRole("link").first().click();

  await expect(page.locator(".pt-tile").filter({ hasText: "Risk" })).toContainText("19 Low");
  const panel = page.locator(".pt-risk");
  await expect(panel).toContainText("Testville, Sample State, US");
  await expect(panel).toContainText("203.0.113.24");
  await expect(panel).toContainText("No match");
  await expect(panel).toContainText("Not verified");
  await expect(panel).toContainText("A1B2C3");
  await expect(panel).toContainText("Reported by Whop");
  await expect(panel).not.toContainText("19 Low");
});
