// @vitest-environment jsdom

import { act, createElement, useSyncExternalStore } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ExtractState } from "zustand";
import type * as Data from "@/lib/data";
import type { Account, BulletinBoard, BulletinThreadView } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import type * as UI from "@/lib/ui-store";
import { bulletinCompanies, BulletinView } from "./bulletin";

const state = vi.hoisted(() => ({
  board: undefined as BulletinBoard | undefined,
  detail: undefined as BulletinThreadView | undefined,
  isPending: false,
  isError: false,
}));

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useBulletinBoard: () => {
    return {
      data: state.board,
      isPending: state.isPending,
      isError: state.isError,
      isFetching: false,
      error: new Error("Service unavailable"),
    };
  },
  useBulletinThread: () => ({
    data: state.detail,
    isError: false,
    isFetching: false,
  }),
}));
vi.mock("@/lib/ui-store", async (original) => {
  const store = await original<typeof UI>();
  return {
    useUI: Object.assign(
      (select: (value: ExtractState<typeof store.useUI>) => unknown) =>
        select(
          useSyncExternalStore(
            store.useUI.subscribe,
            store.useUI.getState,
            store.useUI.getState,
          ),
        ),
      store.useUI,
    ),
  };
});
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-11-01T12:00:00Z"),
}));

function account(id: string, name?: string): Account {
  return {
    id,
    label: "Account label is not a company name",
    company_id: id,
    enabled: true,
    created_at: "2026-11-01T10:00:00Z",
    snapshot: name ? { profile: { company: { name } } } : {},
  };
}

function render(accounts: Account[] = [account("a", "Our traders")]) {
  return renderToStaticMarkup(createElement(BulletinView, { accounts }));
}

let root: Root | undefined;
let container: HTMLDivElement;

async function renderClient() {
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
  await refreshClient();
}

async function refreshClient() {
  await act(async () => {
    root!.render(
      createElement(BulletinView, { accounts: [account("a", "Our traders")] }),
    );
  });
}

afterEach(async () => {
  if (root) {
    await act(async () => root!.unmount());
    root = undefined;
    container.remove();
  }
  vi.unstubAllGlobals();
});

beforeEach(() => {
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  useUI.setState({
    selectedBulletinThreadID: "thread-one",
    bulletinSearch: "",
    bulletinCompany: "",
  });
  state.isPending = false;
  state.isError = false;
  const thread = {
    id: "thread-one",
    title: "Seeking timber",
    company_name: "Our traders",
    link: { type: "company", name: "Northern traders" },
    created_at: "2026-11-01T09:00:00Z",
    last_post_at: "2026-11-01T10:00:00Z",
    replies: 2,
    posts_recorded: 2,
    archived: true,
    removed: false,
    detail_synced_at: "2026-11-01T10:05:00Z",
    detail_current: false,
  };
  state.board = {
    threads: [thread],
    total: 1,
    companies: ["Northern traders", "Our traders"],
    companies_truncated: false,
    synced_at: "2026-11-01T10:05:00Z",
  };
  state.detail = {
    thread,
    posts_truncated: false,
    posts_total: 2,
    posts: [
      {
        id: "opening",
        company_name: "Our traders",
        body: "<script>alert('unsafe')</script>\nSecond line",
        is_opening: true,
        created_at: "2026-11-01T09:00:00Z",
        edited_at: "2026-11-01T09:05:00Z",
        removed: false,
        deleted: false,
      },
      {
        id: "reply",
        company_name: "Northern traders",
        body: "Deleted text must not be displayed",
        is_opening: false,
        created_at: "2026-11-01T10:00:00Z",
        edited_at: null,
        removed: false,
        deleted: true,
      },
    ],
  };
});

describe("bulletin company ownership", () => {
  it("uses every recorded company profile and excludes missing names and account labels", () => {
    const companies = bulletinCompanies([
      account("one", "Our traders"),
      account("two", "Second fleet"),
      account("three"),
      account("four", "Our traders"),
      account("five", ""),
    ]);
    expect([...companies]).toEqual(["Our traders", "Second fleet"]);
    expect(companies.has("our traders")).toBe(false);
    expect(companies.has("Account label is not a company name")).toBe(false);
  });
});

