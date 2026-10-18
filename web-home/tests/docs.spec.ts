import { expect, test } from "@playwright/test";
import catalog from "../docs-catalog.json" with { type: "json" };

test("every published guide renders with working anchors in both themes", async ({
  page,
  request,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  await page.emulateMedia({ colorScheme: "light" });
  for (const guide of catalog) {
    const path = guide.slug ? `/docs/${guide.slug}` : "/docs";
    const response = await page.goto(path);
    expect(response?.status()).toBe(200);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(
      page.locator('#docs-navigation [aria-current="page"]'),
    ).toHaveCount(1);
    await expect(page).toHaveTitle(
      new RegExp(
        `${guide.title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}.*Paymux documentation`,
      ),
    );
    const anchors = await page
      .locator(".docs-outline nav a")
      .evaluateAll((links) =>
        links.map((link) => (link as HTMLAnchorElement).hash),
      );
    for (const anchor of anchors)
      expect(
        await page
          .locator(`[id="${decodeURIComponent(anchor.slice(1))}"]`)
          .count(),
        `${path}${anchor}`,
      ).toBe(1);
    for (const theme of ["light", "dark"]) {
      if (theme === "dark")
        await page.getByRole("button", { name: "Use dark theme" }).click();
      for (const width of [1440, 390, 360]) {
        await page.setViewportSize({ width, height: 1000 });
        await expect
          .poll(
            () =>
              page.evaluate(
                () => document.documentElement.scrollWidth <= innerWidth,
              ),
            { message: `${path}, ${theme}, ${width}px` },
          )
          .toBe(true);
      }
      if (theme === "dark")
        await page.getByRole("button", { name: "Use light theme" }).click();
    }
  }
  const response = await request.get("/docs/not-a-guide");
  expect(response.status()).toBe(404);
  expect(errors).toEqual([]);
});

test("search, internal links, code copying, and mobile guide navigation", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/docs");
  const search = page.getByRole("searchbox", { name: "Search documentation" });
  await search.fill("HMAC-SHA256");
  await expect(
    page.getByRole("heading", { name: "Search the documentation" }),
  ).toBeVisible();
  await page
    .locator(".docs-results")
    .getByRole("link")
    .filter({ hasText: "Webhooks" })
    .click();
  await expect(page).toHaveURL(/\/docs\/webhooks$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Webhooks and event delivery",
  );
  await page.getByRole("button", { name: "Copy", exact: true }).first().click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toContain(
    '"type": "payment.succeeded"',
  );
  await search.fill("no-match-for-this-example");
  await expect(page.getByRole("status")).toContainText("0 guides");
  await page
    .getByRole("button", { name: "Clear documentation search" })
    .click();
  await page.setViewportSize({ width: 390, height: 900 });
  await page.getByRole("button", { name: "Browse all guides" }).click();
  await page
    .getByRole("navigation", { name: "Documentation guides" })
    .getByRole("link", { name: "Payments and routing", exact: true })
    .click();
  await expect(page).toHaveURL(/\/docs\/payments$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Payments and routing",
  );
  await expect(
    page.getByRole("button", { name: "Browse all guides" }),
  ).toHaveAttribute("aria-expanded", "false");
  await page
    .locator(".docs-article")
    .getByRole("link", { name: "troubleshooting", exact: true })
    .click();
  await expect(page).toHaveURL(/\/docs\/troubleshooting$/);
});
