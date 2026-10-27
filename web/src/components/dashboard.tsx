"use client";

import { useQueryClient } from "@tanstack/react-query";
import {
  Anchor,
  BookOpen,
  Bot,
  Boxes,
  ChartCandlestick,
  ChartNoAxesCombined,
  Compass,
  FileText,
  Globe2,
  Hammer,
  Landmark,
  Menu,
  Moon,
  RefreshCw,
  Sailboat,
  ScrollText,
  Ship,
  Sun,
  Trophy,
  X,
  type LucideIcon,
} from "lucide-react";
import { useNow } from "@/app/providers";
import { dataset, fleet, useDashboard } from "@/lib/data";
import { age, count } from "@/lib/format";
import { datasetObservation } from "@/lib/operations";
import type {
  Account,
  Catalog,
  Clock,
  CompanyProfile,
  DashboardData,
} from "@/lib/types";
import { useUI, type View } from "@/lib/ui-store";
import { ContractsView } from "./contracts";
import { DecisionPanel } from "./decisions";
import { DiagnosticsView } from "./diagnostics";
import { HoldingsView, TreasuryView } from "./economy";
import { ExchangeView } from "./exchange";
import { EngineWarnings } from "./engine-warnings";
import { FleetPanel } from "./fleet";
import { Journal } from "./journal";
import { OperationsOverview } from "./operations-overview";
import { Performance } from "./performance";
import { ShipyardView } from "./shipyard";
import { StandingsView } from "./standings";
import { EmptyState } from "./ui";
import { UnderwritingLedger } from "./underwriting";
import { VoyagesView } from "./voyages";
import { WorldView } from "./world";

const navigationGroups: {
  label: string;
  items: { id: View; label: string; icon: LucideIcon }[];
}[] = [
  {
    label: "Overview",
    items: [{ id: "chart", label: "Overview", icon: Compass }],
  },
  {
    label: "Fleet",
    items: [
      { id: "fleet", label: "Vessels", icon: Ship },
      { id: "voyages", label: "Voyages", icon: Sailboat },
    ],
  },
  {
    label: "Journal",
    items: [{ id: "journal", label: "Journal", icon: BookOpen }],
  },
  {
    label: "Money",
    items: [
      { id: "performance", label: "Performance", icon: ChartNoAxesCombined },
      { id: "treasury", label: "Treasury", icon: Landmark },
      { id: "holdings", label: "Warehouses", icon: Boxes },
    ],
  },
  {
    label: "Trade",
    items: [
      { id: "contracts", label: "Contracts", icon: FileText },
      { id: "exchange", label: "Exchange", icon: ChartCandlestick },
      { id: "shipyard", label: "Shipyards", icon: Hammer },
    ],
  },
  {
    label: "World",
    items: [
      { id: "world", label: "World", icon: Globe2 },
      { id: "standings", label: "Standings", icon: Trophy },
    ],
  },
  {
    label: "System",
    items: [
      { id: "decisions", label: "Decisions", icon: Bot },
      { id: "diagnostics", label: "Diagnostics", icon: ScrollText },
    ],
  },
];
const navigation = navigationGroups.flatMap((group) => group.items);

