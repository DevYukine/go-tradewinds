"use client";

import {
  AlertTriangle,
  CheckCircle2,
  Clock3,
  Database,
  KeyRound,
  Radio,
  ScrollText,
  Terminal,
  UserRound,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useNow } from "@/app/providers";
import {
  dataset,
  fleet,
  number,
  record,
  rows,
  text,
  useHistory,
} from "@/lib/data";
import { count, dateTime, humanize } from "@/lib/format";
import type {
  Account,
  CompanyProfile,
  DashboardData,
  DatasetMeta,
  JsonRecord,
} from "@/lib/types";
import {
  Badge,
  Section,
  DataTable,
  Dataset,
  DatasetStatus,
  SearchField,
  StructuredDetails,
} from "./ui";
import styles from "./operations.module.css";
import viewStyles from "./world-views.module.css";

type DiagnosticKind =
  "events" | "requests" | "decisions" | "datasets" | "identity";

const KINDS = [
  { id: "events", label: "Events", icon: Radio },
  { id: "requests", label: "API requests", icon: Terminal },
  { id: "decisions", label: "Decisions", icon: ScrollText },
  { id: "datasets", label: "Dataset health", icon: Database },
  { id: "identity", label: "Account identity", icon: UserRound },
] as const;

export function DiagnosticsView({
  account,
  dashboard,
}: {
  account: Account;
  dashboard: DashboardData;
}) {
  const [kind, setKind] = useState<DiagnosticKind>("events");
  const [search, setSearch] = useState("");
  const [type, setType] = useState("");
  const [range, setRange] = useState("7d");
  const now = useNow();
  const journal =
    kind === "events" || kind === "requests" || kind === "decisions";
  const history = useHistory<JsonRecord>(
    journal ? account.id : undefined,
    kind,
  );
  const all = useMemo(() => {
    const initial =
      kind === "events"
        ? dashboard.events
        : kind === "requests"
          ? dashboard.requests
          : kind === "decisions"
            ? dashboard.decisions
            : [];
    const unique = new Map<string, JsonRecord>();
    for (const value of [
      ...initial,
      ...(journal ? (history.data ?? []) : []),
    ]) {
      const row = record(value);
      if (row.account_id !== account.id) {
        continue;
      }
      const key = `${row.event_id ?? ""}:${row.created_at}:${row.method ?? row.ship_id ?? ""}:${row.path ?? row.kind ?? ""}:${text(row.reason, "")}`;
      unique.set(key, row);
    }
    return [...unique.values()].sort(
      (left, right) =>
        Date.parse(text(right.created_at)) - Date.parse(text(left.created_at)),
    );
  }, [
    account.id,
    dashboard.events,
    dashboard.requests,
    dashboard.decisions,
    history.data,
    journal,
    kind,
  ]);
  const typeOf = (row: JsonRecord) => {
    if (kind !== "requests") {
      return text(row.event_type, text(row.kind, ""));
    }
    const status = number(row.status);
    if (row.error || status === 0 || (status !== undefined && status >= 400)) {
      return "Error or transport failure";
    }
    return status !== undefined && status >= 200 && status < 400
      ? "Successful response"
      : "Outcome unclassified";
  };
  const types = [...new Set(all.map(typeOf))].filter(Boolean).sort();
  const cutoff =
    range === "24h" ? now - 86_400_000 : range === "7d" ? now - 604_800_000 : 0;
  const filtered = all.filter(
    (row) =>
      Date.parse(text(row.created_at)) >= cutoff &&
      (!type || typeOf(row) === type) &&
      (JSON.stringify(row) ?? "").toLowerCase().includes(search.toLowerCase()),
  );

  return (
    <div className={`${styles.workspace} ${viewStyles.diagnostics}`}>
      <div
        className={styles.tabs}
        role="group"
        aria-label="Diagnostic record kind"
      >
        {KINDS.map((item) => (
          <button
            key={item.id}
            type="button"
            aria-pressed={kind === item.id}
            onClick={() => {
              setKind(item.id);
              setType("");
            }}
          >
            <item.icon size={15} strokeWidth={1.7} aria-hidden="true" />
            {item.label}
          </button>
        ))}
      </div>
      {kind === "datasets" ? (
        <DatasetHealth account={account} />
      ) : kind === "identity" ? (
        <AccountIdentity account={account} />
      ) : (
        <>
          <Section
            title={
              KINDS.find((item) => item.id === kind)?.label ??
              "Operations journal"
            }
            icon={KINDS.find((item) => item.id === kind)?.icon ?? ScrollText}
            className={styles.section}
          >
            <p className={styles.note}>
              Seven-day retention, up to 500 records per request. Coverage is
              bounded, and request bodies are redacted.
            </p>
            <div className={styles.toolbar}>
              <SearchField
                value={search}
                onChange={setSearch}
                placeholder="Search recorded messages and payloads"
                label="Search recorded diagnostic payloads"
              />
              <label>
                {kind === "requests" ? "Outcome" : "Type"}
                <select
                  value={type}
                  onChange={(event) => setType(event.target.value)}
                >
                  <option value="">
                    All {kind === "requests" ? "outcomes" : "types"}
                  </option>
                  {types.map((value) => (
                    <option key={value} value={value}>
                      {humanize(value)}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Recorded window
                <select
                  value={range}
                  onChange={(event) => setRange(event.target.value)}
                >
                  <option value="24h">Last 24 hours of records</option>
                  <option value="7d">Last 7 days of records</option>
                  <option value="all">All returned records</option>
                </select>
              </label>
              {(search || type || range !== "7d") && (
                <button
                  type="button"
                  className={styles.reset}
                  onClick={() => {
                    setSearch("");
                    setType("");
                    setRange("7d");
                  }}
                >
                  Clear journal filters
                </button>
              )}
            </div>
            <div className={styles.coverage} role="status">
              {count(filtered.length)} matching records from {count(all.length)}{" "}
              supplied records.{" "}
              {all.length > 0 && (
                <>
                  Coverage: {dateTime(text(all.at(-1)?.created_at))} to{" "}
                  {dateTime(text(all[0].created_at))}.{" "}
                </>
              )}
              {history.isFetching && "Updating history. "}Last history
              retrieval:{" "}
              {history.dataUpdatedAt
                ? dateTime(new Date(history.dataUpdatedAt).toISOString())
                : "Not fetched"}
              . Retrieval time is not source freshness.
            </div>
            {history.error && (
              <div className={styles.warning} role="alert">
                History unavailable: {history.error.message}{" "}
                {all.length
                  ? "Same-account retained records remain visible."
                  : "No diagnostic sample is available."}
              </div>
            )}
            {history.data && history.data.length >= 500 && (
              <p className={styles.warning}>
                History response limit reached. Older events may be outside this
                sample even within the selected window.
              </p>
            )}
            {kind === "events" && (
              <DataTable
                label="Recorded game event sample"
                searchable={false}
                data={filtered}
                rowKey={(row, index) =>
                  text(row.event_id, `${row.created_at}:${index}`)
                }
                empty={
                  history.isPending
                    ? "Retrieving recorded events"
                    : "No matching events in the supplied sample"
                }
                columns={[
                  {
                    label: "Recorded",
                    value: (row) => text(row.created_at, "") || undefined,
                    render: (row) => dateTime(text(row.created_at)),
                  },
                  {
                    label: "Event type",
                    value: (row) => text(row.event_type, "") || undefined,
                    render: (row) => (
                      <span className={viewStyles.inline}>
                        <Radio size={13} strokeWidth={1.7} aria-hidden="true" />
                        {humanize(text(row.event_type))}
                      </span>
                    ),
                  },
                  {
                    label: "Event ID",
                    value: (row) => text(row.event_id, "") || undefined,
                    render: (row) => (
                      <span className={styles.identifier}>
                        {text(row.event_id)}
                      </span>
                    ),
                  },
                  {
                    label: "Recorded summary",
                    value: (row) =>
                      text(
                        record(row.payload).reason,
                        text(record(row.payload).detail, ""),
                      ) || undefined,
                    render: (row) =>
                      text(
                        record(row.payload).reason,
                        text(record(row.payload).detail, "No summary recorded"),
                      ),
                  },
                  {
                    label: "Payload",
                    render: (row) => (
                      <StructuredDetails
                        value={row}
                        label="Full recorded event"
                      />
                    ),
                  },
                ]}
              />
            )}
            {kind === "requests" && (
              <DataTable
                label="Recorded game API request sample"
                searchable={false}
                data={filtered}
                rowKey={(row, index) =>
                  `${row.created_at}:${row.method}:${row.path}:${index}`
                }
                empty={
                  history.isPending
                    ? "Retrieving recorded requests"
                    : "No matching requests in the supplied sample"
                }
                columns={[
                  {
                    label: "Recorded",
                    value: (row) => text(row.created_at, "") || undefined,
                    render: (row) => dateTime(text(row.created_at)),
                  },
                  {
                    label: "Method",
                    value: (row) => text(row.method, "") || undefined,
                    render: (row) => text(row.method),
                  },
                  {
                    label: "Path",
                    value: (row) => text(row.path, "") || undefined,
                    render: (row) => (
                      <span className={styles.identifier}>
                        {text(row.path)}
                      </span>
                    ),
                  },
                  {
                    label: "HTTP status",
                    value: (row) => number(row.status),
                    render: (row) => (
                      <Badge
                        icon={
                          typeOf(row) === "Error or transport failure"
                            ? AlertTriangle
                            : typeOf(row) === "Successful response"
                              ? CheckCircle2
                              : Clock3
                        }
                        tone={
                          typeOf(row) === "Error or transport failure"
                            ? "danger"
                            : typeOf(row) === "Successful response"
                              ? "good"
                              : "neutral"
                        }
                      >
                        {number(row.status) === 0
                          ? "Transport failure"
                          : (number(row.status) ?? "Not recorded")}
                      </Badge>
                    ),
                  },
                  {
                    label: "Duration",
                    value: (row) => number(row.duration_ms),
                    render: (row) => `${count(number(row.duration_ms))} ms`,
                    className: styles.numeric,
                  },
                  {
                    label: "Error",
                    value: (row) => text(row.error, "") || undefined,
                    render: (row) => text(row.error, "None recorded"),
                  },
                  {
                    label: "Request and response",
                    render: (row) => (
                      <StructuredDetails
                        value={row}
                        label="Redacted recorded exchange"
                      />
                    ),
                  },
                ]}
              />
            )}
            {kind === "decisions" && (
              <DataTable
                label="Recorded automation decision details"
                searchable={false}
                data={filtered}
                rowKey={(row, index) =>
                  `${row.created_at}:${row.ship_id}:${index}`
                }
                empty="No matching decisions recorded"
                columns={[
                  {
                    label: "Recorded",
                    value: (row) => text(row.created_at, "") || undefined,
                    render: (row) => dateTime(text(row.created_at)),
                  },
                  {
                    label: "Kind",
                    value: (row) => text(row.kind, "") || undefined,
                    render: (row) => (
                      <Badge icon={ScrollText}>
                        {humanize(text(row.kind))}
                      </Badge>
                    ),
                  },
                  {
                    label: "Reason",
                    value: (row) => text(row.reason, "") || undefined,
                    render: (row) => text(row.reason, "No reason recorded"),
                  },
                  {
                    label: "Payload",
                    render: (row) => (
                      <StructuredDetails
                        value={row}
                        label="Full recorded decision"
                      />
                    ),
                  },
                ]}
              />
            )}
          </Section>
        </>
      )}
    </div>
  );
}

function DatasetHealth({ account }: { account: Account }) {
  const metadata = dataset<Record<string, DatasetMeta>>(account, "_meta") ?? {};
  const keys = [
    ...new Set([
      ...Object.keys(metadata),
      ...Object.keys(account.snapshot).filter((key) => !key.startsWith("_")),
    ]),
  ].sort();
  const entries = keys.map((key) => ({ key, meta: metadata[key] ?? {} }));

  return (
    <Section
      title="Recorded dataset health"
      icon={Database}
      className={styles.section}
    >
      <p className={styles.note}>
        Recording success, latest attempt and source time are separate facts.
        Fleet records are delayed after 30 seconds and stale after two minutes.
        Browser polling does not prove that the runner is connected.
      </p>
      <DataTable
        label="Dataset freshness, completeness and errors"
        data={entries}
        rowKey={(row) => row.key}
        empty="No dataset metadata recorded"
        columns={[
          {
            label: "Dataset",
            value: (row) => row.key,
            render: (row) => (
              <span className={styles.identifier}>{row.key}</span>
            ),
          },
          {
            label: "Freshness",
            value: (row) => row.meta.updated_at || undefined,
            render: (row) => <DatasetStatus account={account} name={row.key} />,
          },
          {
            label: "Last successful recording",
            value: (row) => row.meta.updated_at || undefined,
            render: (row) => dateTime(row.meta.updated_at),
          },
          {
            label: "Latest attempt",
            value: (row) => row.meta.attempted_at || undefined,
            render: (row) => dateTime(row.meta.attempted_at),
          },
          {
            label: "Upstream source time",
            value: (row) => row.meta.upstream_as_of || undefined,
            render: (row) => dateTime(row.meta.upstream_as_of),
          },
          {
            label: "Complete",
            value: (row) =>
              row.meta.complete === undefined
                ? undefined
                : row.meta.complete
                  ? "Yes"
                  : "No",
            render: (row) =>
              row.meta.complete === undefined
                ? "Unknown"
                : row.meta.complete
                  ? "Yes"
                  : "No",
          },
          {
            label: "Generation",
            value: (row) => row.meta.generation || undefined,
            render: (row) => (
              <span className={styles.identifier}>
                {row.meta.generation || "Not recorded"}
              </span>
            ),
          },
          {
            label: "Latest error",
            value: (row) => row.meta.error || undefined,
            render: (row) => (
              <span className={row.meta.error ? styles.negative : undefined}>
                {row.meta.error || "None recorded"}
              </span>
            ),
          },
          {
            label: "Fetch metadata",
            render: (row) => <StructuredDetails value={row.meta} />,
          },
        ]}
      />
    </Section>
  );
}

function AccountIdentity({ account }: { account: Account }) {
  const profile = dataset<CompanyProfile>(account, "profile");
  const playerProfile = record(dataset(account, "player"));
  const player = record(playerProfile.player);
  const identity = record(dataset(account, "whoami"));
  const scopes = Array.isArray(identity.scopes)
    ? identity.scopes.filter(
        (scope): scope is string => typeof scope === "string",
      )
    : undefined;
  const companies = rows(playerProfile.companies).map((company) => ({
    id: text(company.id, ""),
    name: text(company.name, ""),
    player_id: text(company.player_id, ""),
    home_port_id: text(company.home_port_id, ""),
    crown_prestige: number(company.crown_prestige),
    created_at: text(company.created_at, ""),
    updated_at: text(company.updated_at, ""),
  }));

  return (
    <div className={styles.workspace}>
      <Section
        title="Configured account and recorded company"
        icon={UserRound}
        className={styles.section}
        actions={<Badge icon={UserRound}>{account.label}</Badge>}
      >
        <details className={viewStyles.disclosure}>
          <summary>Account configuration</summary>
          <dl className={styles.details}>
            <div>
              <dt>Configured account label</dt>
              <dd>{account.label}</dd>
            </div>
            <div>
              <dt>Configured account ID</dt>
              <dd className={styles.identifier}>{account.id}</dd>
            </div>
            <div>
              <dt>Configured company ID</dt>
              <dd className={styles.identifier}>
                {account.company_id || "Not bound"}
              </dd>
            </div>
            <div>
              <dt>Automation setting</dt>
              <dd>
                {account.enabled
                  ? "Enabled in configuration"
                  : "Disabled in configuration"}
                ; runner liveness is not reported
              </dd>
            </div>
            <div>
              <dt>Configured account created</dt>
              <dd>{dateTime(account.created_at)}</dd>
            </div>
            <div>
              <dt>Snapshot published</dt>
              <dd>{dateTime(account.updated_at)}</dd>
            </div>
          </dl>
        </details>
        <Dataset account={account} name="profile">
          <h3>{profile?.company.name || "Company not recorded"}</h3>
          <p className={styles.note}>
            {profile?.handle || "Player not recorded"} at{" "}
            {humanize(profile?.company.home_port_id)}
          </p>
          <details className={viewStyles.disclosure}>
            <summary>Company profile details</summary>
            <dl className={styles.details}>
              <div>
                <dt>Recorded company</dt>
                <dd>{profile?.company.name || "Not recorded"}</dd>
              </div>
              <div>
                <dt>Recorded company ID</dt>
                <dd className={styles.identifier}>
                  {profile?.company.id || "Not recorded"}
                </dd>
              </div>
              <div>
                <dt>Player handle</dt>
                <dd>{profile?.handle || "Not recorded"}</dd>
              </div>
              <div>
                <dt>Company player ID</dt>
                <dd className={styles.identifier}>
                  {profile?.company.player_id || "Not recorded"}
                </dd>
              </div>
              <div>
                <dt>Home port</dt>
                <dd>{humanize(profile?.company.home_port_id)}</dd>
              </div>
              <div>
                <dt>Crown prestige</dt>
                <dd>{count(profile?.company.crown_prestige)}</dd>
              </div>
              <div>
                <dt>Profile fleet count</dt>
                <dd>{count(profile?.fleet_count)}</dd>
              </div>
              <div>
                <dt>Company created</dt>
                <dd>{dateTime(profile?.company.created_at)}</dd>
              </div>
              <div>
                <dt>Company domain updated</dt>
                <dd>{dateTime(profile?.company.updated_at)}</dd>
              </div>
            </dl>
          </details>
        </Dataset>
        <Dataset account={account} name="ships">
          <p className={styles.note}>
            {count(fleet(account).length)} ships in the recorded fleet snapshot.
            This can differ from a separately recorded profile fleet count.
          </p>
        </Dataset>
      </Section>
      <Section
        title="Player identity and company directory"
        icon={UserRound}
        className={styles.section}
      >
        <Dataset account={account} name="player">
          <dl className={styles.details}>
            <div>
              <dt>Player handle</dt>
              <dd>{text(player.handle)}</dd>
            </div>
            <div>
              <dt>Player ID</dt>
              <dd className={styles.identifier}>{text(player.id)}</dd>
            </div>
            <div>
              <dt>Player created</dt>
              <dd>{dateTime(text(player.created_at))}</dd>
            </div>
            <div>
              <dt>Player updated</dt>
              <dd>{dateTime(text(player.updated_at))}</dd>
            </div>
          </dl>
          <p className={styles.note}>
            This directory lists company identities, not independently recorded
            fleet, treasury or account data.
          </p>
          <DataTable
            label="Recorded player company directory"
            data={companies}
            rowKey={(company, index) => company.id || String(index)}
            columns={[
              {
                label: "Company",
                value: (company) => company.name || undefined,
                render: (company) => company.name || "Not recorded",
              },
              {
                label: "Company ID",
                value: (company) => company.id || undefined,
                render: (company) => (
                  <span className={styles.identifier}>
                    {company.id || "Not recorded"}
                  </span>
                ),
              },
              {
                label: "Player ID",
                value: (company) => company.player_id || undefined,
                render: (company) => (
                  <span className={styles.identifier}>
                    {company.player_id || "Not recorded"}
                  </span>
                ),
              },
              {
                label: "Home port",
                value: (company) => company.home_port_id || undefined,
                render: (company) => humanize(company.home_port_id),
              },
              {
                label: "Crown prestige",
                value: (company) => company.crown_prestige,
                render: (company) => count(company.crown_prestige),
                className: styles.numeric,
              },
              {
                label: "Created",
                value: (company) => company.created_at || undefined,
                render: (company) => dateTime(company.created_at),
              },
              {
                label: "Updated",
                value: (company) => company.updated_at || undefined,
                render: (company) => dateTime(company.updated_at),
              },
            ]}
          />
        </Dataset>
      </Section>
      <details className={viewStyles.disclosure}>
        <summary>
          <KeyRound size={17} strokeWidth={1.7} aria-hidden="true" /> Key
          identity
        </summary>
        <Section
          title="Recorded key identity"
          icon={KeyRound}
          className={styles.section}
        >
          <Dataset account={account} name="whoami">
            <dl className={styles.details}>
              <div>
                <dt>API key identifier, not the key</dt>
                <dd className={styles.identifier}>
                  {text(identity.api_key_id)}
                </dd>
              </div>
              <div>
                <dt>Player ID</dt>
                <dd className={styles.identifier}>
                  {text(identity.player_id)}
                </dd>
              </div>
              <div>
                <dt>Bound company ID</dt>
                <dd className={styles.identifier}>
                  {identity.company_id === null
                    ? "Unbound player key"
                    : text(identity.company_id)}
                </dd>
              </div>
              <div>
                <dt>Granted scopes</dt>
                <dd>
                  {scopes
                    ? scopes.length
                      ? scopes.join(", ")
                      : "No scopes"
                    : "Not recorded"}
                </dd>
              </div>
              <div>
                <dt>Expiration</dt>
                <dd>
                  {identity.expires_at === null
                    ? "No expiration recorded"
                    : dateTime(text(identity.expires_at))}
                </dd>
              </div>
            </dl>
            <p className={styles.note}>
              Only identity identifiers, scope and expiration are shown.
              Credentials and bearer tokens are never rendered. This is a
              recorded introspection result, not a live authentication check.
            </p>
          </Dataset>
        </Section>
      </details>
    </div>
  );
}
