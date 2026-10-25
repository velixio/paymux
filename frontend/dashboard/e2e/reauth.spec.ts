import { execFileSync } from "node:child_process";
import { readdirSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { OWNER_EMAIL, OWNER_PASSWORD } from "./global-setup";

async function login(page: Page) {
  await page.goto("/dashboard/login");
  await page.getByLabel("Email address").fill(OWNER_EMAIL);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: /sign in|log in|continue/i }).click();
  await page.waitForURL(/\/dashboard\/(overview)?$/);
}

// The hub runs on a throw-away data directory; the newest one is ours.
function hubDB(): string {
  const dirs = readdirSync(tmpdir())
    .filter((n) => n.startsWith("paymux-e2e-"))
    .map((n) => join(tmpdir(), n))
    .filter((p) => {
      try {
        return statSync(join(p, "paymux.db")).isFile();
      } catch {
        return false;
      }
    })
    .sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs);
  return join(dirs[0], "paymux.db");
}

// Makes every session look confirmed 16 hours ago (the limit is 12).
function ageSessions() {
  execFileSync("python3", [
    "-I",
    "-c",
    "import sqlite3,sys,time;c=sqlite3.connect(sys.argv[1],timeout=10);" +
      "c.execute('update sessions set authenticated_at=?',(int((time.time()-16*3600)*1000),));c.commit()",
    hubDB(),
  ]);
}

test("a write that needs a recent sign-in shows the confirm form, returns to the page, and then saves", async ({
  page,
}) => {
  await login(page);
  await page.goto("/dashboard/appearance");
  const paddle = page.locator('[data-theme-card="paddle"]');
  await expect(paddle).toBeVisible();
  ageSessions();

  await paddle.getByRole("button", { name: "Set as default" }).click();
  await expect(
    page.getByRole("heading", { name: "Confirm it is you" }),
  ).toBeVisible();
  await expect(page.getByText("Your change was not saved")).toBeVisible();

  // A reload of the confirmation page shows it too.
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Confirm it is you" }),
  ).toBeVisible();
  await expect(page.getByText("Your change was not saved")).toBeVisible();

  // A wrong password stays on the form with the reason.
  await page.locator('input[name="password"]').fill("not-the-password");
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page.locator('p[role="alert"]')).toBeVisible();
  await expect(page.locator('input[name="password"]')).toBeVisible();

  // The right one returns to the Appearance page, where the change saves.
  await page.locator('input[name="password"]').fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/dashboard\/appearance/);
  const again = page.locator('[data-theme-card="paddle"]');
  await again.getByRole("button", { name: "Set as default" }).click();
  await expect(again).toHaveAttribute("data-default", "true");
});
