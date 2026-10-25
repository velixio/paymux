import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

// The tokens the page is wearing right now.
const primary = (page: Page) =>
  page.evaluate(() =>
    getComputedStyle(document.documentElement)
      .getPropertyValue("--primary")
      .trim(),
  );

test("an admin makes a theme from Paddle, sets it as default; a user overrides it with Midnight; the dark toggle still works; a theme in use cannot be deleted", async ({
  page,
}) => {
  await login(page);

  // Create from Paddle with another accent.
  await page.goto("/dashboard/appearance/themes/new?from=paddle");
  await page.locator('input[name="name"]').fill("harbour");
  await page.locator('input[name="light_accent"]').fill("#b3261e");
  await page.getByRole("button", { name: "Create theme" }).click();
  const card = page.locator('[data-theme-card="harbour"]');
  await expect(card).toBeVisible();
  await expect(card).toHaveAttribute("data-kind", "custom");
  // Its preview is drawn from the seeds (light and dark).
  await expect(card.getByRole("img")).toHaveCount(2);

  // Set it as the organisation default: the dashboard wears it.
  await card.getByRole("button", { name: "Set as default" }).click();
  await expect(card).toHaveAttribute("data-default", "true");
  await page.goto("/dashboard/overview");
  await expect.poll(() => primary(page)).toBe("#b3261e");

  // Editing the theme you wear shows at once, by navigating inside the app (no reload).
  await page
    .getByRole("link", { name: "Appearance", exact: true })
    .first()
    .click();
  await page
    .locator('[data-theme-card="harbour"]')
    .getByRole("link", { name: "Edit" })
    .click();
  await page.locator('input[name="light_accent"]').fill("#0b5ed7");
  await page.getByRole("button", { name: "Save theme" }).click();
  await expect(page.locator('[data-theme-card="harbour"]')).toBeVisible();
  await expect.poll(() => primary(page)).toBe("#0b5ed7");

  // A user's own choice beats the default.
  await page.goto("/dashboard/account");
  await page.locator('select[name="theme"]').selectOption("midnight");
  await page.getByRole("button", { name: "Save theme" }).click();
  await page.goto("/dashboard/overview");
  await expect.poll(() => primary(page)).toBe("#c6f36b"); // Midnight is dark by default
  await expect(page.locator("html")).toHaveClass(/dark/);

  // The dark toggle still works with any theme.
  await page.getByRole("button", { name: /switch to light theme/i }).click();
  await expect(page.locator("html")).not.toHaveClass(/dark/);
  await expect.poll(() => primary(page)).toBe("#4d7c0f");
  await page.getByRole("button", { name: /switch to dark theme/i }).click();
  await expect(page.locator("html")).toHaveClass(/dark/);

  // The theme in use (as the default) cannot be deleted.
  await page.goto("/dashboard/appearance");
  const mine = page.locator('[data-theme-card="harbour"]');
  await mine.getByRole("button", { name: "Delete" }).click();
  await expect(page.getByRole("alert")).toContainText("in use");
  await expect(page.locator('[data-theme-card="harbour"]')).toBeVisible();
});

test("editing a theme previews it live and leaving restores the page", async ({
  page,
}) => {
  await login(page);
  await page.goto("/dashboard/appearance/themes/new?from=stripe");
  await page.locator('input[name="name"]').fill("inkwell");
  await page.getByRole("button", { name: "Create theme" }).click();
  await expect(page.locator('[data-theme-card="inkwell"]')).toBeVisible();

  await page.goto("/dashboard/appearance/themes/inkwell/edit");
  const preview = page.locator("style#paymux-theme-preview");
  await page.locator('input[name="light_accent"]').fill("#123456");
  await expect.poll(() => preview.textContent()).toContain("--primary:#123456");
  await expect(page.getByRole("table", { name: /contrast/i })).toBeVisible();
  // The whole dashboard wears it while editing.
  await expect.poll(() => primary(page)).toBe("#123456");

  // Leaving with unsaved edits asks first; staying keeps the preview.
  await page.getByRole("link", { name: "Back to Appearance" }).click();
  await expect(page.getByText("Discard unsaved changes?")).toBeVisible();
  await page.getByRole("button", { name: "Keep editing" }).click();
  await expect(preview).toHaveCount(1);

  // Discarding leaves, and the page gets its own colours back.
  await page.getByRole("link", { name: "Back to Appearance" }).click();
  await page.getByRole("button", { name: "Discard and leave" }).click();
  await expect(preview).toHaveCount(0);
  await expect.poll(() => primary(page)).not.toBe("#123456");
});
