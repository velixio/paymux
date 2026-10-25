// Every keyboard binding of the workspace, for the `?` sheet. Keep in step
// with the bindings in shell.tsx (tabs, command bar) and use-table-nav.ts.
export type Shortcut = {
  /** Alternative key combinations; each is a list of keys pressed together or, with `then`, in order. */
  combos: { keys: string[]; then?: boolean }[];
  label: string;
};
export const SHORTCUTS: { group: string; items: Shortcut[] }[] = [
  {
    group: "Tabs",
    items: [
      { combos: [{ keys: ["["] }], label: "Previous tab" },
      { combos: [{ keys: ["]"] }], label: "Next tab" },
      {
        combos: [{ keys: ["g", "1–9"], then: true }],
        label: "Go to tab 1 to 9",
      },
      { combos: [{ keys: ["g", "w"], then: true }], label: "Close tab" },
      {
        combos: [{ keys: ["g", "t"], then: true }],
        label: "Reopen closed tab",
      },
      {
        combos: [{ keys: ["Alt", "click"] }],
        label: "Open a link in a new tab",
      },
    ],
  },
  {
    group: "Command bar",
    items: [
      {
        combos: [
          { keys: ["Ctrl", "K"] },
          { keys: ["⌘", "K"] },
          { keys: ["/"] },
        ],
        label: "Open command bar",
      },
      {
        combos: [{ keys: ["Alt", "Enter"] }],
        label: "Open the result in a new tab",
      },
    ],
  },
  {
    group: "Tables",
    items: [
      { combos: [{ keys: ["j"] }, { keys: ["↓"] }], label: "Next row" },
      { combos: [{ keys: ["k"] }, { keys: ["↑"] }], label: "Previous row" },
      { combos: [{ keys: ["Home"] }], label: "First row" },
      { combos: [{ keys: ["End"] }], label: "Last row" },
      { combos: [{ keys: ["Enter"] }], label: "Open row" },
      { combos: [{ keys: ["Alt", "Enter"] }], label: "Open row in a new tab" },
    ],
  },
  {
    group: "Help",
    items: [{ combos: [{ keys: ["?"] }], label: "Show shortcuts" }],
  },
];
