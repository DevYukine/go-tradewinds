"use client";

import { useQueryClient } from "@tanstack/react-query";
import {
  Anchor,
  BookOpen,
  Bot,
  Boxes,
  ChartNoAxesCombined,
  ChartCandlestick,
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
import {
  combineMetrics,
  dataset,
  fleet,
  metadata,
  useDashboard,
  useHistory,
} from "@/lib/data";
import { age, count, money } from "@/lib/format";
import type {
  Account,
  Catalog,
  Clock,
  CompanyProfile,
  DashboardData,
  Metric,
  Vault,
} from "@/lib/types";
import { useUI, type View } from "@/lib/ui-store";
import { ContractsView } from "./contracts";
import { DecisionPanel } from "./decisions";
import { DiagnosticsView } from "./diagnostics";
import { HoldingsView, TreasuryView } from "./economy";
import { ExchangeView } from "./exchange";
import { FleetPanel } from "./fleet";
import { Journal } from "./journal";
import { Performance } from "./performance";
import { ShipyardView } from "./shipyard";
import { StandingsView } from "./standings";
import { DatasetStatus, EmptyState } from "./ui";
import { VoyagesView } from "./voyages";
import { WorldView } from "./world";

const navigationGroups: {
  label: string;
  items: { id: View; label: string; icon: LucideIcon }[];
}[] = [
  {
    label: "Operations",
    items: [
      { id: "chart", label: "Fleet chart", icon: Compass },
      { id: "voyages", label: "Voyages", icon: Sailboat },
      { id: "fleet", label: "Vessels", icon: Ship },
      { id: "journal", label: "Journal", icon: BookOpen },
    ],
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
  const now = useNow();
  const observations = scoped.map((account) => metadata(account, "ships"));
  const knownTimes = observations
    .map((meta) => meta.updated_at)
    .filter(
      (value): value is string =>
        Boolean(value) && Number.isFinite(Date.parse(value!)),
    );
  const oldest =
    knownTimes.length === scoped.length && knownTimes.length
      ? knownTimes.reduce((left, right) =>
          Date.parse(left) < Date.parse(right) ? left : right,
        )
      : undefined;
  const observationAge = oldest && now ? now - Date.parse(oldest) : undefined;
  const unavailable = observations.some(
    (meta) => meta.error || meta.complete === false || meta.stale,
  );
  const freshness = unavailable
    ? "Fleet records incomplete or stale"
    : observationAge === undefined
      ? "Freshness unknown"
      : observationAge > 120_000
        ? "Fleet records stale"
        : observationAge > 30_000
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
        <div className="topbar-status">
          <span
            className={`connection-dot ${query.isError ? "is-offline" : query.isPending ? "is-pending" : ""}`}
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
          {navigationGroups.map((group) => (
            <div className="navigation-group" key={group.label}>
              <p className="navigation-label">{group.label}</p>
              {group.items.map(({ id, label, icon: Icon }) => (
                <button
                  key={id}
                  className={`nav-item ${view === id ? "is-active" : ""}`}
                  aria-current={view === id ? "page" : undefined}
                  onClick={() => setView(id)}
                >
                  <Icon size={17} strokeWidth={1.7} aria-hidden="true" />
                  <span>{label}</span>
                </button>
              ))}
            </div>
          ))}
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
            <p>
              {scoped.length === 1
                ? (dataset<CompanyProfile>(scoped[0], "profile")?.company
                    .name ?? scoped[0].label)
                : `${count(scoped.length)} configured accounts`}
            </p>
          </div>
          <div className="observation-status">
            <span>
              {freshness}
              {oldest ? `, ${age(oldest, now)}` : ""}
            </span>
            {clock && (
              <span>
                Day {clock.calendar.day}, month {clock.calendar.month},{" "}
                {clock.calendar.year}
                {clock.paused ? " / Simulation paused" : ""}
              </span>
            )}
          </div>
        </div>
        {query.isError && (
          <div className="notice notice-danger" role="alert">
            <strong>Updates interrupted.</strong> {query.error.message} Last
            successful records remain visible. Reconnection is automatic.
          </div>
        )}
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
              <FleetSummary accounts={scoped} dashboard={query.data} />
            )}
            {(view === "chart" || view === "fleet") && (
              <FleetPanel
                accounts={scoped}
                catalog={catalog}
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

function FleetSummary({
  accounts,
  dashboard,
}: {
  accounts: Account[];
  dashboard: DashboardData;
}) {
  const vaultSets = accounts.map((account) =>
    dataset<{ vaults: Vault[] }>(account, "vaults"),
  );
  const cash = vaultSets.every(Boolean)
    ? vaultSets
        .flatMap((item) => item!.vaults)
        .filter((vault) => vault.account_type === "vault")
        .reduce((sum, vault) => sum + vault.balance_pence, 0)
    : undefined;
  const ships = accounts.flatMap(fleet);
  const observedFleet = accounts.every(
    (account) => dataset(account, "ships") !== undefined,
  );
  const accountIDs = new Set(accounts.map((account) => account.id));
  const settlements = (dashboard.settlements ?? []).filter((item) =>
    accountIDs.has(item.account_id),
  );
  const known = settlements.filter((item) => item.costs_known);
  const settledNet = known.length
    ? known.reduce(
        (sum, item) =>
          sum +
          item.proceeds_pence -
          item.acquisition_pence -
          item.fees_pence -
          item.provisions_pence -
          item.expenses_pence,
        0,
      )
    : undefined;
  return (
    <div className="fleet-summary" aria-label="Scoped company statistics">
      <div>
        <span>Recorded cash</span>
        <strong>{money(cash)}</strong>
      </div>
      <div>
        <span>Ships afloat</span>
        <strong>
          {observedFleet
            ? count(ships.filter((item) => item.ship.status !== "sunk").length)
            : "Not recorded"}
        </strong>
      </div>
      <div>
        <span>At sea</span>
        <strong>
          {observedFleet
            ? count(
                ships.filter((item) => item.ship.status === "sailing").length,
              )
            : "Not recorded"}
        </strong>
      </div>
      <div>
        <span>Settled net, loaded sample</span>
        <strong
          className={
            settledNet === undefined || settledNet === 0
              ? undefined
              : settledNet < 0
                ? "negative"
                : "positive"
          }
        >
          {money(settledNet)}
        </strong>
        <small>
          {known.length} known-cost settlements
          {settlements.length !== known.length
            ? ` / ${settlements.length - known.length} costs unknown`
            : ""}
        </small>
      </div>
      <details className="summary-sources">
        <summary>Recording sources</summary>
        {accounts.map((account) => (
          <div key={account.id}>
            <strong>{account.label}</strong>
            <DatasetStatus account={account} name="vaults" />
            <DatasetStatus account={account} name="ships" />
          </div>
        ))}
        <p>
          Cash excludes escrow. Settlement subtotal covers only returned
          records, not complete period profit. Recording freshness does not
          prove runner connectivity.
        </p>
      </details>
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
  const history = useHistory<Metric>(
    view === "performance" ? account.id : undefined,
    "metrics",
  );
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
        <Performance
          metrics={combineMetrics(dashboard.metrics, history.data, account.id)}
          error={history.error?.message}
          fetching={history.isFetching}
        />
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
