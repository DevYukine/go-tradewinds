import { create } from "zustand";
import { persist } from "zustand/middleware";

export type View =
  | "chart"
  | "voyages"
  | "fleet"
  | "journal"
  | "performance"
  | "treasury"
  | "holdings"
  | "contracts"
  | "exchange"
  | "shipyard"
  | "world"
  | "standings"
  | "decisions"
  | "diagnostics";

interface UIState {
  accountID: string;
  view: View;
  theme: "light" | "dark";
  selectedShipID: string;
  selectedShipAccountID: string;
  selectedPortID: string;
  navigationOpen: boolean;
  setAccount: (id: string) => void;
  setView: (view: View) => void;
  toggleTheme: () => void;
  selectShip: (id: string, accountID?: string) => void;
  selectPort: (id: string) => void;
  setNavigationOpen: (open: boolean) => void;
}

export const useUI = create<UIState>()(
  persist(
    (set) => ({
      accountID: "",
      view: "chart",
      theme: "light",
      selectedShipID: "",
      selectedShipAccountID: "",
      selectedPortID: "",
      navigationOpen: false,
      setAccount: (id) =>
        set({
          accountID: id,
          selectedShipID: "",
          selectedShipAccountID: "",
          selectedPortID: "",
        }),
      setView: (view) => set({ view, navigationOpen: false }),
      toggleTheme: () =>
        set((state) => ({ theme: state.theme === "light" ? "dark" : "light" })),
      selectShip: (id, accountID) =>
        set((state) => ({
          selectedShipID: id,
          selectedShipAccountID: accountID ?? state.accountID,
        })),
      selectPort: (id) => set({ selectedPortID: id }),
      setNavigationOpen: (open) => set({ navigationOpen: open }),
    }),
    {
      name: "tradewinds-operations",
      skipHydration: true,
      version: 1,
      migrate: (persisted) => {
        const state = persisted as {
          accountID: string;
          theme: "light" | "dark";
          view: View | "overview";
        };
        return {
          ...state,
          view: state.view === "overview" ? "chart" : state.view,
        };
      },
      partialize: (state) => ({
        accountID: state.accountID,
        theme: state.theme,
        view: state.view,
      }),
    },
  ),
);
