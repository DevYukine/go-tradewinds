"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useNow } from "@/app/providers";
import { dataset, useBulletinBoard, useBulletinThread } from "@/lib/data";
import { age, count, dateTime, humanize } from "@/lib/format";
import type { Account, CompanyProfile } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { EmptyState } from "./ui";

export function bulletinCompanies(accounts: Account[]): Set<string> {
  return new Set(
    accounts
      .map(
        (account) => dataset<CompanyProfile>(account, "profile")?.company?.name,
      )
      .filter((name): name is string => Boolean(name)),
  );
}

function BulletinTime({ value, now }: { value: string; now: number }) {
  return (
    <time dateTime={value} title={`${dateTime(value)} (${value})`}>
      {age(value, now)}
    </time>
  );
}

export function BulletinView({ accounts }: { accounts: Account[] }) {
  const now = useNow();
  const selectedID = useUI((state) => state.selectedBulletinThreadID);
  const selectThread = useUI((state) => state.selectBulletinThread);
  const search = useUI((state) => state.bulletinSearch);
  const company = useUI((state) => state.bulletinCompany);
  const setSearch = useUI((state) => state.setBulletinSearch);
  const setCompany = useUI((state) => state.setBulletinCompany);
  const [debouncedSearch, setDebouncedSearch] = useState(search);
  const searchRef = useRef<HTMLInputElement>(null);
  const readerRef = useRef<HTMLElement>(null);
  const ownCompanies = useMemo(() => bulletinCompanies(accounts), [accounts]);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedSearch(search), 300);
    return () => window.clearTimeout(timer);
  }, [search]);

  const board = useBulletinBoard({ q: debouncedSearch, company, limit: 500 });
  const detail = useBulletinThread(selectedID);
  // Previous query data can belong to another selection while its detail loads.
  const threadView =
    detail.data?.thread.id === selectedID ? detail.data : undefined;
  const thread = threadView?.thread;
  const link = thread?.link;
  const filtered = Boolean(debouncedSearch || company);
  const stale = Boolean(
    board.data?.synced_at &&
    now &&
    now - Date.parse(board.data.synced_at) > 30 * 60_000,
  );

  return (
    <div className="bulletin-view">
      <div className="bulletin-toolbar">
        <label className="bulletin-search">
          <span>Search bulletin</span>
          <input
            ref={searchRef}
            type="search"
            value={search}
            maxLength={200}
            placeholder="Title, company or post text"
            onChange={(event) => setSearch(event.target.value)}
          />
        </label>
        <label className="bulletin-company">
          <span>Company</span>
          <select
            value={company}
            onChange={(event) => setCompany(event.target.value)}
          >
            <option value="">All companies</option>
            {company && !board.data?.companies.includes(company) && (
              <option value={company} dir="auto">
                {company}
              </option>
            )}
            {board.data?.companies.map((name) => (
              <option key={name} value={name} dir="auto">
                {name}
              </option>
            ))}
          </select>
        </label>
        <button
          className="button button-quiet"
          type="button"
          disabled={!search && !company}
          onClick={() => {
            setSearch("");
            setDebouncedSearch("");
            setCompany("");
          }}
        >
          Clear filters
        </button>
      </div>
      {board.data?.companies_truncated && (
        <p className="notice">
          The company filter lists the first 1,000 names. Use search to find
          threads from other companies.
        </p>
      )}
      {board.isError && (
        <div className="notice notice-danger" role="alert">
          <strong>
            {board.data
              ? "Bulletin updates interrupted."
              : "Recording service unavailable."}
          </strong>{" "}
          {board.error.message}{" "}
          {board.data
            ? "Last recorded threads remain visible. Reconnection is automatic."
            : "Check the Go service and its database connection. This view reads recorded posts only."}
        </div>
      )}
      {board.data && (
        <p className={`bulletin-freshness ${stale ? "notice" : "muted"}`}>
          {stale ? "Bulletin records are stale. " : ""}
          {board.data.synced_at ? (
            <>
              Board last synced{" "}
              <BulletinTime value={board.data.synced_at} now={now} />.{" "}
            </>
          ) : (
            "No board sync recorded. "
          )}
          The board is shared across all accounts.
        </p>
      )}
      <div className="bulletin-panes" data-selected={Boolean(selectedID)}>
        <section
          className="bulletin-index"
          aria-label="Bulletin threads"
          aria-busy={board.isFetching}
        >
          <p className="bulletin-list-status muted" role="status">
            {board.isPending
              ? "Loading recorded bulletin threads..."
              : board.data
                ? `${count(board.data.threads.length)} of ${count(board.data.total)} threads`
                : "Threads unavailable"}
          </p>
          {board.data?.threads.length === 0 && (
            <EmptyState
              title={
                filtered
                  ? "No matching threads"
                  : "No bulletin posts recorded yet."
              }
              description={
                filtered
                  ? "Clear the filters or search for another title, company or phrase."
                  : "The bot records the board while it runs."
              }
            />
          )}
          <ul className="bulletin-thread-list">
            {board.data?.threads.map((item) => (
              <li key={item.id}>
                <button
                  type="button"
                  className="bulletin-thread-row"
                  aria-current={selectedID === item.id ? "true" : undefined}
                  aria-controls="bulletin-reader"
                  onClick={() => {
                    selectThread(item.id);
                    if (window.matchMedia("(max-width: 1100px)").matches) {
                      readerRef.current?.focus();
                    }
                  }}
                >
                  <strong>
                    <bdi>{item.title || "Title not recorded"}</bdi>
                  </strong>
                  <span className="bulletin-author">
                    <bdi>{item.company_name || "Company not recorded"}</bdi>
                    {ownCompanies.has(item.company_name) && (
                      <span className="bulletin-own">Our company</span>
                    )}
                  </span>
                  <span className="bulletin-row-meta">
                    <BulletinTime value={item.last_post_at} now={now} />
                    <span>
                      {count(item.replies)}{" "}
                      {item.replies === 1 ? "reply" : "replies"}
                    </span>
                    {item.archived && <span>Archived</span>}
                    {item.removed && <span>Removed</span>}
                  </span>
                </button>
              </li>
            ))}
          </ul>
          {board.data && board.data.total > board.data.threads.length && (
            <p className="bulletin-list-status muted">
              Showing the newest 500 matching threads. Search or filter by
              company to find older posts.
            </p>
          )}
        </section>
        <section
          id="bulletin-reader"
          className="bulletin-reader"
          ref={readerRef}
          tabIndex={-1}
          aria-label="Selected bulletin thread"
          aria-busy={Boolean(selectedID) && detail.isFetching}
        >
          {!selectedID ? (
            <EmptyState
              title="Choose a thread"
              description="Read opening posts and replies from every company, in the order they were posted."
            />
          ) : (
            <>
              <button
                type="button"
                className="button button-quiet bulletin-back"
                onClick={() => {
                  selectThread("");
                  searchRef.current?.focus();
                }}
              >
                Back to threads
              </button>
              {detail.isError && (
                <div className="notice notice-danger" role="alert">
                  <strong>Thread unavailable.</strong> {detail.error.message}{" "}
                  {threadView
                    ? "Last recorded posts remain visible."
                    : "The thread may not have been recorded yet. Updates retry automatically."}
                </div>
              )}
              {!threadView && !detail.isError && (
                <p className="muted" role="status">
                  Loading recorded posts...
                </p>
              )}
              {thread && threadView && (
                <>
                  <header className="bulletin-thread-heading">
                    <h2>
                      <bdi>{thread.title || "Title not recorded"}</bdi>
                    </h2>
                    {link?.type === "company" &&
                      typeof link.name === "string" && (
                        <p>
                          About company: <bdi>{link.name}</bdi>
                        </p>
                      )}
                    {link?.type === "exchange" &&
                      typeof link.kind === "string" && (
                        <p>Exchange entry: {humanize(link.kind)}</p>
                      )}
                    <p className="muted">
                      {count(thread.replies)}{" "}
                      {thread.replies === 1 ? "reply" : "replies"}
                      {thread.archived ? " / Archived thread" : ""}
                      {thread.removed ? " / Thread removed" : ""}
                    </p>
                    <p className="muted">
                      {thread.detail_synced_at ? (
                        <>
                          Posts last synced{" "}
                          <BulletinTime
                            value={thread.detail_synced_at}
                            now={now}
                          />
                          .
                        </>
                      ) : (
                        "Thread detail has not yet been synced."
                      )}
                    </p>
                  </header>
                  {!thread.detail_current && (
                    <p className="notice" role="status">
                      Thread not yet fully synced:{" "}
                      {count(thread.posts_recorded)}
                      {thread.posts_recorded < thread.replies + 1
                        ? ` of ${count(thread.replies + 1)}`
                        : ""}{" "}
                      posts recorded. The bot will refresh this thread as it
                      runs.
                    </p>
                  )}
                  {threadView.posts_truncated && (
                    <p className="notice">
                      Showing the newest {count(threadView.posts.length)} of{" "}
                      {count(threadView.posts_total)} recorded posts. Older
                      posts are retained in the database but are not shown here.
                    </p>
                  )}
                  {threadView.posts.length === 0 && (
                    <p className="muted">
                      No posts recorded for this thread yet.
                    </p>
                  )}
                  <ol className="bulletin-posts">
                    {threadView.posts.map((post) => (
                      <li key={post.id}>
                        <article
                          className={`bulletin-post${post.is_opening ? " is-opening" : ""}`}
                        >
                          <header className="bulletin-post-meta">
                            <div className="bulletin-author">
                              <h3>
                                <bdi>
                                  {post.company_name || "Company not recorded"}
                                </bdi>
                              </h3>
                              {ownCompanies.has(post.company_name) && (
                                <span className="bulletin-own">
                                  Our company
                                </span>
                              )}
                              {post.is_opening && (
                                <span className="muted">Opening post</span>
                              )}
                            </div>
                            <div className="bulletin-row-meta">
                              <span>
                                Posted{" "}
                                <BulletinTime
                                  value={post.created_at}
                                  now={now}
                                />
                              </span>
                              {post.edited_at && (
                                <span>
                                  Edited{" "}
                                  <BulletinTime
                                    value={post.edited_at}
                                    now={now}
                                  />
                                </span>
                              )}
                            </div>
                          </header>
                          {post.removed && (
                            <p className="bulletin-post-notice muted">
                              This post was removed.
                            </p>
                          )}
                          {post.deleted && (
                            <p className="bulletin-post-notice muted">
                              This post was deleted.
                            </p>
                          )}
                          {!post.removed && !post.deleted && (
                            <p className="bulletin-post-body">
                              {post.body || "No post text recorded."}
                            </p>
                          )}
                        </article>
                      </li>
                    ))}
                  </ol>
                </>
              )}
            </>
          )}
        </section>
      </div>
    </div>
  );
}
