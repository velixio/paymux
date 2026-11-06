import { expect, test, type Page } from "./fixtures";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function openFailedPayment(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
  await page.goto("/dashboard/payments/pay_e2e_tries");
  await expect(page.locator(".payment-timeline")).toBeVisible();
}

test("one page with no inner tabs, tiles and all events", async ({ page }) => {
  await openFailedPayment(page);
  const root = page.locator(".payment-timeline");
  await expect(page.getByRole("tablist", { name: "Sections" })).toHaveCount(0);

  const tiles = root.locator(".pt-tile");
  await expect(tiles).toHaveCount(5);
  await expect(tiles.filter({ hasText: "Last failure" })).toContainText(
    "3D Secure failed",
  );
  await expect(tiles.filter({ hasText: "Tries" })).toContainText("2 failed");
  await expect(tiles.filter({ hasText: "Risk" })).toContainText("62 Medium");

  // Provider text and badges on the try.
  await expect(root).toContainText("Your card has insufficient funds");
  await expect(root).toContainText("S CUSTOMER");
  // The Risk panel shows ticks and crosses, and leaves the score to the tile.
  const risk = root.locator(".pt-risk");
  await expect(risk).toContainText("No match");
  await expect(risk).not.toContainText("62");

  // A try's own details open in place.
  await root.getByText("Provider IDs and logs").first().click();
  await expect(root.getByText("ch_e2e_declined")).toBeVisible();

  // Ledger rows join the list only when asked for.
  const ledger = root.locator(".pt-ledger").first();
  await expect(ledger).toBeHidden();
  await root.getByText("All events").click();
  await expect(ledger).toBeVisible();
});

test("a webhook lists its endpoints, and redelivers to all of them", async ({
  page,
}) => {
  await openFailedPayment(page);
  const root = page.locator(".payment-timeline");
  await expect(root.locator(".pt-tile").filter({ hasText: "Webhooks" })).toContainText("1 failing");
  const webhook = root.locator(".pt-hook");
  await expect(webhook).toContainText("api.example.com");
  await expect(webhook).toContainText("200");
  await expect(webhook).toContainText("500");
  await expect(webhook).toContainText("crm.example.com");
  await expect(webhook).toContainText("Not sent");
  await expect(webhook.getByRole("button", { name: "Retry" })).toHaveCount(1);

  await webhook.getByRole("button", { name: "Redeliver to all (3)" }).click();
  await page.getByRole("button", { name: "Confirm change" }).click();
  await expect(page.getByText("Sent to 3 endpoints.")).toBeVisible();
});

test("the refund pop-up opens and closes", async ({ page }) => {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
  await page.goto("/dashboard/payments");
  await page
    .getByRole("row")
    .filter({ hasText: "19 Low" })
    .getByRole("link")
    .first()
    .click();
  const dialog = page.getByRole("dialog", { name: "Refund" });
  await expect(dialog).toBeHidden();
  await page.getByRole("button", { name: "Refund", exact: true }).click();
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Create refund" })).toBeVisible();
  await dialog.getByRole("button", { name: "Close" }).click();
  await expect(dialog).toBeHidden();

  // The review step that guards a refund has to be reachable over the pop-up.
  await page.getByRole("button", { name: "Refund", exact: true }).click();
  await dialog.getByRole("button", { name: "Create refund" }).click();
  const cancel = page.getByRole("button", { name: "Cancel", exact: true });
  await cancel.click({ timeout: 5000 });
  await expect(cancel).toBeHidden();
});

test("a provider that sends little shows no false crosses", async ({ page }) => {
  await openFailedPayment(page);
  await page.goto("/dashboard/payments/pay_e2e_sparse");
  const root = page.locator(".payment-timeline");
  await expect(root.locator(".pt-tile").filter({ hasText: "Last failure" })).toContainText("No reason given");
  await expect(root.locator(".pt-tile").filter({ hasText: "Risk" })).toContainText("Not provided");
  await expect(root.locator(".pt-risk")).toHaveCount(0);

  await page.goto("/dashboard/payments/pay_e2e_untried");
  await expect(root).toContainText("The customer hasn't tried to pay yet.");
});

test("on a phone the side panels sit below the timeline, with no sideways scroll", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openFailedPayment(page);
  const timeline = await page.locator(".pt-main").boundingBox();
  const side = await page.locator(".pt-side").boundingBox();
  expect(side!.y).toBeGreaterThanOrEqual(timeline!.y + timeline!.height - 1);
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth - window.innerWidth,
  );
  expect(overflow).toBeLessThanOrEqual(0);
});
