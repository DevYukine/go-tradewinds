"use client";

import {
  AlertTriangle,
  Anchor,
  Clock3,
  MapPin,
  Package,
  Sailboat,
  Utensils,
} from "lucide-react";
import { useNow } from "@/app/providers";
import { fleet, shipSource } from "@/lib/data";
import { count, dateTime, humanize } from "@/lib/format";
import type { Account, ShipDetail } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { RecentSettlements } from "./settlements";
import {
  Badge,
  DataTable,
  Dataset,
  DatasetStatus,
  EmptyState,
  Section,
  SpriteImage,
  StatusBadge,
} from "./ui";
import styles from "./world-views.module.css";

function arrival(detail: ShipDetail) {
  return detail.voyage?.estimated_arrival_at ?? detail.ship.leg_arrives_at;
}

export function VoyagesView({ account }: { account: Account }) {
  const now = useNow();
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedAccountID = useUI((state) => state.selectedShipAccountID);
  const selectShip = useUI((state) => state.selectShip);
  const ships = fleet(account);
  const sailing = ships
    .filter((detail) => detail.ship.status === "sailing")
    .sort((left, right) => {
      const leftTime = Date.parse(arrival(left) ?? "");
      const rightTime = Date.parse(arrival(right) ?? "");
      return (
        (Number.isFinite(leftTime) ? leftTime : Infinity) -
        (Number.isFinite(rightTime) ? rightTime : Infinity)
      );
    });
  const focused =
    sailing.find(
      (detail) =>
        selectedAccountID === account.id && detail.ship.id === selectedShipID,
    ) ?? sailing[0];
  const leg = focused?.route_legs?.[focused.ship.current_leg_idx];
  const destination = focused?.route_legs?.at(-1)?.to_node;
  const eta = focused && arrival(focused);
  const awaiting =
    eta && Number.isFinite(Date.parse(eta)) && Date.parse(eta) <= now;

  return (
    <div className="view-stack">
      <Section
        title="Active voyage"
        icon={Sailboat}
        actions={
          sailing.length > 1 ? (
            <label>
              <span className="sr-only">Choose an active voyage</span>
              <select
                value={focused?.ship.id ?? ""}
                onChange={(event) => selectShip(event.target.value, account.id)}
              >
                {sailing.map((detail) => (
                  <option key={detail.ship.id} value={detail.ship.id}>
                    {detail.ship.name}
                  </option>
                ))}
              </select>
            </label>
          ) : undefined
        }
      >
        <Dataset account={account} name="ships">
          {focused ? (
            <div className="voyage-focus">
              <div className="vessel-identity">
                <SpriteImage name={focused.ship.name} />
                <div>
                  <h3>{focused.ship.name}</h3>
                  <StatusBadge status={focused.ship.status} />
                </div>
              </div>
              <DatasetStatus
                account={account}
                name={shipSource(account, focused.ship.id)}
              />
              <dl className="voyage-facts">
                <div>
                  <dt className={styles.inline}>
                    <Sailboat size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
                    Current leg
                  </dt>
                  <dd>
                    {leg
                      ? `${humanize(leg.from_node)} to ${humanize(leg.to_node)}`
                      : "Not recorded"}
                  </dd>
                </div>
                <div>
                  <dt className={styles.inline}>
                    <MapPin size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
                    Destination
                  </dt>
                  <dd>
                    {destination ? humanize(destination) : "Not recorded"}
                  </dd>
                </div>
                <div>
                  <dt className={styles.inline}>
                    <Clock3 size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
                    Estimated arrival
                  </dt>
                  <dd>{eta ? dateTime(eta) : "Not recorded"}</dd>
                </div>
                <div>
                  <dt className={styles.inline}>
                    <Package size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
                    Cargo aboard
                  </dt>
                  <dd>{count(focused.cargo_used_tons)} tons</dd>
                </div>
                <div>
                  <dt className={styles.inline}>
                    <Utensils size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
                    Provisions
                  </dt>
                  <dd>{count(focused.stores?.sea_days_remaining)} sea days</dd>
                </div>
              </dl>
              {awaiting && (
                <p className={`section-note ${styles.inline}`}>
                  <Clock3 size={15} strokeWidth={1.7} aria-hidden="true" />
                  Estimated arrival has passed. Awaiting a recorded arrival.
                </p>
              )}
              {focused.voyage?.legs.some((item) => item.starving) && (
                <p className={`notice notice-danger ${styles.inline}`}>
                  <AlertTriangle
                    size={15}
                    strokeWidth={1.7}
                    aria-hidden="true"
                  />
                  The recorded voyage estimate includes a starving leg.
                </p>
              )}
            </div>
          ) : (
            <EmptyState
              title="No active voyage recorded"
              description="Ships recorded as sailing appear here; berthed and waiting ships are in Vessels."
              icon={Sailboat}
            />
          )}
        </Dataset>
      </Section>
      <Section title="Arrivals and settlements" icon={Anchor}>
        <h3>Expected arrivals</h3>
        <p className="section-note">
          Recorded estimates, not confirmed arrivals. Select a vessel to focus
          its voyage.
        </p>
        {sailing.length === 0 ? (
          <EmptyState
            icon={Anchor}
            title="No arrivals recorded"
            description="Arrival estimates appear when a vessel is recorded as sailing."
          />
        ) : (
          <div className={styles.arrivals}>
            {sailing.slice(0, 4).map((detail) => {
              const time = arrival(detail);
              const overdue =
                time &&
                Number.isFinite(Date.parse(time)) &&
                Date.parse(time) <= now;
              const port = detail.route_legs?.at(-1)?.to_node;
              return (
                <button
                  key={detail.ship.id}
                  type="button"
                  className={styles.arrival}
                  aria-pressed={focused?.ship.id === detail.ship.id}
                  onClick={() => selectShip(detail.ship.id, account.id)}
                >
                  <span className={styles.inline}>
                    <SpriteImage name={detail.ship.name} />
                    <span className={styles.arrivalIdentity}>
                      <strong>{detail.ship.name}</strong>
                      <span>
                        {port ? humanize(port) : "Destination not recorded"}
                      </span>
                    </span>
                  </span>
                  <span className={styles.arrivalTime}>
                    <Badge icon={Clock3} tone={overdue ? "warning" : "neutral"}>
                      {overdue
                        ? "Awaiting arrival record"
                        : "Estimated arrival"}
                    </Badge>
                    <span>{time ? dateTime(time) : "Time not recorded"}</span>
                  </span>
                </button>
              );
            })}
          </div>
        )}
        {sailing.length > 0 && (
          <details className={styles.disclosure}>
            <summary>All {count(sailing.length)} active voyages</summary>
            <DataTable
              label="Expected vessel arrivals"
              data={sailing}
              rowKey={(detail) => detail.ship.id}
              empty="No sailing vessels recorded"
              columns={[
                {
                  label: "Vessel",
                  value: (detail) => detail.ship.name,
                  render: (detail) => (
                    <button
                      className="button button-quiet vessel-identity"
                      onClick={() => selectShip(detail.ship.id, account.id)}
                      aria-pressed={focused?.ship.id === detail.ship.id}
                    >
                      <SpriteImage name={detail.ship.name} />
                      {detail.ship.name}
                    </button>
                  ),
                },
                {
                  label: "Destination",
                  value: (detail) => detail.route_legs?.at(-1)?.to_node,
                  render: (detail) => {
                    const port = detail.route_legs?.at(-1)?.to_node;
                    return port ? humanize(port) : "Not recorded";
                  },
                },
                {
                  label: "Estimated arrival",
                  value: (detail) => {
                    const time = arrival(detail);
                    const parsed = time ? Date.parse(time) : NaN;
                    return Number.isFinite(parsed) ? parsed : undefined;
                  },
                  render: (detail) =>
                    arrival(detail)
                      ? dateTime(arrival(detail))
                      : "Not recorded",
                },
              ]}
            />
          </details>
        )}
        <RecentSettlements account={account} />
      </Section>
    </div>
  );
}