describe("recorded bulletin reading", () => {
  it("escapes game text, preserves post order and discloses ownership, edits and missing posts", () => {
    const html = render();
    expect(html).toContain("&lt;script&gt;");
    expect(html).not.toContain("<script>");
    expect(html).toContain("\nSecond line");
    expect(html).toContain("Our company");
    expect(html).toContain("Opening post");
    expect(html).toContain("Edited");
    expect(html).toContain("About company:");
    expect(html).toContain("<bdi>Northern traders</bdi>");
    expect(html).toContain("Archived thread");
    expect(html).toContain("2 of 3 posts recorded");
    expect(html).toContain("Bulletin records are stale");
    expect(html).toContain('title="01 Nov, 10:00 UTC (2026-11-01T10:00:00Z)"');
    expect(html.indexOf("Second line")).toBeLessThan(
      html.indexOf("This post was deleted"),
    );
    expect(html).not.toContain("Deleted text must not be displayed");
  });

  it("shows pending thread updates even when every expected post is recorded", () => {
    state.detail!.thread.posts_recorded = 3;
    const html = render();
    expect(html).toContain("Thread not yet fully synced");
    expect(html).toContain("3 posts recorded");
  });

  it("uses authoritative detail freshness instead of counting stored posts", () => {
    state.detail!.thread.detail_current = true;
    state.board!.synced_at = "2026-11-01T11:50:00Z";
    const html = render();
    expect(html).not.toContain("Thread not yet fully synced");
    expect(html).not.toContain("Bulletin records are stale");
    expect(html).toContain("Board last synced");
  });

  it("renders a removed post as a muted notice without its recorded body", () => {
    state.detail!.posts[1].deleted = false;
    state.detail!.posts[1].removed = true;
    const html = render();
    expect(html).toContain("This post was removed.");
    expect(html).not.toContain("Deleted text must not be displayed");
  });

  it("keeps the selected thread and filters when recorded data refreshes", async () => {
    useUI.setState({
      bulletinSearch: "timber",
      bulletinCompany: "Our traders",
    });
    await renderClient();
    expect(
      container.querySelector('[aria-current="true"]')?.textContent,
    ).toContain("Seeking timber");

    state.board = {
      ...state.board!,
      threads: [
        {
          ...state.board!.threads[0],
          id: "thread-two",
          title: "New timber offer",
        },
        { ...state.board!.threads[0], replies: 3 },
      ],
      total: 2,
      companies: ["Northern traders"],
      synced_at: "2026-11-01T11:59:00Z",
    };
    await refreshClient();

    expect(useUI.getState().selectedBulletinThreadID).toBe("thread-one");
    expect(useUI.getState().bulletinSearch).toBe("timber");
    expect(useUI.getState().bulletinCompany).toBe("Our traders");
    expect(
      container.querySelector<HTMLInputElement>('input[type="search"]')?.value,
    ).toBe("timber");
    expect(container.querySelector("select")?.value).toBe("Our traders");
    expect(
      container.querySelector('[aria-current="true"]')?.textContent,
    ).toContain("Seeking timber");
    expect(container.querySelector("#bulletin-reader h2")?.textContent).toBe(
      "Seeking timber",
    );
    expect(container.querySelector("#bulletin-reader")?.textContent).toContain(
      "Second line",
    );
  });

  it("does not present previous thread detail as a new selection", async () => {
    vi.stubGlobal("matchMedia", () => ({ matches: false }));
    state.board!.threads.push({
      ...state.board!.threads[0],
      id: "thread-two",
      title: "New timber offer",
    });
    await renderClient();
    expect(container.querySelector("#bulletin-reader")?.textContent).toContain(
      "Second line",
    );
    await act(async () => {
      container
        .querySelectorAll<HTMLButtonElement>(".bulletin-thread-row")[1]
        .click();
    });
    state.board = {
      ...state.board!,
      threads: state.board!.threads.map((thread) => ({ ...thread })),
      synced_at: "2026-11-01T11:59:00Z",
    };
    await refreshClient();

    expect(useUI.getState().selectedBulletinThreadID).toBe("thread-two");
    expect(
      container.querySelector('[aria-current="true"]')?.textContent,
    ).toContain("New timber offer");
    const reader = container.querySelector("#bulletin-reader");
    expect(reader?.textContent).toContain("Loading recorded posts");
    expect(reader?.textContent).not.toContain("Second line");
    expect(reader?.textContent).not.toContain("Seeking timber");
    expect(reader?.querySelector("h2")).toBeNull();

    state.detail = {
      ...state.detail!,
      thread: state.board.threads[1],
      posts: [{ ...state.detail!.posts[0], body: "Fresh thread detail" }],
    };
    await refreshClient();
    expect(reader?.querySelector("h2")?.textContent).toBe("New timber offer");
    expect(reader?.textContent).toContain("Fresh thread detail");
    expect(reader?.textContent).not.toContain("Second line");
  });

  it("discloses when only the newest recorded posts are returned", () => {
    state.detail!.posts_truncated = true;
    state.detail!.posts_total = 1100;
    const html = render();
    expect(html).toContain("Showing the newest 2 of 1,100 recorded posts");
    expect(html).toContain("Older posts are retained in the database");
  });

  it("explains how to find companies outside a limited filter list", () => {
    state.board!.companies_truncated = true;
    const html = render();
    expect(html).toContain("The company filter lists the first 1,000 names");
    expect(html).toContain("Use search to find threads from other companies");
  });

  it("distinguishes an empty mirror from loading and a service failure", () => {
    state.board = undefined;
    state.isPending = true;
    expect(render()).toContain("Loading recorded bulletin threads");
    state.isPending = false;
    state.isError = true;
    expect(render()).toContain("Recording service unavailable");
    state.isError = false;
    state.board = {
      threads: [],
      total: 0,
      companies: [],
      companies_truncated: false,
      synced_at: null,
    };
    const html = render();
    expect(html).toContain("No bulletin posts recorded yet.");
    expect(html).toContain("The bot records the board while it runs.");
  });
});
