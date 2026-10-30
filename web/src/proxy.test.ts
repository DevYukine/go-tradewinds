import { NextRequest, NextResponse } from "next/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createAllowedHosts, isAllowedHost } from "./lib/allowed-hosts";
import type { proxy } from "./proxy";

const defaults = ["127.0.0.1:3400", "localhost:3400", "[::1]:3400"];

const deniedHosts = [
  null,
  undefined,
  "",
  "foreign.example:3400",
  "localhost",
  "localhost:3401",
  "127.0.0.1",
  "127.0.0.1:80",
  "[::1]",
  "[::1]:3401",
  "localhost.evil.example:3400",
  "localhost:3400.evil.example",
  "localhost:3400@evil.example",
  "user@localhost:3400",
  "http://localhost:3400",
  "https://localhost:3400",
  "localhost:3400/path",
  "localhost:3400,foreign.example:3400",
  "foreign.example:3400,localhost:3400",
  "::1:3400",
  " localhost:3400 ",
];

describe("exact dashboard authority allowlist", () => {
  it.each([undefined, ""])("retains defaults for configuration %s", (extra) => {
    const allowed = createAllowedHosts(extra);
    expect([...allowed]).toEqual(defaults);
    for (const host of defaults) {
      expect(isAllowedHost(host, allowed)).toBe(true);
    }
    expect(isAllowedHost("LOCALHOST:3400", allowed)).toBe(true);
  });

  it("normalizes configured CSV entries without changing authority matching", () => {
    const allowed = createAllowedHosts(
      " , Fleet.Example:443, , fleet.example:443, [2001:DB8::1]:3400, LOCALHOST:3400, ",
    );
    expect([...allowed]).toEqual([
      ...defaults,
      "fleet.example:443",
      "[2001:db8::1]:3400",
    ]);
    expect(isAllowedHost("FLEET.EXAMPLE:443", allowed)).toBe(true);
    expect(isAllowedHost("[2001:DB8::1]:3400", allowed)).toBe(true);
    expect(isAllowedHost("fleet.example", allowed)).toBe(false);
    expect(isAllowedHost("fleet.example:80", allowed)).toBe(false);
    expect(isAllowedHost("[2001:db8::1]", allowed)).toBe(false);
    expect(isAllowedHost("2001:db8::1:3400", allowed)).toBe(false);
    expect(isAllowedHost(" fleet.example:443 ", allowed)).toBe(false);
  });

  it.each(deniedHosts)("denies nonmatching Host %s", (host) => {
    expect(isAllowedHost(host, createAllowedHosts(undefined))).toBe(false);
  });
});

describe("dashboard proxy with real Next requests", () => {
  let runProxy: typeof proxy;

  beforeEach(async () => {
    vi.stubEnv("DASHBOARD_ALLOWED_HOSTS", " Fleet.Example:443 ");
    vi.resetModules();
    // Reload after stubbing env because the allowlist is captured at import.
    ({ proxy: runProxy } = await import("./proxy"));
  });

  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

  it.each([...defaults, "LOCALHOST:3400", "FLEET.EXAMPLE:443"])(
    "continues for the allowed Host %s",
    (host) => {
      const request = new NextRequest("http://foreign.example/ledger", {
        headers: { host },
      });
      const response = runProxy(request);
      expect(response).toBeInstanceOf(NextResponse);
      expect(response.status).toBe(200);
      expect(response.headers.get("x-middleware-next")).toBe("1");
    },
  );

  it.each([
    ["missing Host", undefined],
    ["empty Host", ""],
    ["foreign Host", "foreign.example:3400"],
    ["wrong port", "localhost:3401"],
    ["suffix attack", "localhost.evil.example:3400"],
    ["userinfo", "localhost:3400@evil.example"],
    ["scheme", "http://localhost:3400"],
    ["path", "localhost:3400/path"],
    ["joined authorities", "localhost:3400,foreign.example:3400"],
    ["unbracketed IPv6", "::1:3400"],
  ])("denies %s despite an allowed URL and forwarded Host", async (_, host) => {
    const headers = new Headers({ "X-Forwarded-Host": "localhost:3400" });
    if (host !== undefined) {
      headers.set("Host", host);
    }
    const request = new NextRequest("http://localhost:3400/ledger", {
      headers,
    });
    const response = runProxy(request);
    expect(response).toBeInstanceOf(NextResponse);
    expect(response.status).toBe(403);
    expect(response.headers.get("x-middleware-next")).toBeNull();
    expect(response.headers.get("content-type")).toBe(
      "text/plain; charset=utf-8",
    );
    expect(await response.text()).toBe("Forbidden host\n");
  });

  it("captures configured authorities once at proxy import", () => {
    vi.stubEnv("DASHBOARD_ALLOWED_HOSTS", "replacement.example:443");
    const configured = runProxy(
      new NextRequest("https://fleet.example/", {
        headers: { host: "fleet.example:443" },
      }),
    );
    const replacement = runProxy(
      new NextRequest("https://replacement.example/", {
        headers: { host: "replacement.example:443" },
      }),
    );
    expect(configured.headers.get("x-middleware-next")).toBe("1");
    expect(replacement.status).toBe(403);
  });
});
