// @vitest-environment jsdom

import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { StatusBadge } from "./ui";

function badge(status: string, fate?: string | null) {
  const container = document.createElement("div");
  container.innerHTML = renderToStaticMarkup(
    createElement(StatusBadge, { status, fate }),
  );
  return container.querySelector(".badge")!;
}

describe("ship status badges", () => {
  it.each([
    ["lost_at_sea", "Lost at sea", "danger", "skull"],
    ["captured", "Captured by pirates", "danger", "triangle-alert"],
    ["seized_smuggling", "Seized for smuggling", "danger", "triangle-alert"],
    ["seized_bailiff", "Seized by the bailiff", "danger", "triangle-alert"],
    [
      "seized_bankruptcy",
      "Seized after bankruptcy",
      "danger",
      "triangle-alert",
    ],
    ["broken_up", "Broken up", "neutral", "hammer"],
    ["royal_navy", "Joined the Royal Navy", "neutral", "flag"],
    [undefined, "Sunk", "danger", "skull"],
    [null, "Sunk", "danger", "skull"],
    ["", "Sunk", "danger", "skull"],
    ["unknown_fate", "Gone", "neutral", "circle"],
  ])(
    "renders %s with meaningful text, tone and icon",
    (fate, label, tone, icon) => {
      const element = badge("sunk", fate);
      expect(element.textContent).toBe(label);
      expect(element.classList.contains(`badge-${tone}`)).toBe(true);
      expect(
        element.querySelector(`.lucide-${icon}`)?.getAttribute("aria-hidden"),
      ).toBe("true");
    },
  );

  it("keeps normal status styling even if a fate is present", () => {
    const element = badge("sailing", "royal_navy");
    expect(element.textContent).toBe("Sailing");
    expect(element.classList.contains("badge-good")).toBe(true);
    expect(element.querySelector(".lucide-ship")).not.toBeNull();
  });

  it("does not treat recorded status or fate text as markup", () => {
    const status = badge('<img src=x onerror="alert(1)">');
    expect(status.querySelector("img")).toBeNull();
    expect(status.textContent).toContain("<img");
    const fate = badge("sunk", '<img src=x onerror="alert(1)">');
    expect(fate.textContent).toBe("Gone");
    expect(fate.querySelector("img")).toBeNull();
  });
});
