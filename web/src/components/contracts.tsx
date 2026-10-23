"use client";

import { FileText, Megaphone, ScrollText } from "lucide-react";
import { useState } from "react";
import { useNow } from "@/app/providers";
import { dataset, fleet } from "@/lib/data";
import { count, dateTime, eta, humanize, money } from "@/lib/format";
import type { Account, CompanyProfile, Contract } from "@/lib/types";
import { Badge, DataTable, Dataset, Section, StatusBadge } from "./ui";
import styles from "./operations.module.css";

export function ContractsView({ account }: { account: Account }) {
  const [type, setType] = useState("");
  const [status, setStatus] = useState("");
  const [scope, setScope] = useState("");
  const now = useNow();
  const contracts = dataset<Contract[]>(account, "contracts") ?? [];
  const companyID =
    dataset<CompanyProfile>(account, "profile")?.company.id ??
    account.company_id;
  const ships = fleet(account);
  const relationship = (contract: Contract) => {
    if (companyID && contract.carrier_company_id === companyID) {
      return "Owned carriage";
    }
    if (companyID && contract.issuer_company_id === companyID) {
      return "Issued by company";
    }
    if (contract.is_public === true) {
      return "Public listing";
    }
    return contract.is_public === false
      ? "Recorded listing"
      : "Ownership not recorded";
  };
  const filtered = contracts.filter(
    (contract) =>
      (!type || contract.contract_type === type) &&
      (!status || contract.status === status) &&
      (!scope || relationship(contract) === scope),
  );
  const types = [
    ...new Set(contracts.map((contract) => contract.contract_type)),
  ].sort();
  const statuses = [
    ...new Set(contracts.map((contract) => contract.status)),
  ].sort();
  const scopes = [...new Set(contracts.map(relationship))].sort();

  return (
    <div className={styles.workspace}>
      <Section title="Contracts" icon={FileText}>
        <div className={styles.toolbar}>
          <label>
            Relationship
            <select
              value={scope}
              onChange={(event) => setScope(event.target.value)}
            >
              <option value="">All recorded contracts</option>
              {scopes.map((value) => (
                <option key={value} value={value}>
                  {value}
                </option>
              ))}
            </select>
          </label>
          <label>
            Type
            <select
              value={type}
              onChange={(event) => setType(event.target.value)}
            >
              <option value="">All types</option>
              {types.map((value) => (
                <option key={value} value={value}>
                  {humanize(value)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Status
            <select
              value={status}
              onChange={(event) => setStatus(event.target.value)}
            >
              <option value="">All statuses</option>
              {statuses.map((value) => (
                <option key={value} value={value}>
                  {humanize(value)}
                </option>
              ))}
            </select>
          </label>
          {(scope || type || status) && (
            <button
              type="button"
              className={styles.reset}
              onClick={() => {
                setScope("");
                setType("");
                setStatus("");
              }}
            >
              Clear filters
            </button>
          )}
        </div>
        <p className={styles.note}>
          Listings are opportunities, not company obligations. Rewards are
          revenue before cargo, collateral and voyage costs.
        </p>
        <Dataset account={account} name="contracts">
          <p className={styles.context}>
            {count(filtered.length)} matching contracts of{" "}
            {count(contracts.length)} recorded
          </p>
          <DataTable
            label="Recorded contracts and listings"
            data={filtered}
            rowKey={(contract) => contract.id}
            empty="No recorded contracts match these filters"
            emptyIcon={FileText}
            columns={[
              {
                label: "Contract",
                value: (contract) =>
                  contract.title || contract.commodity_id || contract.id,
                render: (contract) => (
                  <span className={styles.primary}>
                    <strong>
                      {contract.title ||
                        (contract.commodity_id
                          ? humanize(contract.commodity_id)
                          : "Contract")}
                    </strong>
                    <small>{humanize(contract.contract_type)}</small>
                  </span>
                ),
              },
              {
                label: "Kind",
                value: relationship,
                render: (contract) => (
                  <Badge
                    icon={
                      relationship(contract) === "Owned carriage"
                        ? FileText
                        : relationship(contract) === "Issued by company"
                          ? ScrollText
                          : Megaphone
                    }
                    tone={
                      relationship(contract) === "Owned carriage"
                        ? "blue"
                        : "neutral"
                    }
                  >
                    {relationship(contract)}
                  </Badge>
                ),
              },
              {
                label: "Route",
                value: (contract) =>
                  `${contract.origin_port_id} ${contract.dest_port_id ?? ""}`,
                render: (contract) => (
                  <span className={styles.primary}>
                    {humanize(contract.origin_port_id)}
                    <small>
                      {contract.dest_port_id
                        ? `To ${humanize(contract.dest_port_id)}`
                        : "Delivery at origin"}
                    </small>
                  </span>
                ),
              },
              {
                label: "Progress",
                value: (contract) => contract.remaining_quantity,
                render: (contract) => (
                  <span className={styles.primary}>
                    <StatusBadge status={contract.status} />
                    <small>
                      {count(contract.remaining_quantity)} t remaining
                    </small>
                    {contract.stale && (
                      <Badge tone="warning" icon={Megaphone}>
                        Delayed gossip
                      </Badge>
                    )}
                  </span>
                ),
              },
              {
                label: "Reward",
                value: (contract) => contract.reward_pence,
                render: (contract) => money(contract.reward_pence),
                className: styles.numeric,
              },
              {
                label: "Terms",
                render: (contract) => (
                  <details className={styles.inlineDetails}>
                    <summary>Contract details</summary>
                    <dl className={styles.details}>
                      <div>
                        <dt>Commodity</dt>
                        <dd>{humanize(contract.commodity_id)}</dd>
                      </div>
                      <div>
                        <dt>Required</dt>
                        <dd>{count(contract.quantity_required)} t</dd>
                      </div>
                      <div>
                        <dt>Delivered</dt>
                        <dd>{count(contract.quantity_delivered)} t</dd>
                      </div>
                      <div>
                        <dt>Reward per ton</dt>
                        <dd>{money(contract.reward_per_ton_pence)}</dd>
                      </div>
                      <div>
                        <dt>Collateral</dt>
                        <dd>{money(contract.collateral_pence)}</dd>
                      </div>
                      <div>
                        <dt>Deadline</dt>
                        <dd>
                          {contract.expires_at ? (
                            <span className={styles.primary}>
                              {dateTime(contract.expires_at)}
                              <small>
                                {Date.parse(contract.expires_at) <= now
                                  ? "Recorded deadline passed"
                                  : `${eta(contract.expires_at, now)} remaining`}
                              </small>
                            </span>
                          ) : (
                            "None recorded"
                          )}
                        </dd>
                      </div>
                      <div>
                        <dt>Assigned ship</dt>
                        <dd>
                          {contract.carrier_ship_id
                            ? (ships.find(
                                (item) =>
                                  item.ship.id === contract.carrier_ship_id,
                              )?.ship.name ?? contract.carrier_ship_id)
                            : "Unassigned"}
                        </dd>
                      </div>
                      <div>
                        <dt>Issuer company</dt>
                        <dd>{contract.issuer_company_id || "Not recorded"}</dd>
                      </div>
                      <div>
                        <dt>Carrier company</dt>
                        <dd>{contract.carrier_company_id || "Not recorded"}</dd>
                      </div>
                      <div>
                        <dt>Visibility</dt>
                        <dd>
                          {contract.is_public === true
                            ? "Public"
                            : contract.is_public === false
                              ? "Presence or party access"
                              : "Not recorded"}
                        </dd>
                      </div>
                      <div>
                        <dt>Record quality</dt>
                        <dd>
                          {contract.stale === undefined
                            ? "Not recorded"
                            : contract.stale
                              ? "Delayed gossip"
                              : "Recorded"}
                        </dd>
                      </div>
                      <div>
                        <dt>Contract ID</dt>
                        <dd>{contract.id}</dd>
                      </div>
                    </dl>
                  </details>
                ),
              },
            ]}
          />
        </Dataset>
        <details className={styles.disclosure}>
          <summary>
            <ScrollText size={15} strokeWidth={1.7} aria-hidden="true" />
            Carriage and listing rules
          </summary>
          <p className={styles.note}>
            Owned carriage is identified by the carrier company ID. Issued
            contracts and public listings are shown separately.
          </p>
          <p className={styles.note}>
            Charter cargo must arrive from another port. Sealed freight cannot
            be sold or delivered to charters.
          </p>
          <p className={styles.note}>
            Delayed gossip may contain outdated quantities or status. A deadline
            passing locally does not change recorded status.
          </p>
        </details>
      </Section>
    </div>
  );
}
