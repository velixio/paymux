// Per-user, per-page saved filters of list pages (localStorage; every access
// is guarded because storage may be missing, full or blocked).
export type SavedFilter = { name: string; query: string };

const MAX_PER_PATH = 12;
const key = (user: string, path: string) => `paymux-filters:${user}:${path}`;

const valid = (x: unknown): x is SavedFilter =>
  typeof x === "object" &&
  x !== null &&
  typeof (x as SavedFilter).name === "string" &&
  typeof (x as SavedFilter).query === "string";

export function list(
  storage: Pick<Storage, "getItem"> | null,
  user: string,
  path: string,
): SavedFilter[] {
  try {
    const raw = storage?.getItem(key(user, path));
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed)
      ? parsed.filter(valid).map(({ name, query }) => ({ name, query }))
      : [];
  } catch {
    return [];
  }
}

function save(
  storage: Pick<Storage, "setItem">,
  user: string,
  path: string,
  filters: SavedFilter[],
): void {
  storage.setItem(key(user, path), JSON.stringify(filters));
}

// Same name replaces in place; a new name goes last and the oldest falls off
// beyond 12.
export function add(
  storage: Pick<Storage, "getItem" | "setItem"> | null,
  user: string,
  path: string,
  f: SavedFilter,
): void {
  try {
    const name = f.name.trim();
    if (!storage || !name) return;
    const cur = list(storage, user, path);
    const entry = { name, query: f.query };
    const at = cur.findIndex((x) => x.name === name);
    if (at >= 0) cur[at] = entry;
    else cur.push(entry);
    save(storage, user, path, cur.slice(-MAX_PER_PATH));
  } catch {
    /* storage unavailable: saved filters are a convenience */
  }
}

export function remove(
  storage: Pick<Storage, "getItem" | "setItem"> | null,
  user: string,
  path: string,
  name: string,
): void {
  try {
    if (!storage) return;
    save(
      storage,
      user,
      path,
      list(storage, user, path).filter((x) => x.name !== name),
    );
  } catch {
    /* see add */
  }
}

// The query string of a filter form: drops csrf, empty values and files.
export function filterQuery(
  entries: Iterable<[string, FormDataEntryValue]>,
): string {
  const params = new URLSearchParams();
  for (const [k, v] of entries) {
    if (k === "csrf" || typeof v !== "string" || v === "") continue;
    params.append(k, v);
  }
  return params.toString();
}