export function Dashboard() {
  const query = useDashboard();
  const client = useQueryClient();
  const accountID = useUI((state) => state.accountID);
  const view = useUI((state) => state.view);
  const theme = useUI((state) => state.theme);
  const setAccount = useUI((state) => state.setAccount);
  const setView = useUI((state) => state.setView);
  const toggleTheme = useUI((state) => state.toggleTheme);
  const navigationOpen = useUI((state) => state.navigationOpen);
  const setNavigationOpen = useUI((state) => state.setNavigationOpen);
  const accounts = query.data?.accounts ?? [];
  const scoped = accountID
    ? accounts.filter((account) => account.id === accountID)
    : accounts;
  const catalog = scoped
    .map((account) => dataset<Catalog>(account, "catalog"))
    .find(Boolean);
  const clock = dataset<Clock>(scoped[0], "clock");
  const activeView =
    navigation.find((item) => item.id === view) ?? navigation[0];
  const ViewIcon = activeView.icon;
  const activeGroup =
    navigationGroups.find((group) =>
      group.items.some((item) => item.id === view),
    ) ?? navigationGroups[0];
  const now = useNow();
  const observations = scoped.map((account) =>
    datasetObservation(account, "ships", now),
  );
  const knownTimes = observations
    .map((observation) => observation.timestamp)
    .filter((value): value is string => Boolean(value));
  const oldest =
    knownTimes.length === scoped.length && knownTimes.length
      ? knownTimes.reduce((left, right) =>
          Date.parse(left) < Date.parse(right) ? left : right,
        )
      : undefined;
  const freshness = observations.some((item) =>
    ["error", "incomplete", "stale"].includes(item.state),
  )
    ? "Fleet records incomplete or stale"
    : observations.some((item) =>
          ["unknown", "missing"].includes(item.state),
        ) || !observations.length
      ? "Fleet freshness unknown"
      : observations.some((item) => item.state === "delayed")
        ? "Fleet records delayed"
        : "Fleet records current";

  return (
    <div className="application-shell">
      <header className="topbar">
        <div className="brand">
          <Anchor size={23} strokeWidth={1.7} aria-hidden="true" />
          <span>Tradewinds</span>
        </div>
        <button
          className="icon-button navigation-toggle"
          aria-label={navigationOpen ? "Close navigation" : "Open navigation"}
          aria-controls="main-navigation"
          aria-expanded={navigationOpen}
          onClick={() => setNavigationOpen(!navigationOpen)}
        >
          {navigationOpen ? (
            <X size={20} strokeWidth={1.7} aria-hidden="true" />
          ) : (
            <Menu size={20} strokeWidth={1.7} aria-hidden="true" />
          )}
        </button>
        <label className="scope-control">
          <span className="sr-only">Account and company</span>
          <select
            value={accountID}
            onChange={(event) => setAccount(event.target.value)}
          >
            <option value="">All accounts and companies</option>
            {accounts.map((account) => (
              <option key={account.id} value={account.id}>
                {account.label} /{" "}
                {dataset<CompanyProfile>(account, "profile")?.company.name ??
                  "Company not recorded"}
              </option>
            ))}
            {accountID &&
              !accounts.some((account) => account.id === accountID) && (
                <option value={accountID}>Selected account unavailable</option>
              )}
          </select>
        </label>
        <div className="topbar-status" role="status">
          <span
            className={`connection-dot ${query.isError ? "is-offline" : query.isPending ? "is-pending" : ""}`}
            aria-hidden="true"
          />
          <span>
            {query.isError
              ? "Recording API unavailable"
              : query.isPending
                ? "Connecting"
                : "Recording API connected"}
          </span>
        </div>
        <div className="button-group">
          <button
            className="icon-button"
            aria-label="Refresh recorded data"
            disabled={query.isFetching}
            onClick={() => void client.invalidateQueries()}
          >
            <RefreshCw size={17} strokeWidth={1.7} aria-hidden="true" />
          </button>
          <button
            className="icon-button"
            aria-label={`Switch to ${theme === "light" ? "dark" : "light"} theme`}
            onClick={toggleTheme}
          >
            {theme === "light" ? (
              <Moon size={17} strokeWidth={1.7} aria-hidden="true" />
            ) : (
              <Sun size={17} strokeWidth={1.7} aria-hidden="true" />
            )}
          </button>
        </div>
      </header>
      <aside
        className={`navigation ${navigationOpen ? "is-open" : ""}`}
        id="main-navigation"
      >
        <nav aria-label="Main navigation">
          {navigationGroups.map((group) => {
            const Icon = group.items[0].icon;
            const active = group === activeGroup;
            return (
              <button
                key={group.label}
                className={`nav-item ${active ? "is-active" : ""}`}
                aria-current={active ? "page" : undefined}
                onClick={() => setView(active ? view : group.items[0].id)}
              >
                <Icon size={17} strokeWidth={1.7} aria-hidden="true" />
                <span>{group.label}</span>
              </button>
            );
          })}
        </nav>
        <div className="navigation-footer">
          <span>Read-only operations</span>
          <p>Recorded facts. Automated trade.</p>
        </div>
      </aside>
      <main id="main-content" className="workspace" data-view={view}>
        <div className="workspace-heading">
          <div>
            <h1 className="view-title">
              <ViewIcon size={22} strokeWidth={1.7} aria-hidden="true" />
              {activeView.label}
            </h1>
            {view !== "chart" && (
              <p>
                {scoped.length === 1
                  ? (dataset<CompanyProfile>(scoped[0], "profile")?.company
                      .name ?? scoped[0].label)
                  : `${count(scoped.length)} configured accounts`}
              </p>
            )}
          </div>
          <div className="observation-status">
            {view !== "chart" && (
              <span>
                {freshness}
                {oldest ? `, ${age(oldest, now)}` : ""}
              </span>
            )}
            {clock && (
              <span>
                Day {clock.calendar.day}, month {clock.calendar.month},{" "}
                {clock.calendar.year}
                {clock.paused && view !== "chart" ? " / Recorded paused" : ""}
              </span>
            )}
          </div>
        </div>
        {activeGroup.items.length > 1 && (
          <nav
            className="secondary-navigation"
            aria-label={`${activeGroup.label} views`}
          >
            {activeGroup.items.map(({ id, label }) => (
              <button
                type="button"
                key={id}
                aria-current={view === id ? "page" : undefined}
                onClick={() => setView(id)}
              >
                {label}
              </button>
            ))}
          </nav>
        )}
        {query.isError && (
          <div className="notice notice-danger" role="alert">
            <strong>Updates interrupted.</strong> {query.error.message} Last
            successful records remain visible. Reconnection is automatic.
          </div>
        )}
        <EngineWarnings
          accounts={scoped}
          accountsLoaded={query.data !== undefined}
          overview={view === "chart"}
        />
        {query.isPending ? (
          <div className="loading-state" role="status">
            Loading recorded fleet and company data…
          </div>
        ) : !query.data ? (
          <EmptyState
            title="Recording service unavailable"
            description="Start the Go service and check its database connection. This dashboard does not contact the game directly."
          />
        ) : scoped.length === 0 ? (
          <EmptyState
            title={
              accounts.length
                ? "Selected account is unavailable"
                : "No accounts recorded"
            }
            description={
              accounts.length
                ? "Choose another account or return to all accounts."
                : "Register an account with the Go command, then start the observer or executor."
            }
          />
        ) : (
          <>
            {view === "chart" && (
              <OperationsOverview accounts={scoped} dashboard={query.data} />
            )}
            {(view === "chart" || view === "fleet") && (
              <FleetPanel
                accounts={scoped}
                catalog={catalog}
                dashboard={query.data}
                compact={view === "chart"}
              />
            )}
            {scoped.map((account) => (
              <AccountWorkspace
                key={account.id}
                account={account}
                dashboard={query.data!}
                view={view}
                showIdentity={scoped.length > 1}
              />
            ))}
          </>
        )}
      </main>
    </div>
  );
}

