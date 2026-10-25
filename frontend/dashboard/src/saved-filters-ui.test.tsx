import { afterEach, beforeEach, expect, test, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { handlers, mount, pageHtml, resetHost } from "./host.test.helpers";

beforeEach(resetHost);
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const KEY = "paymux-filters:u@example.com (admin):/dashboard/payments";
const form = (method = "get") =>
  `<form class="filters" method="${method}" action="/dashboard/payments"><input type="search" name="q" aria-label="Search payments"><select name="status" aria-label="Status"><option value="">Any</option><option value="succeeded">Succeeded</option></select><input type="hidden" name="csrf" value="tok"><button type="submit">Filter</button></form><p>Payments body</p>`;

async function payments(method = "get") {
  handlers["/dashboard/payments"] = () => pageHtml("Payments", form(method));
  return mount("/dashboard/payments");
}
const search = (q: string) =>
  screen.getByLabelText("Search payments") as HTMLInputElement;

async function save(name: string, q: string, status = "") {
  fireEvent.change(search(q), { target: { value: q } });
  if (status)
    fireEvent.change(screen.getByLabelText("Status"), {
      target: { value: status },
    });
  fireEvent.click(screen.getByRole("button", { name: "Save filter" }));
  fireEvent.change(await screen.findByLabelText("Filter name"), {
    target: { value: name },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
}

test("Save filter and the chips end the filter row, inside the form", async () => {
  await payments();
  const formEl = document.querySelector("form.filters") as HTMLElement;
  const button = within(formEl).getByRole("button", { name: "Save filter" });
  expect(formEl.lastElementChild!.contains(button)).toBe(true);
  // Saving never submits the filter form.
  const submit = vi.fn((e: Event) => e.preventDefault());
  formEl.addEventListener("submit", submit);
  await save("Alice succeeded", "alice", "succeeded");
  await screen.findByRole("button", { name: "Alice succeeded" });
  expect(submit).not.toHaveBeenCalled();
  expect(
    within(formEl).getByRole("group", { name: "Saved filters" }),
  ).toBeTruthy();
  expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual([
    { name: "Alice succeeded", query: "q=alice&status=succeeded" },
  ]);
  expect(screen.queryByLabelText("Filter name")).toBeNull();
});

test("a chip applies its filter to the current tab", async () => {
  localStorage.setItem(
    KEY,
    JSON.stringify([{ name: "Alice", query: "q=alice&status=succeeded" }]),
  );
  const router = await payments();
  fireEvent.click(screen.getByRole("button", { name: "Alice" }));
  await waitFor(() =>
    expect(router.state.location.searchStr).toBe("?q=alice&status=succeeded"),
  );
  expect(router.state.location.pathname).toBe("/dashboard/payments");
});

test("a chip can be removed", async () => {
  localStorage.setItem(
    KEY,
    JSON.stringify([{ name: "Alice", query: "q=alice" }]),
  );
  await payments();
  fireEvent.click(screen.getByRole("button", { name: "Remove filter Alice" }));
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Alice" })).toBeNull(),
  );
  expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual([]);
});

test("saving needs a name and some filter", async () => {
  await payments();
  fireEvent.click(screen.getByRole("button", { name: "Save filter" }));
  const save = await screen.findByRole("button", { name: "Save" });
  fireEvent.change(screen.getByLabelText("Filter name"), {
    target: { value: "Nothing" },
  });
  fireEvent.click(save);
  expect(localStorage.getItem(KEY)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(screen.queryByLabelText("Filter name")).toBeNull();
});

test("the chip row is not offered on POST forms", async () => {
  await payments("post");
  expect(screen.queryByRole("button", { name: "Save filter" })).toBeNull();
});

test("chips are listed in a labelled group", async () => {
  localStorage.setItem(
    KEY,
    JSON.stringify([
      { name: "A", query: "q=a" },
      { name: "B", query: "q=b" },
    ]),
  );
  await payments();
  const group = screen.getByRole("group", { name: "Saved filters" });
  expect(within(group).getAllByRole("button", { name: /^[AB]$/ })).toHaveLength(
    2,
  );
});
