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

test("the payment page risk row opens the modal", async ({
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

  const open = page.getByRole("button", { name: /19 Low.*Testville/ });
  await expect(open).toBeVisible();
  const dialog = page.getByRole("dialog", { name: "Risk review" });
  await expect(dialog).toBeHidden();

  await open.click();
  await expect(dialog).toBeVisible();
  // Centered: the app's CSS reset removes a dialog's default auto margin.
  const box = await dialog.boundingBox();
  const width = page.viewportSize()!.width;
  expect(Math.abs(box!.x + box!.width / 2 - width / 2)).toBeLessThan(2);
  await expect(dialog).toContainText("Testville, Sample State, US");
  await expect(dialog).toContainText("203.0.113.24");
  await expect(dialog).toContainText("No match");
  await expect(dialog).toContainText("Not verified");
  await expect(dialog).toContainText("A1B2C3");
  await expect(dialog).toContainText("Reported by Whop");
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(dialog).toBeHidden();
});
