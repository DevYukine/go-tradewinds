import type * as ReactTable from "@tanstack/react-table";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DataTable, type Column } from "./data-table";

const state = vi.hoisted(() => ({
  sorting: [] as ReactTable.SortingState,
  globalFilter: "",
  columnVisibility: {} as ReactTable.ColumnVisibilityState,
}));
vi.mock("@tanstack/react-table", async (original) => {
  const reactTable = await original<typeof ReactTable>();
  const useControlledTable: typeof reactTable.useTable = (options, selector) =>
    reactTable.useTable(
      { ...options, state: Object.assign({}, options.state, state) },
      selector,
    );
  return { ...reactTable, useTable: useControlledTable };
});

type Row = { id: string; amount?: number };
const data: Row[] = [
  { id: "Nine", amount: 900 },
  { id: "Unknown" },
  { id: "Hundred", amount: 10000 },
];
const columns: Column<Row>[] = [
  { label: "Name", value: (row) => row.id, render: (row) => row.id },
  {
    label: "Amount",
    value: (row) => row.amount,
    render: (row) => row.amount ?? "Not recorded",
    className: "numeric",
  },
  {
    label: "Details",
    render: (row) =>
      createElement(
        "details",
        null,
        createElement("summary", null, "Recorded payload"),
        row.id,
      ),
  },
];
function body() {
  const html = renderToStaticMarkup(
    createElement(DataTable<Row>, {
      data,
      columns,
      rowKey: (row) => row.id,
      label: "Recorded rows",
    }),
  );
  return html.match(/<tbody>([\s\S]*?)<\/tbody>/)![1];
}

describe("shared table custom cell rendering", () => {
  beforeEach(() => {
    state.sorting = [];
    state.globalFilter = "";
    state.columnVisibility = {};
  });

  it.each([false, true])(
    "keeps numeric sorting and unknowns last with descending=%s",
    (desc) => {
      state.sorting = [{ id: "Amount", desc }];
      const html = body();
      const names = [...html.matchAll(/<tr><td>([^<]+)<\/td>/g)].map(
        (match) => match[1],
      );
      expect(names).toEqual(
        desc ? ["Hundred", "Nine", "Unknown"] : ["Nine", "Hundred", "Unknown"],
      );
      expect(html).toContain('class="numeric"');
      expect(html).toContain("<details><summary>Recorded payload</summary>");
    },
  );

  it("keeps filtering and column visibility independent of cell rendering", () => {
    state.globalFilter = "Nine";
    state.columnVisibility = { Amount: false };
    const html = body();
    expect(html).toContain("Nine");
    expect(html).not.toContain("Hundred");
    expect(html).not.toContain("Unknown");
    expect(html).not.toContain('class="numeric"');
    expect(html).toContain("<details><summary>Recorded payload</summary>");
  });
});
