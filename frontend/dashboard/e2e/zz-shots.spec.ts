import { test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

const DIR = "/tmp/claude-1000/-home-user-code-payments/53805d94-0b86-41c4-8796-718cb9efa3af/scratchpad/shots";
async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}
test("command", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await page.goto("/dashboard/overview");
  await page.locator(".revenue-chart svg").waitFor();
  await page.keyboard.press("Control+k");
  await page.getByRole("combobox", { name: /search pages/i }).fill("pay");
  await page.waitForTimeout(300);
  await page.screenshot({ path: `${DIR}/command-bar-v1.png` });
});
