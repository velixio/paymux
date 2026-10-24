import { useState, type RefObject } from "react";
import { useRouter } from "@tanstack/react-router";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useRuntime } from "./runtime";
import { add, filterQuery, list, remove } from "./saved-filters";
import { useOptionalTabs } from "./tabs-context";

const storage = (): Storage | null => {
  try {
    return localStorage;
  } catch {
    return null;
  }
};

// Chips of the user's saved filters for this page, and "Save filter", above a
// GET filter form. A chip applies its query to the current tab.
export function SavedFiltersBar({
  form,
}: {
  form: RefObject<HTMLFormElement | null>;
}) {
  const { page } = useRuntime();
  const tabs = useOptionalTabs();
  const router = useRouter();
  const path = new URL(page.href, location.origin).pathname;
  const [filters, setFilters] = useState(() =>
    list(storage(), page.user, path),
  );
  const [naming, setNaming] = useState(false);
  const [name, setName] = useState("");

  function apply(query: string) {
    const href = query ? `${path}?${query}` : path;
    if (tabs) tabs.openHref(href, "replace");
    else
      void router.navigate({
        to: path,
        search: Object.fromEntries(new URLSearchParams(query)),
      });
  }
  function save() {
    const query = form.current ? filterQuery(new FormData(form.current)) : "";
    if (!query || !name.trim()) return;
    add(storage(), page.user, path, { name, query });
    setFilters(list(storage(), page.user, path));
    setNaming(false);
    setName("");
  }

  return (
    <div className="saved-filters">
      {filters.length > 0 && (
        <div role="group" aria-label="Saved filters" className="saved-chips">
          {filters.map((f) => (
            <span className="saved-chip" key={f.name}>
              <button type="button" onClick={() => apply(f.query)}>
                {f.name}
              </button>
              <button
                type="button"
                className="saved-chip-remove"
                aria-label={`Remove filter ${f.name}`}
                onClick={() => {
                  remove(storage(), page.user, path, f.name);
                  setFilters(list(storage(), page.user, path));
                }}
              >
                <X size={12} />
              </button>
            </span>
          ))}
        </div>
      )}
      {naming ? (
        <span className="saved-name">
          <input
            aria-label="Filter name"
            value={name}
            maxLength={40}
            autoFocus
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                save();
              }
              if (e.key === "Escape") setNaming(false);
            }}
          />
          <Button size="sm" disabled={!name.trim()} onClick={save}>
            Save
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setNaming(false)}>
            Cancel
          </Button>
        </span>
      ) : (
        <Button
          variant="ghost"
          size="sm"
          className="save-filter"
          onClick={() => setNaming(true)}
        >
          Save filter
        </Button>
      )}
    </div>
  );
}
