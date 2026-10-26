import { create } from "zustand";
import { persist } from "zustand/middleware";
import {
  DEFAULT_MONEY_PREFERENCES,
  restoreMoneyPreferences,
  type MoneyPreferences,
} from "./money-history";

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
  moneyHistory: MoneyPreferences;
  setMoneyHistory: (preferences: Partial<MoneyPreferences>) => void;
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
      moneyHistory: DEFAULT_MONEY_PREFERENCES,
      setMoneyHistory: (preferences) =>
        set((state) => ({
          moneyHistory: { ...state.moneyHistory, ...preferences },
        })),
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
      merge: (persisted, current) => {
        const state =
          persisted !== null && typeof persisted === "object"
            ? (persisted as Partial<UIState>)
            : {};
        return {
          ...current,
          ...state,
          moneyHistory: restoreMoneyPreferences(state.moneyHistory),
        };
      },
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
        moneyHistory: state.moneyHistory,
      }),
    },
  ),
);
