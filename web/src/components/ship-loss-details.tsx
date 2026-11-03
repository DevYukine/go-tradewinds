import { dateTime, humanize, money, quantity } from "@/lib/format";
import {
  cargoEntries,
  catalogPlaceName,
  shipRecordKind,
  shipStatusLabel,
} from "@/lib/operations";
import type { Account, Catalog, ShipDetail } from "@/lib/types";

export function ShipLossHistoryNotice({
  account,
  className,
}: {
  account: Account;
  className?: string;
}) {
  if (!account.ship_losses_unavailable && account.ship_losses !== undefined) {
    return null;
  }
  return (
    <p className={className} role="status">
      {account.label}: Ship loss and exit history{" "}
      {account.ship_losses_unavailable ? "unavailable" : "not recorded"};
      retained vessels may be incomplete.
    </p>
  );
}

export function ShipLossDetails({
  detail,
  catalog,
  historyUnavailable = false,
  className,
  factsClassName,
  noteClassName,
}: {
  detail: ShipDetail;
  catalog?: Catalog;
  historyUnavailable?: boolean;
  className: string;
  factsClassName: string;
  noteClassName: string;
}) {
  const gone = detail.ship.status === "sunk";
  const loss = detail.loss;
  if (!loss && !gone) return null;
  const pastAbsence = !gone && Boolean(loss?.recovered_at);
  const kind = pastAbsence
    ? "absence"
    : shipRecordKind(
        loss?.status ?? detail.ship.status,
        loss?.fate ?? detail.ship.fate,
      );
  const recordLabel = `${kind} record`;
  const firstObservedLabel = `First observed ${kind}`;
  const manifest = cargoEntries(loss?.cargo_manifest);
  const lastCargo = manifest
    ?.map(
      ([commodity, tons]) =>
        `${quantity(tons, "t")} ${catalog?.commodities.find((item) => item.id === commodity)?.name ?? humanize(commodity)}`,
    )
    .join(", ");

  return (
    <section className={className}>
      <h3>
        {pastAbsence
          ? "Past absence record"
          : gone
            ? humanize(recordLabel)
            : `Historical ${recordLabel}`}
      </h3>
      {loss ? (
        <>
          {pastAbsence ? (
            <p className={noteClassName}>
              A later afloat observation resolved this absence. The ship is in
              the fleet again.
            </p>
          ) : !gone ? (
            <p className={noteClassName}>
              A newer afloat observation overrides this historical {recordLabel}
              .
              {kind === "loss"
                ? " The ship is not currently recorded as lost."
                : " The ship is in the fleet again."}
            </p>
          ) : null}
          <dl className={factsClassName}>
            <div>
              <dt>{pastAbsence ? "Past observation" : "Recorded fate"}</dt>
              <dd>{shipStatusLabel(loss.status, loss.fate)}</dd>
            </div>
            <div>
              <dt>{firstObservedLabel}</dt>
              <dd>
                <time dateTime={loss.observed_at}>
                  {dateTime(loss.observed_at)}
                </time>
              </dd>
            </div>
            <div>
              <dt>Last observed afloat</dt>
              <dd>
                {loss.last_observed_at ? (
                  <time dateTime={loss.last_observed_at}>
                    {dateTime(loss.last_observed_at)}
                  </time>
                ) : (
                  "Not recorded"
                )}
              </dd>
            </div>
            {pastAbsence && loss.recovered_at && (
              <div>
                <dt>Observed afloat again</dt>
                <dd>
                  <time dateTime={loss.recovered_at}>
                    {dateTime(loss.recovered_at)}
                  </time>
                </dd>
              </div>
            )}
            <div>
              <dt>Last afloat port</dt>
              <dd>{catalogPlaceName(catalog, loss.last_port_id)}</dd>
            </div>
            <div>
              <dt>Last afloat sea zone</dt>
              <dd>{catalogPlaceName(catalog, loss.last_sea_zone_id)}</dd>
            </div>
            <div>
              <dt>Voyage origin</dt>
              <dd>{catalogPlaceName(catalog, loss.voyage_origin_id)}</dd>
            </div>
            <div>
              <dt>Voyage destination</dt>
              <dd>{catalogPlaceName(catalog, loss.voyage_destination_id)}</dd>
            </div>
            <div>
              <dt>Hazard cause</dt>
              <dd>
                {loss.hazard_cause
                  ? humanize(loss.hazard_cause)
                  : loss.hazard_unavailable_reason
                    ? `Unavailable: ${loss.hazard_unavailable_reason}`
                    : "Not recorded"}
              </dd>
            </div>
            <div>
              <dt>Last afloat cargo</dt>
              <dd>
                {lastCargo === undefined
                  ? loss.cargo_unavailable_reason
                    ? `Unavailable: ${loss.cargo_unavailable_reason}`
                    : "Not recorded"
                  : lastCargo || "No trade cargo recorded"}
              </dd>
            </div>
            <div>
              <dt>Last afloat stores</dt>
              <dd>
                {loss.stores
                  ? `${quantity(loss.stores.provisions_tons, "t provisions")}; ${quantity(loss.stores.sea_days_remaining, "game days at sea")}`
                  : loss.stores_unavailable_reason
                    ? `Unavailable: ${loss.stores_unavailable_reason}`
                    : "Not recorded"}
              </dd>
            </div>
            <div>
              <dt>Game-reported insurance claim</dt>
              <dd>{money(loss.insurance_claim_paid_pence)}</dd>
            </div>
          </dl>
          <p className={noteClassName}>
            {firstObservedLabel} is when the {kind} was recorded, not an exact{" "}
            {kind} time.{" "}
            {kind === "loss"
              ? "Cargo and stores describe the last afloat observation, not confirmed losses."
              : "Cargo and stores describe the last afloat observation."}
          </p>
        </>
      ) : (
        <p className={noteClassName}>
          Detailed {kind} evidence{" "}
          {historyUnavailable ? "unavailable" : "not recorded"}.
        </p>
      )}
    </section>
  );
}
