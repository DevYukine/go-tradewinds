"use client";

import {
  ArrowLeftRight,
  Boxes,
  CheckCircle2,
  Coins,
  Landmark,
  Layers,
  MapPin,
  Package,
  Users,
  Wheat,
} from "lucide-react";
import { dataset, number, record, rows, text } from "@/lib/data";
import { count, dateTime, humanize, money, percent } from "@/lib/format";
import type {
  Account,
  Catalog,
  JsonRecord,
  Vault,
  Warehouse,
} from "@/lib/types";
import {
  Badge,
  DataTable,
  Dataset,
  DatasetStatus,
  EmptyState,
  Section,
} from "./ui";
import styles from "./operations.module.css";

export function TreasuryView({ account }: { account: Account }) {
  const vaultData = dataset<{ vaults: Vault[] }>(account, "vaults");
  const vaults = vaultData?.vaults ?? [];
  const liquid = vaults
    .filter((item) => item.account_type === "vault")
    .reduce((sum, item) => sum + item.balance_pence, 0);
  const escrow = vaults
    .filter((item) => item.account_type === "escrow")
    .reduce((sum, item) => sum + item.balance_pence, 0);
  const regions = new Map<
    string,
    { region: string; cash?: number; escrow?: number; accounts: Vault[] }
  >();
  for (const vault of vaults) {
    let region = regions.get(vault.banking_region);
    if (!region) {
      region = { region: vault.banking_region, accounts: [] };
      regions.set(vault.banking_region, region);
    }
    region.accounts.push(vault);
    if (vault.account_type === "vault") {
      region.cash = (region.cash ?? 0) + vault.balance_pence;
    } else if (vault.account_type === "escrow") {
      region.escrow = (region.escrow ?? 0) + vault.balance_pence;
    }
  }
  const settings = dataset<{
    auto_remit_for_bills?: boolean;
    auto_remit_min_reserve_pence?: number;
  }>(account, "settings");
  const ledger = rows(dataset(account, "ledger"));
  const rates = rows(dataset(account, "rates"));

  return (
    <div className={styles.workspace}>
      <Section title="Regional vaults" icon={Landmark}>
        <Dataset account={account} name="vaults">
          <dl className={styles.facts}>
            <div>
              <dt>Available cash</dt>
              <dd>{money(liquid)}</dd>
            </div>
            <div>
              <dt>In escrow</dt>
              <dd>{money(escrow)}</dd>
            </div>
          </dl>
          {vaults.length ? (
            <DataTable
              label="Regional cash and escrow"
              data={[...regions.values()]}
              rowKey={(row) => row.region}
              columns={[
                {
                  label: "Region",
                  value: (row) => row.region || undefined,
                  render: (row) => (
                    <span className={styles.iconLabel}>
                      <Landmark
                        size={15}
                        strokeWidth={1.7}
                        aria-hidden="true"
                      />
                      {humanize(row.region)}
                    </span>
                  ),
                },
                {
                  label: "Available cash",
                  value: (row) => row.cash,
                  render: (row) => (
                    <span
                      className={
                        row.cash !== undefined && row.cash < 0
                          ? styles.negative
                          : ""
                      }
                    >
                      {money(row.cash)}
                    </span>
                  ),
                  className: styles.numeric,
                },
                {
                  label: "In escrow",
                  value: (row) => row.escrow,
                  render: (row) => (
                    <span
                      className={
                        row.escrow !== undefined && row.escrow < 0
                          ? styles.negative
                          : ""
                      }
                    >
                      {money(row.escrow)}
                    </span>
                  ),
                  className: styles.numeric,
                },
                {
                  label: "Treasury accounts",
                  value: (row) =>
                    row.accounts.map((vault) => vault.account_id).join(" "),
                  render: (row) => (
                    <details className={styles.inlineDetails}>
                      <summary>
                        {count(row.accounts.length)}{" "}
                        {row.accounts.length === 1 ? "account" : "accounts"}
                      </summary>
                      <dl className={styles.treasuryAccounts}>
                        {row.accounts.map((vault) => (
                          <div key={vault.account_id}>
                            <dt>
                              {humanize(vault.account_type)}
                              <strong
                                className={
                                  vault.balance_pence < 0 ? styles.negative : ""
                                }
                              >
                                {money(vault.balance_pence)}
                              </strong>
                            </dt>
                            <dd>{vault.account_id || "Not recorded"}</dd>
                          </div>
                        ))}
                      </dl>
                    </details>
                  ),
                },
              ]}
            />
          ) : (
            <EmptyState
              icon={Landmark}
              title="No vaults recorded"
              description="The recorded treasury contains no regional accounts."
            />
          )}
          <p className={styles.note}>
            Cash can be spent in its banking region. Escrow is committed
            capital.
          </p>
        </Dataset>
      </Section>
      <Section title="Remittances" icon={ArrowLeftRight}>
        <Dataset account={account} name="rates">
          <DataTable
            label="Regional remittance fees"
            data={rates}
            rowKey={(row, index) =>
              `${row.from_region}:${row.to_region}:${index}`
            }
            empty="No regional remittance rates recorded"
            emptyIcon={ArrowLeftRight}
            columns={[
              {
                label: "From",
                value: (row) => text(row.from_region, "") || undefined,
                render: (row) => (
                  <span className={styles.iconLabel}>
                    <Landmark size={13} strokeWidth={1.7} aria-hidden="true" />
                    {humanize(text(row.from_region))}
                  </span>
                ),
              },
              {
                label: "To",
                value: (row) => text(row.to_region, "") || undefined,
                render: (row) => humanize(text(row.to_region)),
              },
              {
                label: "Fee",
                value: (row) => number(row.fee_bps),
                render: (row) => percent(number(row.fee_bps)),
                className: styles.numeric,
              },
            ]}
          />
        </Dataset>
        <details className={styles.disclosure}>
          <summary>
            <Landmark size={15} strokeWidth={1.7} aria-hidden="true" />
            Remittance policy
          </summary>
          <Dataset account={account} name="settings">
            <dl className={styles.details}>
              <div>
                <dt>Automatic transfers for bills</dt>
                <dd>
                  {settings?.auto_remit_for_bills === undefined ? (
                    "Not recorded"
                  ) : (
                    <Badge
                      icon={ArrowLeftRight}
                      tone={settings.auto_remit_for_bills ? "good" : "neutral"}
                    >
                      {settings.auto_remit_for_bills ? "Enabled" : "Disabled"}
                    </Badge>
                  )}
                </dd>
              </div>
              <div>
                <dt>Minimum regional reserve</dt>
                <dd>{money(settings?.auto_remit_min_reserve_pence)}</dd>
              </div>
            </dl>
            <p className={styles.note}>
              Bills draw from their port&apos;s banking region. This read-only
              view cannot transfer money or change policy.
            </p>
          </Dataset>
        </details>
        <details className={styles.disclosure}>
          <summary>
            <Coins size={15} strokeWidth={1.7} aria-hidden="true" />
            Treasury postings
          </summary>
          <p className={styles.note}>
            This recorded slice shows cash movements, not realized profit.
          </p>
          <Dataset account={account} name="ledger">
            <DataTable
              label="Recorded treasury postings"
              data={ledger}
              rowKey={(row, index) => text(row.id, String(index))}
              emptyIcon={Coins}
              columns={[
                {
                  label: "Recorded",
                  value: (row) => text(row.created_at, "") || undefined,
                  render: (row) => dateTime(text(row.created_at)),
                },
                {
                  label: "Reason",
                  value: (row) => text(row.reason, "") || undefined,
                  render: (row) => humanize(text(row.reason)),
                },
                {
                  label: "Amount",
                  value: (row) => number(row.amount_pence),
                  render: (row) => money(number(row.amount_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Accounts",
                  render: (row) => (
                    <details className={styles.inlineDetails}>
                      <summary>Posting details</summary>
                      <dl className={styles.details}>
                        <div>
                          <dt>Debit account</dt>
                          <dd>{text(row.debit_account_id, "Not recorded")}</dd>
                        </div>
                        <div>
                          <dt>Credit account</dt>
                          <dd>{text(row.credit_account_id, "Not recorded")}</dd>
                        </div>
                        <div>
                          <dt>Transaction group</dt>
                          <dd>{text(row.tx_group_id, "Not recorded")}</dd>
                        </div>
                      </dl>
                    </details>
                  ),
                },
              ]}
            />
          </Dataset>
        </details>
      </Section>
    </div>
  );
}

export function HoldingsView({ account }: { account: Account }) {
  const catalog = dataset<Catalog>(account, "catalog");
  const warehouses = dataset<Warehouse[]>(account, "warehouses") ?? [];
  const factors = dataset<string[]>(account, "factors") ?? [];
  const warehouseInventory = warehouses.flatMap((warehouse) =>
    warehouse.inventory.map((item) => ({
      ...item,
      port_id: warehouse.port_id,
    })),
  );
  const sources = Object.entries(
    dataset<Record<string, unknown>>(account, "inventory") ?? {},
  ).map(([portID, value]) => ({ port_id: portID, inventory: record(value) }));
  const inventory: JsonRecord[] = sources.flatMap(({ port_id, inventory }) =>
    rows(inventory.inventory).map((item) => ({ ...item, port_id })),
  );
  const portName = (portID: string) =>
    catalog?.ports.find((port) => port.id === portID)?.name ?? humanize(portID);
  const region = (portID: string) =>
    catalog?.ports.find((port) => port.id === portID)?.banking_region;
  const commodityLabel = (commodityID: string) => {
    const commodity = catalog?.commodities.find(
      (item) => item.id === commodityID,
    );
    const category = `${commodity?.category ?? ""} ${commodityID}`;
    const Icon = /provision|food|grain|wheat/.test(category)
      ? Wheat
      : /timber|wood/.test(category)
        ? Layers
        : /valuable|precious|gold|silver/.test(category)
          ? Coins
          : Package;
    return (
      <span className={styles.iconLabel}>
        <Icon size={13} strokeWidth={1.7} aria-hidden="true" />
        {commodity?.name ?? humanize(commodityID)}
      </span>
    );
  };

  return (
    <div className={styles.workspace}>
      <Section title="Warehouse leases" icon={Boxes}>
        <Dataset account={account} name="warehouses">
          {warehouses.length ? (
            <div className={styles.cardGrid}>
              {warehouses.map((warehouse) => (
                <article
                  className={styles.warehouseCard}
                  key={warehouse.port_id}
                >
                  <div className={styles.cardLabel}>
                    <MapPin size={17} strokeWidth={1.7} aria-hidden="true" />
                    <h3>{portName(warehouse.port_id)}</h3>
                  </div>
                  <p className={styles.context}>
                    {humanize(region(warehouse.port_id))}
                  </p>
                  <div className={styles.capacityLabel}>
                    <span>{count(warehouse.used_tonnage)} t used</span>
                    <span>{count(warehouse.leased_tonnage)} t leased</span>
                  </div>
                  {warehouse.leased_tonnage > 0 ? (
                    <progress
                      className={styles.capacity}
                      max={warehouse.leased_tonnage}
                      value={warehouse.used_tonnage}
                      aria-label={`${portName(warehouse.port_id)} warehouse capacity`}
                    />
                  ) : (
                    <p className={styles.context}>
                      No leased capacity recorded
                    </p>
                  )}
                  <div className={styles.context}>
                    <span>{count(warehouse.free_tonnage)} t free</span>
                    <span>{count(warehouse.reserved_tonnage)} t reserved</span>
                  </div>
                  <Badge
                    icon={Users}
                    tone={warehouse.has_factor ? "good" : "neutral"}
                  >
                    {warehouse.has_factor === undefined
                      ? "Factor not recorded"
                      : warehouse.has_factor
                        ? "Resident factor"
                        : "No resident factor"}
                  </Badge>
                </article>
              ))}
            </div>
          ) : (
            <EmptyState
              icon={Boxes}
              title="No warehouse leases"
              description="No warehouse leases are present in the recorded account data."
            />
          )}
        </Dataset>
      </Section>
      <Section title="Stored cargo" icon={Package}>
        <Dataset account={account} name="warehouses">
          <DataTable
            label="Stored warehouse cargo"
            data={warehouseInventory}
            rowKey={(row) => `${row.port_id}:${row.commodity_id}`}
            empty="No stored warehouse cargo recorded"
            emptyIcon={Package}
            columns={[
              {
                label: "Cargo",
                value: (row) => row.commodity_id,
                render: (row) => commodityLabel(row.commodity_id),
              },
              {
                label: "Port",
                value: (row) => portName(row.port_id),
                render: (row) => portName(row.port_id),
              },
              {
                label: "Owned",
                value: (row) => row.quantity,
                render: (row) => `${count(row.quantity)} t`,
                className: styles.numeric,
              },
              {
                label: "Locked",
                value: (row) => row.locked_quantity,
                render: (row) => `${count(row.locked_quantity)} t`,
                className: styles.numeric,
              },
              {
                label: "Updated",
                value: (row) => row.updated_at || undefined,
                render: (row) => dateTime(row.updated_at),
              },
            ]}
          />
        </Dataset>
        <details className={styles.disclosure}>
          <summary>
            <Users size={15} strokeWidth={1.7} aria-hidden="true" />
            Resident factors
          </summary>
          <Dataset account={account} name="factors">
            <DataTable
              label="Resident factors by port"
              data={factors.map((portID) => ({ portID }))}
              rowKey={({ portID }) => portID}
              empty="No resident factors recorded"
              emptyIcon={Users}
              columns={[
                {
                  label: "Port",
                  value: ({ portID }) => portName(portID),
                  render: ({ portID }) => (
                    <span className={styles.iconLabel}>
                      <MapPin size={13} strokeWidth={1.7} aria-hidden="true" />
                      {portName(portID)}
                    </span>
                  ),
                },
                {
                  label: "Region",
                  value: ({ portID }) => region(portID),
                  render: ({ portID }) => humanize(region(portID)),
                },
                {
                  label: "Visibility",
                  render: () => (
                    <Badge icon={CheckCircle2} tone="good">
                      Full market depth
                    </Badge>
                  ),
                },
              ]}
            />
            <p className={styles.note}>
              Factors provide full market depth without a ship in port. Hiring
              and upkeep are managed by the runner.
            </p>
          </Dataset>
        </details>
        <details className={styles.disclosure}>
          <summary>
            <Package size={15} strokeWidth={1.7} aria-hidden="true" />
            Separate port inventory
          </summary>
          <p className={styles.note}>
            Port inventory is recorded separately from warehouse manifests.
            These quantities are not added together.
          </p>
          <Dataset account={account} name="inventory">
            <DataTable
              label="Separately recorded port inventory"
              data={inventory}
              rowKey={(row, index) =>
                `${row.port_id}:${row.commodity_id}:${index}`
              }
              columns={[
                {
                  label: "Cargo",
                  value: (row) => text(row.commodity_id, "") || undefined,
                  render: (row) => commodityLabel(text(row.commodity_id)),
                },
                {
                  label: "Port",
                  value: (row) => text(row.port_id, "") || undefined,
                  render: (row) => portName(text(row.port_id)),
                },
                {
                  label: "Owned",
                  value: (row) => number(row.quantity),
                  render: (row) => `${count(number(row.quantity))} t`,
                  className: styles.numeric,
                },
                {
                  label: "Locked",
                  value: (row) => number(row.locked_quantity),
                  render: (row) => `${count(number(row.locked_quantity))} t`,
                  className: styles.numeric,
                },
                {
                  label: "Updated",
                  value: (row) => text(row.updated_at, "") || undefined,
                  render: (row) => dateTime(text(row.updated_at)),
                },
              ]}
            />
            <details className={styles.inlineDetails}>
              <summary>Port sources, including empty inventories</summary>
              <DataTable
                label="Port inventory sources"
                data={sources}
                rowKey={(row) => row.port_id}
                emptyIcon={MapPin}
                columns={[
                  {
                    label: "Port",
                    value: (row) => portName(row.port_id),
                    render: (row) => portName(row.port_id),
                  },
                  {
                    label: "Capacity",
                    render: (row) => (
                      <dl className={styles.details}>
                        <div>
                          <dt>Leased</dt>
                          <dd>
                            {count(number(row.inventory.leased_tonnage))} t
                          </dd>
                        </div>
                        <div>
                          <dt>Used</dt>
                          <dd>{count(number(row.inventory.used_tonnage))} t</dd>
                        </div>
                        <div>
                          <dt>Free</dt>
                          <dd>{count(number(row.inventory.free_tonnage))} t</dd>
                        </div>
                        <div>
                          <dt>Reserved</dt>
                          <dd>
                            {count(number(row.inventory.reserved_tonnage))} t
                          </dd>
                        </div>
                      </dl>
                    ),
                  },
                  {
                    label: "Freshness",
                    render: (row) => (
                      <DatasetStatus
                        account={account}
                        name={`inventory/${row.port_id}`}
                      />
                    ),
                  },
                ]}
              />
            </details>
          </Dataset>
        </details>
      </Section>
    </div>
  );
}
