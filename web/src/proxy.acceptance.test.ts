import { existsSync, readFileSync } from "node:fs";
import { NextRequest } from "next/server";
import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe("Next dashboard proxy cutover", () => {
  it("replaces middleware.ts with proxy.ts at the Next file-convention boundary", () => {
    expect(existsSync(new URL("./proxy.ts", import.meta.url))).toBe(true);
    expect(existsSync(new URL("./middleware.ts", import.meta.url))).toBe(false);
  });

  it("exports proxy with matcher-only config and no unsupported runtime export", async () => {
    // Keep the missing file an individual acceptance failure, rather than a
    // transform failure that would prevent the other independent checks running.
    const path = "./proxy";
    const proxyModule = await import(path);
    expect(proxyModule.proxy).toBeTypeOf("function");
    expect(proxyModule).not.toHaveProperty("runtime");
    expect(Object.keys(proxyModule.config)).toEqual(["matcher"]);
    expect(proxyModule.config.matcher).toBe("/:path*");
  });

  it("preserves exact Host enforcement after the rename, ignoring forwarded hosts", async () => {
    vi.stubEnv("DASHBOARD_ALLOWED_HOSTS", "fleet.example:443");
    vi.resetModules();
    // This loading-boundary test needs the import after the environment change:
    // the module captures its Host allowlist once at initialization.
    const path = "./proxy";
    const { proxy } = await import(path);
    const allowed = await proxy(
      new NextRequest("http://foreign.example/ledger", {
        headers: { host: "fleet.example:443" },
      }),
    );
    expect(allowed.status).toBe(200);
    expect(allowed.headers.get("x-middleware-next")).toBe("1");
    const denied = await proxy(
      new NextRequest("http://localhost:3400/api/rivals", {
        headers: {
          host: "localhost.evil.example:3400",
          "x-forwarded-host": "localhost:3400",
        },
      }),
    );
    expect(denied.status).toBe(403);
    expect(await denied.text()).toBe("Forbidden host\n");
  });
});

type RGBA = [number, number, number, number];

// Evaluate the actual light tokens and the badge's actual color-mix declaration,
// including its transparent tint composited over both dashboard base surfaces.
// This is a numerical WCAG contrast test, not an assertion of CSS source text.
describe("light-theme good badge readability", () => {
  it("keeps normal-size good-badge text at least 4.5:1 on canvas and raised surfaces", () => {
    const css = readFileSync(
      new URL("./app/globals.css", import.meta.url),
      "utf8",
    );
    const declarations = (selector: string) => {
      const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const body = css.match(new RegExp(`${escaped}\\s*\\{([^}]+)\\}`))?.[1];
      expect(
        body,
        `${selector} must have published CSS declarations`,
      ).toBeDefined();
      return Object.fromEntries(
        (body ?? "")
          .split(";")
          .filter((entry) => entry.includes(":"))
          .map((entry) => {
            const colon = entry.indexOf(":");
            return [
              entry.slice(0, colon).trim(),
              entry.slice(colon + 1).trim(),
            ];
          }),
      );
    };
    const tokens = declarations(":root");
    const badge = { ...declarations(".badge"), ...declarations(".badge-good") };
    const color = (input: string): RGBA => {
      const value = input.trim();
      const variable = value.match(/^var\((--[\w-]+)\)$/);
      if (variable) return color(tokens[variable[1]]);
      if (value === "transparent") return [0, 0, 0, 0];
      const hex = value.match(/^#([\da-f]{6})([\da-f]{2})?$/i);
      if (hex)
        return [0, 2, 4]
          .map((offset) => parseInt(hex[1].slice(offset, offset + 2), 16))
          .concat(hex[2] ? parseInt(hex[2], 16) / 255 : 1) as RGBA;
      const mix = value.match(/^color-mix\(in srgb, (.+) ([\d.]+)%, (.+)\)$/);
      if (mix) {
        const left = color(mix[1]);
        const right = color(mix[3]);
        const weight = Number(mix[2]) / 100;
        const alpha = left[3] * weight + right[3] * (1 - weight);
        return [0, 1, 2]
          .map((index) =>
            alpha === 0
              ? 0
              : (left[index] * left[3] * weight +
                  right[index] * right[3] * (1 - weight)) /
                alpha,
          )
          .concat(alpha) as RGBA;
      }
      throw new Error(
        `Unsupported CSS color in contrast calculation: ${value}`,
      );
    };
    const over = (foreground: RGBA, background: RGBA): RGBA =>
      [
        ...[0, 1, 2].map(
          (index) =>
            foreground[index] * foreground[3] +
            background[index] * (1 - foreground[3]),
        ),
        1,
      ] as RGBA;
    const luminance = (rgb: RGBA) => {
      const linear = rgb
        .slice(0, 3)
        .map((channel) => channel / 255)
        .map((channel) =>
          channel <= 0.04045
            ? channel / 12.92
            : ((channel + 0.055) / 1.055) ** 2.4,
        );
      return linear[0] * 0.2126 + linear[1] * 0.7152 + linear[2] * 0.0722;
    };
    for (const surface of ["--canvas", "--surface"]) {
      const background = over(color(badge.background), color(tokens[surface]));
      const foreground = over(color(badge.color), background);
      const left = luminance(foreground);
      const right = luminance(background);
      const ratio =
        (Math.max(left, right) + 0.05) / (Math.min(left, right) + 0.05);
      expect(
        ratio,
        `light badge-good on ${surface}: ${ratio.toFixed(3)}:1`,
      ).toBeGreaterThanOrEqual(4.5);
    }
  });
});
