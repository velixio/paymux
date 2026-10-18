import { readFile, stat } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { guides, guidePath, headings } from "../frontend/home/src/docs/catalog";

const errors: string[] = [];
const seen = new Set<string>();
for (const guide of guides) {
  const route = guidePath(guide);
  if (seen.has(route)) errors.push(`Duplicate public route: ${route}`);
  seen.add(route);
  const body = await readFile(guide.source, "utf8");
  if (!body.startsWith("# "))
    errors.push(`${guide.source}: missing document title`);
  if (headings(body).filter((heading) => heading.level === 1).length !== 1)
    errors.push(`${guide.source}: expected one H1`);
  // Fenced examples are instructions, not Markdown navigation.
  const prose = body.replace(/^```[^\n]*\n[\s\S]*?^```\s*$/gm, "");
  for (const match of prose.matchAll(/\]\(([^\s)]+)\)/g)) {
    const href = match[1];
    if (/^(https?:|mailto:|\/\/)/.test(href)) continue;
    const [path, hash] = href.split("#");
    const target = path
      ? resolve(dirname(guide.source), path)
      : resolve(guide.source);
    try {
      const file = await stat(target);
      if (hash && target.endsWith(".md") && file.isFile()) {
        const outline = headings(await readFile(target, "utf8"));
        if (!outline.some((heading) => heading.id === hash))
          errors.push(`${guide.source}: missing anchor ${href}`);
      }
    } catch {
      errors.push(`${guide.source}: missing target ${href}`);
    }
  }
}
if (errors.length) {
  for (const error of errors) console.error(error);
  process.exitCode = 1;
} else
  console.log(
    `Checked ${guides.length} public guides: unique routes, titles, local links, and heading anchors.`,
  );
