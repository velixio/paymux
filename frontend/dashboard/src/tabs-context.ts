import { createContext, useContext } from "react";
import type { TabState } from "./tabs";

export type TabsApi = {
  state: TabState;
  openHref(href: string, how: "replace" | "new"): void;
  /** Opens href in a tab of its section (reusing a clean one), not here. */
  openInSection(href: string): void;
  /** Changes what a tab will show without switching to it or navigating. */
  retarget(id: string, href: string): void;
  activate(id: string): void;
  close(id: string): void;
  closeOthers(id: string): void;
  closeRight(id: string): void;
  duplicate(id: string): void;
  /** Marks a tab's data stale; the tab refetches now or when next shown. */
  reload(id: string): void;
  reopen(): void;
  pin(id: string, v: boolean): void;
  move(id: string, i: number): void;
  dirtyIds: ReadonlySet<string>;
};
export const TabsContext = createContext<TabsApi | null>(null);
export function useTabs(): TabsApi {
  const value = useContext(TabsContext);
  if (!value) throw Error("Missing workspace tabs");
  return value;
}
/** For links that also render outside the shell (sign-in pages). */
export const useOptionalTabs = (): TabsApi | null => useContext(TabsContext);
