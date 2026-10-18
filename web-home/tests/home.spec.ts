import { expect, test } from "@playwright/test";

test("payment outcomes keep accounting and delivery status separate", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(m.text());
  });
  await page.goto("/");
  await page.locator("#demo-provider").selectOption("Stripe");
  await page
    .getByRole("button", { name: "Run sample payment", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Processing sample…" }),
  ).toBeDisabled();
  await expect(page.getByRole("status")).toHaveText(
    "Captured, recorded, delivered.",
  );
  await expect(
    page.getByText("Stripe · illustrative routing decision"),
  ).toBeVisible();
  await page.getByRole("tab", { name: "Ledger", exact: true }).click();
  await expect(page.getByText("Dr ₹499.00")).toBeVisible();
  await expect(page.getByText("Cr ₹499.00")).toBeVisible();

  await page.locator("#demo-scenario").selectOption("declined");
  await page
    .getByRole("button", { name: "Run sample payment", exact: true })
    .click();
  await expect(page.getByRole("status")).toHaveText(
    "Declined. No capture recorded.",
  );
  await page.getByRole("tab", { name: "Ledger", exact: true }).click();
  await expect(
    page.getByText("No capture entries", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Dr ₹499.00")).toHaveCount(0);
  await page.getByRole("tab", { name: "Webhook", exact: true }).click();
  await expect(page.locator(".event-payload")).toContainText("payment.failed");

  await page.locator("#demo-scenario").selectOption("retry");
  await page
    .getByRole("button", { name: "Run sample payment", exact: true })
    .click();
  await expect(page.getByRole("status")).toHaveText(
    "Captured. Delivery needs a retry.",
  );
  await page.getByRole("tab", { name: "Ledger", exact: true }).click();
  await expect(page.getByText("Balanced capture entries")).toBeVisible();
  await page.getByRole("tab", { name: "Webhook", exact: true }).click();
  await page.getByRole("button", { name: "Retry sample delivery" }).click();
  await expect(page.getByRole("status")).toHaveText(
    "Captured, recorded, delivered.",
  );
  await expect(page.getByText("Your app has the event.")).toBeVisible();

  // A reset must cancel pending timers rather than update the next demo later.
  await page
    .getByRole("button", { name: "Run sample payment", exact: true })
    .click();
  await page.getByRole("button", { name: "Reset payment demo" }).click();
  await expect(page.getByRole("status")).toHaveText("Ready when you are");
  await page.waitForTimeout(1900);
  await expect(page.getByRole("status")).toHaveText("Ready when you are");
  expect(errors).toEqual([]);
});

test("both themes fit desktop and mobile and persist across routes", async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
  await page.goto("/");
  for (const theme of ["light", "dark"]) {
    if (theme === "dark")
      await page.getByRole("button", { name: "Use dark theme" }).click();
    for (const width of [1440, 1024, 768, 390, 360]) {
      await page.setViewportSize({ width, height: 1000 });
      await expect
        .poll(
          () =>
            page.evaluate(
              () => document.documentElement.scrollWidth <= innerWidth,
            ),
          { message: `${theme} theme at ${width}px must fit the viewport` },
        )
        .toBe(true);
    }
  }
  await page.reload();
  await expect(page.locator("html")).toHaveClass(/dark/);
  await expect(page.locator('meta[name="theme-color"]')).toHaveAttribute(
    "content",
    "#101c20",
  );
  for (const path of ["/docs", "/integrations", "/self-hosting"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.locator("html")).toHaveClass(/dark/);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
  await page.getByRole("button", { name: "Open navigation" }).click();
  await page
    .getByRole("navigation", { name: "Mobile navigation" })
    .getByRole("link", { name: "Documentation" })
    .click();
  await expect(page).toHaveURL(/\/docs$/);
  await page.getByRole("button", { name: "Use light theme" }).click();
  await expect(page.locator("html")).not.toHaveClass(/dark/);
});

test("system dark preference and keyboard-operated payment tabs", async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto("/");
  await expect(page.locator("html")).toHaveClass(/dark/);
  const events = page.getByRole("tab", { name: "Event timeline" });
  await events.focus();
  await page.keyboard.press("ArrowRight");
  await expect(
    page.getByRole("tab", { name: "Ledger", exact: true }),
  ).toBeFocused();
  await expect(page.getByText("No ledger entries yet")).toBeVisible();
  await page.keyboard.press("ArrowRight");
  await expect(
    page.getByRole("tab", { name: "Webhook", exact: true }),
  ).toBeFocused();
  await expect(page.getByText("No delivery attempt yet")).toBeVisible();
});