function AccountWorkspace({
  account,
  dashboard,
  view,
  showIdentity,
}: {
  account: Account;
  dashboard: DashboardData;
  view: View;
  showIdentity: boolean;
}) {
  const profile = dataset<CompanyProfile>(account, "profile");
  let content;
  switch (view) {
    case "treasury":
      content = <TreasuryView account={account} />;
      break;
    case "holdings":
      content = <HoldingsView account={account} />;
      break;
    case "shipyard":
      content = <ShipyardView account={account} />;
      break;
    case "contracts":
      content = <ContractsView account={account} />;
      break;
    case "exchange":
      content = <ExchangeView account={account} />;
      break;
    case "world":
      content = <WorldView account={account} />;
      break;
    case "standings":
      content = <StandingsView account={account} />;
      break;
    case "voyages":
      content = <VoyagesView account={account} />;
      break;
    case "journal":
      content = <Journal key={account.id} account={account} />;
      break;
    case "performance":
      content = (
        <>
          <Performance key={account.id} accountID={account.id} />
          <UnderwritingLedger account={account} />
        </>
      );
      break;
    case "decisions":
      content = (
        <DecisionPanel
          account={account}
          decisions={dashboard.decisions.filter(
            (item) => item.account_id === account.id,
          )}
          ships={fleet(account)}
        />
      );
      break;
    case "diagnostics":
      content = <DiagnosticsView account={account} dashboard={dashboard} />;
      break;
    case "chart":
    case "fleet":
      return null;
  }
  return (
    <section
      className="account-workspace"
      aria-label={`${account.label} ${profile?.company.name ?? "company"}`}
    >
      {showIdentity && (
        <header className="account-section-heading">
          <h2 className="view-title">
            <Ship size={17} strokeWidth={1.7} aria-hidden="true" />
            {profile?.company.name ?? account.label}
          </h2>
          <span>{account.label}</span>
        </header>
      )}
      {content}
    </section>
  );
}
