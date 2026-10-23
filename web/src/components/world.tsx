"use client";

import {
  CheckCircle2,
  FileText,
  Flag,
  Map,
  Megaphone,
  Pause,
  Shield,
  Ship,
  Sun,
  Users,
  Waves,
} from "lucide-react";
import { useState } from "react";
import { dataset, fleet, number, record, rows, text } from "@/lib/data";
import { count, dateTime, humanize, money, percent } from "@/lib/format";
import type { Account, Catalog, Clock, JsonRecord } from "@/lib/types";
import {
  Badge,
  Section,
  DataTable,
  Dataset,
  EmptyState,
  Progress,
  StatusBadge,
} from "./ui";
import styles from "./operations.module.css";
import viewStyles from "./world-views.module.css";

export function WorldView({ account }: { account: Account }) {
  const [noticeScope, setNoticeScope] = useState("active");
  const [zoneScope, setZoneScope] = useState("fleet");
  const clock = dataset<Clock>(account, "clock");
  const expansion = record(dataset(account, "expansion"));
  const tier = record(expansion.current_tier);
  const progress = number(tier.progress_bps);
  const catalog = dataset<Catalog>(account, "catalog");
  const events = rows(dataset(account, "world_events")).sort(
    (left, right) =>
      Number(right.is_active === true) - Number(left.is_active === true),
  );
  const zones = rows(dataset(account, "zones"));
  const convoys = rows(dataset(account, "convoys")).map((entry) => ({
    entry,
    convoy: record(entry.convoy),
  }));
  const marques = rows(dataset(account, "marques"));
  const patrols = rows(dataset(account, "patrols"));
  const visibleEvents =
    noticeScope === "active"
      ? events.filter((event) => event.is_active === true)
      : events;
  const fleetZoneIDs = new Set<string>();
  for (const detail of fleet(account)) {
    const nodes = [
      detail.ship.current_port_id,
      ...(detail.route_legs ?? []).flatMap((leg) => [
        leg.from_node,
        leg.to_node,
      ]),
    ];
    for (const node of nodes) {
      const port = catalog?.ports.find((port) => port.id === node);
      const zoneID =
        port?.sea_zone_id ??
        catalog?.sea_zones.find((zone) => zone.id === node)?.id;
      if (zoneID) {
        fleetZoneIDs.add(zoneID);
      }
    }
  }
  const visibleZones =
    zoneScope === "fleet" && fleetZoneIDs.size > 0
      ? zones.filter((zone) =>
          fleetZoneIDs.has(text(zone.sea_zone_id, text(zone.id, ""))),
        )
      : zones;

  return (
    <div className={styles.workspace}>
      <div className={styles.split}>
        <Section
          title="Season and simulation"
          icon={Sun}
          className={styles.section}
        >
          <Dataset account={account} name="clock">
            {clock && (
              <>
                <dl className={styles.facts}>
                  <div>
                    <dt>Game calendar</dt>
                    <dd>
                      {String(clock.calendar.day).padStart(2, "0")}.
                      {String(clock.calendar.month).padStart(2, "0")}.
                      {clock.calendar.year}{" "}
                      {String(clock.calendar.hour).padStart(2, "0")}:
                      {String(clock.calendar.minute).padStart(2, "0")}
                    </dd>
                  </div>
                  <div>
                    <dt>Recorded world state</dt>
                    <dd>
                      <Badge
                        tone={clock.paused ? "warning" : "good"}
                        icon={clock.paused ? Pause : Sun}
                      >
                        {clock.paused ? "Paused" : "Running"}
                      </Badge>
                    </dd>
                  </div>
                  <div>
                    <dt>Season</dt>
                    <dd>
                      {count(clock.season_number)};{" "}
                      {humanize(clock.calendar.season)}
                    </dd>
                  </div>
                  <div>
                    <dt>Unlocked phase</dt>
                    <dd>Phase {count(clock.unlocked_phase)}</dd>
                  </div>
                </dl>
                <dl className={styles.facts}>
                  <div>
                    <dt>Game time scale</dt>
                    <dd>{count(clock.calendar.time_scale)}× real time</dd>
                  </div>
                  <div>
                    <dt>Global economic health</dt>
                    <dd>{percent(clock.global_health_bps)}</dd>
                  </div>
                  <div>
                    <dt>Active-player turnover, 24h</dt>
                    <dd>{money(clock.active_player_volume_24h)}</dd>
                  </div>
                  <div>
                    <dt>Planned season end</dt>
                    <dd>
                      {clock.season_planned_end_at
                        ? dateTime(clock.season_planned_end_at)
                        : "Not announced"}
                    </dd>
                  </div>
                  <div>
                    <dt>Recorded real UTC clock</dt>
                    <dd>{dateTime(clock.real_utc)}</dd>
                  </div>
                </dl>
              </>
            )}
          </Dataset>
        </Section>
        <Section title="World expansion" icon={Map} className={styles.section}>
          <Dataset account={account} name="expansion">
            {Object.keys(tier).length ? (
              <>
                <h3>{text(tier.title, "Current expansion tier")}</h3>
                <dl className={styles.details}>
                  <div>
                    <dt>Target phase</dt>
                    <dd>{count(number(tier.target_phase))}</dd>
                  </div>
                  <div>
                    <dt>Delivered / target</dt>
                    <dd>
                      {count(number(tier.tons_delivered))} /{" "}
                      {count(number(tier.target_tons))} t
                    </dd>
                  </div>
                  <div>
                    <dt>Progress</dt>
                    <dd>{percent(progress)}</dd>
                  </div>
                  <div>
                    <dt>Remaining</dt>
                    <dd>{count(number(tier.tons_remaining))} t</dd>
                  </div>
                </dl>
                <details className={viewStyles.disclosure}>
                  <summary>Contribution details</summary>
                  <dl className={styles.details}>
                    <div>
                      <dt>Completion bonus pool</dt>
                      <dd>{money(number(tier.completion_bonus_pence))}</dd>
                    </div>
                    <div>
                      <dt>Active trading companies</dt>
                      <dd>
                        {count(number(expansion.active_trading_companies))}
                      </dd>
                    </div>
                  </dl>
                  <ExpansionTierDetails tier={tier} />
                </details>
                {progress !== undefined && (
                  <Progress
                    value={progress / 100}
                    label="Recorded world expansion progress"
                  />
                )}
              </>
            ) : (
              <EmptyState
                icon={Map}
                title="No open expansion tier recorded"
                description={`Recorded unlocked phase: ${count(number(expansion.unlocked_phase))}.`}
              />
            )}
            <details className={viewStyles.disclosure}>
              <summary>
                <CheckCircle2 size={17} strokeWidth={1.7} aria-hidden="true" />
                Completed tiers
              </summary>
              {!Array.isArray(expansion.completed_tiers) ? (
                <p className={styles.note}>Not recorded</p>
              ) : expansion.completed_tiers.length === 0 ? (
                <p className={styles.note}>No completed tiers recorded.</p>
              ) : (
                rows(expansion.completed_tiers).map((completed, index) => (
                  <details
                    key={text(completed.title, String(index))}
                    className={viewStyles.disclosure}
                  >
                    <summary>{text(completed.title)}</summary>
                    <dl className={styles.details}>
                      <div>
                        <dt>Unlocked phase</dt>
                        <dd>{count(number(completed.target_phase))}</dd>
                      </div>
                      <div>
                        <dt>Completed</dt>
                        <dd>{dateTime(text(completed.completed_at))}</dd>
                      </div>
                      <div>
                        <dt>Delivered / target</dt>
                        <dd>
                          {count(number(completed.tons_delivered))} /{" "}
                          {count(number(completed.target_tons))} t
                        </dd>
                      </div>
                      <div>
                        <dt>Completion bonus pool</dt>
                        <dd>
                          {money(number(completed.completion_bonus_pence))}
                        </dd>
                      </div>
                    </dl>
                    <ExpansionTierDetails tier={completed} />
                  </details>
                ))
              )}
            </details>
          </Dataset>
        </Section>
      </div>
      <Section
        title="World notices"
        icon={Megaphone}
        className={styles.section}
        actions={
          <label className={styles.context}>
            Show
            <select
              value={noticeScope}
              onChange={(event) => setNoticeScope(event.target.value)}
              aria-label="World notice scope"
            >
              <option value="active">Active notices</option>
              <option value="all">All recorded notices</option>
            </select>
          </label>
        }
      >
        <Dataset account={account} name="world_events">
          <DataTable
            label="Recorded active and historical world events"
            data={visibleEvents}
            rowKey={(event, index) => text(event.id, String(index))}
            empty={
              noticeScope === "active"
                ? "No active world notices recorded"
                : "No world notices recorded"
            }
            columns={[
              {
                label: "Notice",
                value: (event) =>
                  text(event.title, text(event.event_type, "")) || undefined,
                render: (event) => (
                  <span className={styles.primary}>
                    <strong>
                      {text(event.title, humanize(text(event.event_type)))}
                    </strong>
                    <small>
                      {text(event.description, "No description recorded.")}
                    </small>
                  </span>
                ),
              },
              {
                label: "Target",
                value: (event) =>
                  text(
                    event.target_port_id,
                    text(event.target_sea_zone_id, ""),
                  ) || undefined,
                render: (event) =>
                  humanize(
                    text(
                      event.target_port_id,
                      text(event.target_sea_zone_id, "World-wide"),
                    ),
                  ),
              },
              {
                label: "Notice expires",
                value: (event) => text(event.expires_at, "") || undefined,
                render: (event) => dateTime(text(event.expires_at)),
              },
              {
                label: "Type",
                value: (event) => text(event.event_type, "") || undefined,
                render: (event) => humanize(text(event.event_type)),
              },
              {
                label: "State",
                value: (event) =>
                  event.is_active === true
                    ? "Active"
                    : event.is_active === false
                      ? "Historical"
                      : undefined,
                render: (event) =>
                  event.is_active === undefined ? (
                    "Not recorded"
                  ) : (
                    <Badge tone={event.is_active ? "warning" : "neutral"}>
                      {event.is_active ? "Active" : "Historical"}
                    </Badge>
                  ),
              },
              {
                label: "Structural",
                value: (event) =>
                  event.structural === true
                    ? "Yes"
                    : event.structural === false
                      ? "No"
                      : undefined,
                render: (event) =>
                  event.structural === true
                    ? "Yes"
                    : event.structural === false
                      ? "No"
                      : "Not recorded",
              },
              {
                label: "Starts",
                value: (event) => text(event.starts_at, "") || undefined,
                render: (event) => dateTime(text(event.starts_at)),
              },
            ]}
          />
          <p className={styles.note}>
            A structural notice’s expiration is not the end of the underlying
            economic shift. Hidden magnitudes and durations are not published.
          </p>
        </Dataset>
      </Section>
      <Section
        title="Zone conditions"
        icon={Waves}
        className={styles.section}
        actions={
          <label className={styles.context}>
            Show
            <select
              value={zoneScope}
              onChange={(event) => setZoneScope(event.target.value)}
              aria-label="Zone condition scope"
            >
              <option value="fleet">Fleet ports and routes</option>
              <option value="all">All recorded zones</option>
            </select>
          </label>
        }
      >
        <Dataset account={account} name="zones">
          <p className={styles.note}>
            {zoneScope === "fleet"
              ? fleetZoneIDs.size > 0
                ? "Zones named by recorded fleet ports and routes. "
                : "Fleet zone mapping is unavailable; all recorded zones are shown. "
              : ""}
            Zone piracy figures are recorded conditions, not current voyage loss
            probabilities.
          </p>
          <DataTable
            label="Recorded sea zone conditions"
            data={visibleZones}
            empty={
              zoneScope === "fleet"
                ? "No recorded conditions match the fleet ports and routes"
                : "No sea-zone conditions recorded"
            }
            rowKey={(zone, index) =>
              text(zone.sea_zone_id, text(zone.id, String(index)))
            }
            columns={[
              {
                label: "Sea zone",
                value: (zone) =>
                  text(zone.name, text(zone.sea_zone_id, "")) || undefined,
                render: (zone) =>
                  text(zone.name, humanize(text(zone.sea_zone_id))),
              },
              {
                label: "Net zone piracy",
                value: (zone) => number(zone.net_piracy_bps),
                render: (zone) => percent(number(zone.net_piracy_bps)),
                className: styles.numeric,
              },
              {
                label: "Phase",
                value: (zone) => number(zone.phase),
                render: (zone) => count(number(zone.phase)),
                className: styles.numeric,
              },
              {
                label: "Base piracy",
                value: (zone) => number(zone.base_piracy_bps),
                render: (zone) => percent(number(zone.base_piracy_bps)),
                className: styles.numeric,
              },
              {
                label: "Marque piracy added",
                value: (zone) => number(zone.marque_piracy_bps),
                render: (zone) => percent(number(zone.marque_piracy_bps)),
                className: styles.numeric,
              },
              {
                label: "Active marques",
                value: (zone) => number(zone.active_marques),
                render: (zone) => count(number(zone.active_marques)),
                className: styles.numeric,
              },
              {
                label: "Patrol suppression",
                value: (zone) => number(zone.patrol_suppression_bps),
                render: (zone) => percent(number(zone.patrol_suppression_bps)),
                className: styles.numeric,
              },
              {
                label: "Active patrols",
                value: (zone) => number(zone.active_patrols),
                render: (zone) => count(number(zone.active_patrols)),
                className: styles.numeric,
              },
              {
                label: "Next marque expiry",
                value: (zone) =>
                  text(zone.next_marque_expires_at, "") || undefined,
                render: (zone) => dateTime(text(zone.next_marque_expires_at)),
              },
              {
                label: "Next patrol expiry",
                value: (zone) =>
                  text(zone.next_patrol_expires_at, "") || undefined,
                render: (zone) => dateTime(text(zone.next_patrol_expires_at)),
              },
            ]}
          />
        </Dataset>
      </Section>
      <details className={viewStyles.disclosure} open={convoys.length > 0}>
        <summary>
          <Ship size={17} strokeWidth={1.7} aria-hidden="true" /> Convoys
        </summary>
        <Section title="Convoys" icon={Ship} className={styles.section}>
          <p className={styles.note}>
            Recorded convoy visibility does not establish membership, ownership
            or an escort commitment.
          </p>
          <Dataset account={account} name="convoys">
            <DataTable
              label="Recorded visible convoy routes and membership"
              data={convoys}
              rowKey={(row, index) => text(row.convoy.id, String(index))}
              columns={[
                {
                  label: "Convoy",
                  value: (row) => text(row.convoy.name, "") || undefined,
                  render: (row) => (
                    <span className={styles.primary}>
                      {text(row.convoy.name)}
                      <small className={styles.identifier}>
                        {text(row.convoy.id)}
                      </small>
                    </span>
                  ),
                },
                {
                  label: "Status",
                  value: (row) => text(row.convoy.status, "") || undefined,
                  render: (row) => (
                    <StatusBadge status={text(row.convoy.status)} />
                  ),
                },
                {
                  label: "Origin",
                  value: (row) =>
                    text(row.convoy.origin_port_id, "") || undefined,
                  render: (row) => humanize(text(row.convoy.origin_port_id)),
                },
                {
                  label: "Destination",
                  value: (row) =>
                    text(row.convoy.dest_port_id, "") || undefined,
                  render: (row) => humanize(text(row.convoy.dest_port_id)),
                },
                {
                  label: "Visibility",
                  value: (row) => text(row.convoy.visibility, "") || undefined,
                  render: (row) => humanize(text(row.convoy.visibility)),
                },
                {
                  label: "Admission",
                  value: (row) => text(row.convoy.admission, "") || undefined,
                  render: (row) => humanize(text(row.convoy.admission)),
                },
                {
                  label: "Organizer company",
                  value: (row) =>
                    text(row.convoy.organizer_company_id, "") || undefined,
                  render: (row) => (
                    <span className={styles.identifier}>
                      {text(row.convoy.organizer_company_id)}
                    </span>
                  ),
                },
                {
                  label: "Members",
                  value: (row) =>
                    Array.isArray(row.entry.members)
                      ? row.entry.members.length
                      : undefined,
                  render: (row) =>
                    count(
                      Array.isArray(row.entry.members)
                        ? row.entry.members.length
                        : undefined,
                    ),
                  className: styles.numeric,
                },
                {
                  label: "Minimum speed",
                  value: (row) => number(row.convoy.min_speed_centiknots),
                  render: (row) => {
                    const speed = number(row.convoy.min_speed_centiknots);
                    return `${count(speed === undefined ? undefined : speed / 100)} kn`;
                  },
                  className: styles.numeric,
                },
                {
                  label: "Escort fee per hold ton",
                  value: (row) => number(row.convoy.escort_fee_pence_per_ton),
                  render: (row) =>
                    money(number(row.convoy.escort_fee_pence_per_ton)),
                  className: styles.numeric,
                },
                {
                  label: "Departs",
                  value: (row) => text(row.convoy.departs_at, "") || undefined,
                  render: (row) => dateTime(text(row.convoy.departs_at)),
                },
                {
                  label: "Leg arrival",
                  value: (row) =>
                    text(row.convoy.leg_arrives_at, "") || undefined,
                  render: (row) => dateTime(text(row.convoy.leg_arrives_at)),
                },
                {
                  label: "Current leg index",
                  value: (row) => number(row.convoy.current_leg_idx),
                  render: (row) => count(number(row.convoy.current_leg_idx)),
                  className: styles.numeric,
                },
              ]}
            />
          </Dataset>
        </Section>
      </details>
      <details className={viewStyles.disclosure} open={marques.length > 0}>
        <summary>
          <Flag size={17} strokeWidth={1.7} aria-hidden="true" /> Letters of
          marque
        </summary>
        <Section
          title="Letters of marque"
          icon={Flag}
          className={styles.section}
        >
          <Dataset account={account} name="marques">
            <DataTable
              label="Recorded letters of marque"
              data={marques}
              rowKey={(row, index) => text(row.id, String(index))}
              columns={[
                {
                  label: "Sea zone",
                  value: (row) => text(row.sea_zone_id, "") || undefined,
                  render: (row) => humanize(text(row.sea_zone_id)),
                },
                {
                  label: "Sponsor company",
                  value: (row) => text(row.sponsor_company_id, "") || undefined,
                  render: (row) => (
                    <span className={styles.identifier}>
                      {text(row.sponsor_company_id, "No sponsor recorded")}
                    </span>
                  ),
                },
                {
                  label: "Banking region",
                  value: (row) => text(row.banking_region, "") || undefined,
                  render: (row) => humanize(text(row.banking_region)),
                },
                {
                  label: "Cost paid",
                  value: (row) => number(row.cost_pence),
                  render: (row) => money(number(row.cost_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Added piracy",
                  value: (row) => number(row.added_piracy_bps),
                  render: (row) => percent(number(row.added_piracy_bps)),
                  className: styles.numeric,
                },
                {
                  label: "Prize earned",
                  value: (row) => number(row.prize_earned_pence),
                  render: (row) => money(number(row.prize_earned_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Expires",
                  value: (row) => text(row.expires_at, "") || undefined,
                  render: (row) => dateTime(text(row.expires_at)),
                },
                {
                  label: "Updated",
                  value: (row) => text(row.updated_at, "") || undefined,
                  render: (row) => dateTime(text(row.updated_at)),
                },
              ]}
            />
          </Dataset>
        </Section>
      </details>
      <details className={viewStyles.disclosure} open={patrols.length > 0}>
        <summary>
          <Shield size={17} strokeWidth={1.7} aria-hidden="true" /> Naval
          patrols
        </summary>
        <Section title="Naval patrols" icon={Shield} className={styles.section}>
          <Dataset account={account} name="patrols">
            <DataTable
              label="Recorded funded naval patrols"
              data={patrols}
              rowKey={(row, index) => text(row.id, String(index))}
              columns={[
                {
                  label: "Sea zone",
                  value: (row) => text(row.sea_zone_id, "") || undefined,
                  render: (row) => humanize(text(row.sea_zone_id)),
                },
                {
                  label: "Sponsor company",
                  value: (row) => text(row.sponsor_company_id, "") || undefined,
                  render: (row) => (
                    <span className={styles.identifier}>
                      {text(row.sponsor_company_id)}
                    </span>
                  ),
                },
                {
                  label: "Cost paid",
                  value: (row) => number(row.cost_pence),
                  render: (row) => money(number(row.cost_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Suppressed piracy",
                  value: (row) => number(row.suppressed_piracy_bps),
                  render: (row) => percent(number(row.suppressed_piracy_bps)),
                  className: styles.numeric,
                },
                {
                  label: "Expires",
                  value: (row) => text(row.expires_at, "") || undefined,
                  render: (row) => dateTime(text(row.expires_at)),
                },
                {
                  label: "Updated",
                  value: (row) => text(row.updated_at, "") || undefined,
                  render: (row) => dateTime(text(row.updated_at)),
                },
              ]}
            />
          </Dataset>
        </Section>
      </details>
      <details className={viewStyles.disclosure}>
        <summary>
          <Waves size={17} strokeWidth={1.7} aria-hidden="true" /> Hull and
          weather baselines
        </summary>
        <Section
          title="Hull and weather baselines"
          icon={Waves}
          className={styles.section}
        >
          <Dataset account={account} name="catalog">
            <DataTable
              label="Catalog sea-zone ratings and hazard baselines"
              data={catalog?.sea_zones ?? []}
              rowKey={(zone) => zone.id}
              columns={[
                {
                  label: "Sea zone",
                  value: (zone) => zone.name,
                  render: (zone) => zone.name,
                },
                {
                  label: "Hull rating",
                  value: (zone) => zone.zone_class,
                  render: (zone) => humanize(zone.zone_class),
                },
                {
                  label: "Phase",
                  value: (zone) => zone.phase,
                  render: (zone) => count(zone.phase),
                  className: styles.numeric,
                },
                {
                  label: "Base storm",
                  value: (zone) => zone.base_storm_bps,
                  render: (zone) => percent(zone.base_storm_bps),
                  className: styles.numeric,
                },
                {
                  label: "Base piracy",
                  value: (zone) => zone.base_piracy_bps,
                  render: (zone) => percent(zone.base_piracy_bps),
                  className: styles.numeric,
                },
              ]}
            />
            <p className={styles.note}>
              Catalog baselines, patrol suppression and marque additions
              describe recorded mechanics, not current voyage loss
              probabilities. Season, hull mismatch and world events affect
              actual risk.
            </p>
          </Dataset>
        </Section>
      </details>
    </div>
  );
}

function ExpansionTierDetails({ tier }: { tier: JsonRecord }) {
  return (
    <>
      <dl className={styles.details}>
        <div>
          <dt>Target tons per company</dt>
          <dd>{count(number(tier.tons_per_company))} t</dd>
        </div>
        <div>
          <dt>Minimum target</dt>
          <dd>{count(number(tier.min_tons))} t</dd>
        </div>
        <div>
          <dt>Companies at target calculation</dt>
          <dd>{count(number(tier.active_companies))}</dd>
        </div>
        <div>
          <dt>Remaining charter capacity</dt>
          <dd>{count(number(tier.charter_capacity_tons))} t</dd>
        </div>
      </dl>
      <h3 className={viewStyles.inline}>
        <FileText size={17} strokeWidth={1.7} aria-hidden="true" />
        Expansion charters
      </h3>
      <DataTable
        label="Expansion charter progress and rewards"
        data={rows(tier.charters)}
        rowKey={(charter, index) => text(charter.contract_id, String(index))}
        empty={
          Array.isArray(tier.charters)
            ? "No expansion charters recorded"
            : "Not recorded"
        }
        columns={[
          {
            label: "Port",
            value: (charter) => text(charter.port_id, "") || undefined,
            render: (charter) => humanize(text(charter.port_id)),
          },
          {
            label: "Commodity",
            value: (charter) => text(charter.commodity_id, "") || undefined,
            render: (charter) => humanize(text(charter.commodity_id)),
          },
          {
            label: "Status",
            value: (charter) => text(charter.status, "") || undefined,
            render: (charter) => <StatusBadge status={text(charter.status)} />,
          },
          {
            label: "Delivered / required",
            value: (charter) => number(charter.quantity_delivered),
            render: (charter) =>
              `${count(number(charter.quantity_delivered))} / ${count(number(charter.quantity_required))} t`,
          },
          {
            label: "Remaining",
            value: (charter) => number(charter.quantity_remaining),
            render: (charter) =>
              `${count(number(charter.quantity_remaining))} t`,
          },
          {
            label: "Progress",
            value: (charter) => number(charter.progress_bps),
            render: (charter) => percent(number(charter.progress_bps)),
          },
          {
            label: "Reward per ton",
            value: (charter) => number(charter.reward_per_ton_pence),
            render: (charter) => money(number(charter.reward_per_ton_pence)),
          },
          {
            label: "Expires",
            value: (charter) => text(charter.expires_at, "") || undefined,
            render: (charter) => dateTime(text(charter.expires_at)),
          },
          {
            label: "Charter details",
            render: (charter) => (
              <details>
                <summary>Terms and contribution</summary>
                <dl className={styles.details}>
                  <div>
                    <dt>Total delivered across reposts</dt>
                    <dd>{count(number(charter.tons_delivered_total))} t</dd>
                  </div>
                  <div>
                    <dt>Premium over catalog base price</dt>
                    <dd>{percent(number(charter.premium_bps))}</dd>
                  </div>
                  <div>
                    <dt>Beyond coastal water</dt>
                    <dd>
                      {charter.beyond_coastal_water === true
                        ? "Yes"
                        : charter.beyond_coastal_water === false
                          ? "No"
                          : "Not recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Rate escalations</dt>
                    <dd>{count(number(charter.escalations))}</dd>
                  </div>
                  <div>
                    <dt>Game hours remaining</dt>
                    <dd>{count(number(charter.game_hours_left))}</dd>
                  </div>
                  <div>
                    <dt>Contract ID</dt>
                    <dd className={styles.identifier}>
                      {text(charter.contract_id)}
                    </dd>
                  </div>
                </dl>
              </details>
            ),
          },
        ]}
      />
      <h3 className={viewStyles.inline}>
        <Users size={17} strokeWidth={1.7} aria-hidden="true" />
        Contributors
      </h3>
      <DataTable
        label="Expansion tier company contributions"
        data={rows(tier.contributors)}
        rowKey={(contributor, index) =>
          `${text(contributor.company_name, "")}:${index}`
        }
        empty={
          Array.isArray(tier.contributors)
            ? "No contributors recorded"
            : "Not recorded"
        }
        columns={[
          {
            label: "Company",
            value: (contributor) =>
              text(contributor.company_name, "") || undefined,
            render: (contributor) => text(contributor.company_name),
          },
          {
            label: "Delivered",
            value: (contributor) => number(contributor.tons),
            render: (contributor) => `${count(number(contributor.tons))} t`,
          },
          {
            label: "Contribution and bonus share",
            value: (contributor) => number(contributor.share_bps),
            render: (contributor) => percent(number(contributor.share_bps)),
          },
        ]}
      />
    </>
  );
}
