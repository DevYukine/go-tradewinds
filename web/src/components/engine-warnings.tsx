"use client";

import { OctagonAlert, TriangleAlert } from "lucide-react";
import { useEffect, useRef } from "react";
import { useWarnings } from "@/lib/data";
import { dateTime, humanize } from "@/lib/format";
import type { Account } from "@/lib/types";
import { warningText, warningTime } from "@/lib/warnings";
import styles from "./engine-warnings.module.css";

export function EngineWarnings({
  accounts,
  accountsLoaded,
  overview = false,
}: {
  accounts: Pick<Account, "id" | "label" | "enabled" | "company_id">[];
  accountsLoaded: boolean;
  overview?: boolean;
}) {
  const query = useWarnings();
  const report = query.data;
  const unmonitored = new Map(
    (report?.unmonitored ?? []).map((account) => [
      account.account_id,
      account.reason,
    ]),
  );
  const assessments = accounts
    .filter(
      (account) =>
        account.enabled &&
        account.company_id !== "" &&
        !unmonitored.has(account.id),
    )
    .map((account) => ({
      account,
      assessment: report?.accounts.find(
        (result) => result.account_id === account.id,
      ),
    }));
  const warnings = assessments.flatMap(({ assessment }) =>
    assessment
      ? assessment.warnings.map((warning) => ({
          account: assessment,
          warning,
        }))
      : [],
  );
  const hasWarnings = warnings.length > 0;
  const hasMonitoredAccounts = assessments.length > 0;
  const fullyAssessed =
    Boolean(report) &&
    hasMonitoredAccounts &&
    assessments.every(
      ({ assessment }) =>
        assessment?.available && assessment.suppressed.length === 0,
    );
  const warningSet = JSON.stringify(
    warnings
      .map(({ account, warning }) => `${account.account_id}:${warning.code}`)
      .sort(),
  );
  const lastWarningSet = useRef("[]");
  const announcer = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!announcer.current || lastWarningSet.current === warningSet) return;
    lastWarningSet.current = warningSet;
    // React leaves this element empty; only changes to warning identities
    // update its live text, never ticking facts, check times, or build details.
    announcer.current.textContent = hasWarnings
      ? `Engine warnings changed. ${warnings
          .map(
            ({ account, warning }) =>
              `${accounts.length > 1 ? `${account.label}: ` : ""}${humanize(warning.code)}.`,
          )
          .join(
            " ",
          )}${fullyAssessed ? "" : " Some warning assessments are incomplete; see Overview for details."}`
      : fullyAssessed
        ? "Engine warnings cleared for the assessed accounts."
        : hasMonitoredAccounts
          ? "Engine warning assessment is incomplete. See Overview for assessment details."
          : "No accounts are monitored in the selected scope.";
  }, [
    warningSet,
    warnings,
    hasWarnings,
    fullyAssessed,
    hasMonitoredAccounts,
    accounts.length,
  ]);

  const showStrip =
    overview ||
    hasWarnings ||
    (hasMonitoredAccounts && query.isError) ||
    (Boolean(report) &&
      assessments.some(({ assessment }) => !assessment?.available));

  return (
    <>
      <span
        ref={announcer}
        className="sr-only"
        role="status"
        aria-label="Engine warning changes"
        aria-atomic="true"
      />
      {showStrip && (
        <div className={styles.strip}>
          {warnings.map(({ account, warning }) => {
            const critical = warning.severity === "critical";
            const Icon = critical ? OctagonAlert : TriangleAlert;
            return (
              <p
                key={`${account.account_id}:${warning.code}`}
                className={`notice ${critical ? "notice-danger" : styles.warning} ${styles.line}`}
              >
                <Icon size={17} strokeWidth={1.7} aria-hidden="true" />
                <span>
                  <span className="sr-only">
                    {critical ? "Critical: " : "Warning: "}
                  </span>
                  {warningText(
                    warning,
                    accounts.length > 1 ? account.label : undefined,
                  )}
                </span>
              </p>
            );
          })}
          {overview && hasMonitoredAccounts && !report && !query.isError && (
            <p className={styles.quiet}>
              Engine warnings have not been assessed yet.
            </p>
          )}
          {accountsLoaded && overview && accounts.length === 0 && (
            <p className={styles.quiet}>
              Engine warnings not assessed: no accounts are in scope.
            </p>
          )}
          {overview &&
            accounts
              .filter(
                (account) =>
                  !account.enabled ||
                  account.company_id === "" ||
                  unmonitored.has(account.id),
              )
              .map((account) => (
                <p key={`${account.id}:not-monitored`} className={styles.quiet}>
                  {account.label}: not monitored
                  {unmonitored.get(account.id) === "no_api_key"
                    ? " (no API key)"
                    : ""}
                  .
                </p>
              ))}
          {report &&
            assessments.map(({ account, assessment }) => {
              if (
                assessment?.available &&
                (!overview || assessment.suppressed.length === 0)
              ) {
                return null;
              }
              return (
                <p key={`${account.id}:assessment`} className={styles.quiet}>
                  {accounts.length > 1 && `${account.label}: `}
                  {assessment?.available ? (
                    <>
                      Some engine warnings could not be evaluated from recorded
                      data: {assessment.suppressed.map(humanize).join(", ")}.
                    </>
                  ) : (
                    <>
                      Engine warnings unavailable.
                      {!assessment
                        ? " No warning evaluation was returned for this account."
                        : assessment.error_code &&
                          ` ${humanize(assessment.error_code)}.`}
                    </>
                  )}
                </p>
              );
            })}
          {report && overview && (
            <p className={styles.quiet}>
              {!hasWarnings && fullyAssessed && (
                <>
                  No engine warnings from recorded data, checked{" "}
                  <time
                    dateTime={report.generated_at}
                    title={dateTime(report.generated_at)}
                  >
                    {warningTime(report.generated_at)}
                  </time>
                  .{" "}
                </>
              )}
              Build {report.build.revision?.slice(0, 7) ?? "unknown revision"}
              {report.build.modified && " (modified)"} running since{" "}
              <time
                dateTime={report.build.started_at}
                title={dateTime(report.build.started_at)}
              >
                {warningTime(report.build.started_at)}
              </time>
              .
            </p>
          )}
          {query.isError && hasMonitoredAccounts && (
            <p className={styles.quiet}>
              {report
                ? "Engine warnings could not be updated. Last recorded results are retained."
                : "Engine warnings unavailable."}
            </p>
          )}
        </div>
      )}
    </>
  );
}
