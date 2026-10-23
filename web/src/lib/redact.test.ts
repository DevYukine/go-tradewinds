import { describe, expect, it } from "vitest";
import { formatChildFailure, redactUrlCredentials } from "./redact";

describe("redactUrlCredentials", () => {
  it("redacts a full username and password", () => {
    expect(
      redactUrlCredentials(
        "Failed to proxy http://alice:secret@127.0.0.1:8585/api/dashboard",
      ),
    ).toBe("Failed to proxy http://[redacted]@127.0.0.1:8585/api/dashboard");
  });

  it("redacts username-only userinfo", () => {
    expect(redactUrlCredentials("http://alice@127.0.0.1:8585/x")).toBe(
      "http://[redacted]@127.0.0.1:8585/x",
    );
  });

  it("redacts password-only userinfo", () => {
    expect(redactUrlCredentials("http://:secret@127.0.0.1:8585/x")).toBe(
      "http://[redacted]@127.0.0.1:8585/x",
    );
  });

  it("redacts empty userinfo", () => {
    expect(redactUrlCredentials("http://@127.0.0.1:8585/x")).toBe(
      "http://[redacted]@127.0.0.1:8585/x",
    );
  });

  it("redacts through the last at-sign when the password contains one", () => {
    expect(redactUrlCredentials("http://u:p@tail@host/x")).toBe(
      "http://[redacted]@host/x",
    );
  });

  it("redacts every occurrence and keeps credential-free text unchanged", () => {
    const mixed =
      "a http://u:p@one.test/x b https://two.test/y c postgres://bob:pw@three.test/d";
    expect(redactUrlCredentials(mixed)).toBe(
      "a http://[redacted]@one.test/x b https://two.test/y c postgres://[redacted]@three.test/d",
    );
  });

  it("leaves URLs whose at-sign appears after a path or space untouched", () => {
    const plain =
      "http://127.0.0.1:8585/api/dashboard and mail at bob@example.com plus http://host/x@y";
    expect(redactUrlCredentials(plain)).toBe(plain);
  });

  it("preserves host, port, path and surrounding error context", () => {
    const line =
      "Error: connect ECONNREFUSED http://user:pass@db.internal:5432/store (loopback proxy)";
    expect(redactUrlCredentials(line)).toBe(
      "Error: connect ECONNREFUSED http://[redacted]@db.internal:5432/store (loopback proxy)",
    );
  });
});

describe("formatChildFailure", () => {
  it("redacts credentials in both captured streams before publishing", () => {
    const message = formatChildFailure(
      "dev",
      "GET /api/dashboard failed http://alice:secret@127.0.0.1:8585/x",
      "proxy error https://bob:hunter2@internal.test/y",
    );
    expect(message).not.toContain("alice");
    expect(message).not.toContain("secret");
    expect(message).not.toContain("bob");
    expect(message).not.toContain("hunter2");
    expect(message).toContain("http://[redacted]@127.0.0.1:8585/x");
    expect(message).toContain("https://[redacted]@internal.test/y");
  });

  it("redacts a credential prefix cut by a chunk boundary", () => {
    expect(redactUrlCredentials("proxy http://user:pass")).toBe(
      "proxy http://[redacted]",
    );
  });

  it("redacts userinfo that spans the tail-slice boundary", () => {
    // The password is longer than the 16 KiB slice, so a slice-before-redact
    // order would publish raw password characters without their scheme.
    const longSecret =
      "http://alice:" + "p".repeat(17_000) + "@127.0.0.1:8585/db";
    const message = formatChildFailure("start", longSecret, "");
    expect(message).not.toContain("alice");
    expect(message).not.toContain("pppppppppp");
  });
});
