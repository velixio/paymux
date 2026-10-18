import { describe, expect, it } from "vitest";
import {
  documentLink,
  guidePath,
  guides,
  headingSlug,
  headings,
  searchGuides,
} from "./catalog";

describe("public documentation catalog", () => {
  it("provides unique routes and sources for the whole library", () => {
    expect(guides.length).toBeGreaterThanOrEqual(20);
    expect(new Set(guides.map(guidePath)).size).toBe(guides.length);
    expect(new Set(guides.map((guide) => guide.source)).size).toBe(
      guides.length,
    );
    expect(guidePath(guides[0])).toBe("/docs");
    expect(guidePath(guides[1])).toBe("/docs/installation");
  });
  it("creates predictable anchors and skips fenced examples", () => {
    expect(headingSlug("Verify `webhook-id` & **events**")).toBe(
      "verify-webhook-id--events",
    );
    expect(
      headings(
        "# Guide\n## Step\n```sh\n# Not a heading\n```\n## Step\n### Detail\n",
      ),
    ).toEqual([
      { line: 1, level: 1, text: "Guide", id: "guide" },
      { line: 2, level: 2, text: "Step", id: "step" },
      { line: 6, level: 2, text: "Step", id: "step-1" },
      { line: 7, level: 3, text: "Detail", id: "detail" },
    ]);
    expect(headings("")).toEqual([]);
  });
  it("keeps local guide links in the site and code links in the repository", () => {
    expect(
      documentLink(
        "webhooks.md#retries-failure-and-replay",
        "docs/api-guide.md",
      ),
    ).toBe("/docs/webhooks#retries-failure-and-replay");
    expect(documentLink("docs/payments.md", "docs/README.md")).toBe(
      "/docs/payments",
    );
    expect(documentLink("glossary.md", "docs/README.md")).toBe(
      "/docs/glossary",
    );
    expect(documentLink("openapi.yaml", "docs/api-guide.md")).toBe(
      "https://github.com/velixio/paymux/blob/main/docs/openapi.yaml",
    );
    expect(
      documentLink("../examples/api-client/main.go", "docs/README.md"),
    ).toContain("/blob/main/examples/api-client/main.go");
    for (const href of [
      "",
      "#heading",
      "https://example.com",
      "http://localhost",
      "mailto:help@example.com",
      "//example.com/path",
    ])
      expect(documentLink(href, "docs/README.md")).toBe(href);
  });
  it("searches all words across title, summary, and content", () => {
    const content = { "docs/webhooks.md": "Verify the raw body and timestamp" };
    expect(
      searchGuides("raw timestamp", content).map((guide) => guide.slug),
    ).toEqual(["webhooks"]);
    expect(
      searchGuides("  RAW  timestamp  ", content).map((guide) => guide.slug),
    ).toEqual(["webhooks"]);
    expect(searchGuides("refund", {}).length).toBeGreaterThan(0);
    expect(searchGuides("not-a-real-topic", content)).toEqual([]);
    expect(searchGuides("   ", content)).toEqual([]);
  });
});
