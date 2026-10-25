// Every keyboard binding of the workspace, for the `?` sheet. The tab keys
// are defined here once and shell.tsx binds them from TAB_KEYS, so the sheet
// cannot drift from them; keep the rest in step with shell.tsx (command bar)
// and use-table-nav.ts.

export type TabAction = "previous" | "next" | "go" | "close" | "reopen";
/** Tab key bindings as KeyboardEvent.code values. `digits`: followed by 1–9. */
export const TAB_KEYS: {
  action: TabAction;
  keys: string[];
  digits?: boolean;
  label: string;
}[] = [
  { action: "previous", keys: ["BracketLeft"], label: "Previous tab" },
  { action: "next", keys: ["BracketRight"], label: "Next tab" },
  { action: "go", keys: ["KeyG"], digits: true, label: "Go to tab 1 to 9" },
  { action: "close", keys: ["KeyG", "KeyW"], label: "Close tab" },
  { action: "reopen", keys: ["KeyG", "KeyT"], label: "Reopen closed tab" },
];

const NAMED: Record<string, string> = {
  BracketLeft: "[",
  BracketRight: "]",
  Slash: "/",
};
/** The key a person presses for a KeyboardEvent.code. */
export function keyLabel(code: string): string {
  if (NAMED[code]) return NAMED[code];
  const m = /^(?:Key|Digit)(.)$/.exec(code);
  return m ? m[1].toLowerCase() : code;
}

export type Shortcut = {
  /** Alternative key combinations; each is a list of keys pressed together or, with `then`, in order. */
  combos: { keys: string[]; then?: boolean }[];
  label: string;
};
export const SHORTCUTS: { group: string; items: Shortcut[] }[] = [
  {
    group: "Tabs",
    items: [
      ...TAB_KEYS.map((b) => {
        const keys = b.keys.map(keyLabel).concat(b.digits ? ["1–9"] : []);
        return {
          combos: [keys.length > 1 ? { keys, then: true } : { keys }],
          label: b.label,
        };
      }),
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
