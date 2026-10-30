"use client";

import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import {
  columnFilteringFeature,
  columnVisibilityFeature,
  createFilteredRowModel,
  createPaginatedRowModel,
  createSortedRowModel,
  filterFn_includesString,
  globalFilteringFeature,
  rowPaginationFeature,
  rowSortingFeature,
  sortFn_basic,
  tableFeatures,
  useTable,
  type ColumnDef,
  type ColumnVisibilityState,
  type PaginationState,
  type RowData,
  type SortingState,
} from "@tanstack/react-table";
import {
  ArrowDown,
  ArrowUp,
  Check,
  ChevronLeft,
  ChevronRight,
  Columns3,
  Database,
  Search,
  type LucideIcon,
} from "lucide-react";
import { useId, useMemo, useState, type ReactNode } from "react";

const features = tableFeatures({
  columnFilteringFeature,
  globalFilteringFeature,
  columnVisibilityFeature,
  rowSortingFeature,
  rowPaginationFeature,
  filteredRowModel: createFilteredRowModel(),
  sortedRowModel: createSortedRowModel(),
  paginatedRowModel: createPaginatedRowModel(),
  filterFns: { includesString: filterFn_includesString },
  sortFns: { basic: sortFn_basic },
});

export interface Column<T extends RowData> {
  label: string;
  render: (row: T) => ReactNode;
  value?: (row: T) => string | number | undefined;
  filter?: boolean;
  className?: string;
}

export function DataTable<T extends RowData>({
  columns,
  data,
  rowKey,
  empty = "No recorded entries",
  emptyIcon: EmptyIcon = Database,
  label,
  searchable = true,
}: {
  columns: Column<T>[];
  data: T[];
  rowKey: (row: T, index: number) => string;
  empty?: string;
  emptyIcon?: LucideIcon;
  label: string;
  searchable?: boolean;
}) {
  const helpID = useId();
  const [sorting, setSorting] = useState<SortingState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [columnVisibility, setColumnVisibility] =
    useState<ColumnVisibilityState>({});
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: 10,
  });
  const definitions = useMemo<ColumnDef<typeof features, T>[]>(
    () =>
      columns.map((column, index) => ({
        id: column.label,
        header: column.label,
        accessorFn: column.value,
        enableSorting: Boolean(column.value),
        enableGlobalFilter: Boolean(column.value) && column.filter !== false,
        enableHiding: index !== 0,
        sortFn: "basic",
        sortUndefined: "last",
      })),
    [columns],
  );
  const table = useTable({
    features,
    columns: definitions,
    data,
    getRowId: rowKey,
    state: { sorting, globalFilter, columnVisibility, pagination },
    onSortingChange: setSorting,
    onGlobalFilterChange: setGlobalFilter,
    onColumnVisibilityChange: setColumnVisibility,
    onPaginationChange: setPagination,
    globalFilterFn: "includesString",
    autoResetPageIndex: false,
  });
  const filteredCount = table.getFilteredRowModel().rows.length;
  const pageCount = Math.max(1, table.getPageCount());
  if (pagination.pageIndex >= pageCount) {
    setPagination({ ...pagination, pageIndex: pageCount - 1 });
  }
  const active =
    globalFilter !== "" ||
    sorting.length > 0 ||
    Object.values(columnVisibility).some((visible) => !visible);

  return (
    <div className="data-table">
      <div className="table-toolbar">
        {searchable && (
          <label className="search-field">
            <Search size={15} strokeWidth={1.7} aria-hidden="true" />
            <span className="sr-only">Search {label}</span>
            <input
              type="search"
              placeholder={`Search ${label.toLowerCase()}`}
              value={globalFilter}
              onChange={(event) => {
                setGlobalFilter(event.target.value);
                setPagination((current) => ({ ...current, pageIndex: 0 }));
              }}
            />
          </label>
        )}
        <div className="table-tools">
          <span className="muted tabular-nums">
            {filteredCount} of {data.length} loaded
          </span>
          <DropdownMenu.Root>
            <DropdownMenu.Trigger
              className="button button-quiet"
              aria-label={`Columns for ${label}`}
            >
              <Columns3 size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
              Columns
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content
                className="dropdown-content"
                align="end"
                sideOffset={6}
              >
                <DropdownMenu.Label className="dropdown-label">
                  Visible columns
                </DropdownMenu.Label>
                {table.getAllLeafColumns().map((column) => (
                  <DropdownMenu.CheckboxItem
                    className="dropdown-item"
                    key={column.id}
                    checked={column.getIsVisible()}
                    disabled={!column.getCanHide()}
                    onSelect={(event) => event.preventDefault()}
                    onCheckedChange={(checked) =>
                      column.toggleVisibility(checked === true)
                    }
                  >
                    <span className="dropdown-check">
                      <DropdownMenu.ItemIndicator>
                        <Check size={13} strokeWidth={1.7} aria-hidden="true" />
                      </DropdownMenu.ItemIndicator>
                    </span>
                    {column.id}
                  </DropdownMenu.CheckboxItem>
                ))}
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
          </DropdownMenu.Root>
          {active && (
            <button
              className="button button-quiet"
              onClick={() => {
                setGlobalFilter("");
                setSorting([]);
                setColumnVisibility({});
                setPagination((current) => ({ ...current, pageIndex: 0 }));
              }}
            >
              Reset
            </button>
          )}
        </div>
      </div>
      {sorting.length > 0 && (
        <div className="table-sort-summary">
          Sorted by{" "}
          {sorting
            .map(
              (sort) => `${sort.id} ${sort.desc ? "descending" : "ascending"}`,
            )
            .join(", ")}
        </div>
      )}
      <p className="sr-only" id={helpID}>
        Activate a column heading to sort. Hold Shift to sort by multiple
        columns. Search, sorting and page counts cover loaded records only.
        Focus the table region and use the arrow keys to scroll horizontally.
      </p>
      <div
        className="table-scroll"
        role="region"
        aria-label={`${label} table`}
        aria-describedby={helpID}
        tabIndex={0}
      >
        <table>
          <caption className="sr-only">{label}</caption>
          <thead>
            {table.getHeaderGroups().map((group) => (
              <tr key={group.id}>
                {group.headers.map((header) => {
                  const sorted = header.column.getIsSorted();
                  const primary = sorting[0]?.id === header.column.id;
                  return (
                    <th
                      key={header.id}
                      scope="col"
                      aria-sort={
                        primary
                          ? sorted === "desc"
                            ? "descending"
                            : "ascending"
                          : undefined
                      }
                      className={
                        columns.find(
                          (column) => column.label === header.column.id,
                        )?.className
                      }
                    >
                      {header.column.getCanSort() ? (
                        <button
                          className="table-sort"
                          onClick={header.column.getToggleSortingHandler()}
                        >
                          <table.FlexRender header={header} />
                          {sorted === "asc" ? (
                            <ArrowUp
                              size={13}
                              strokeWidth={1.7}
                              aria-hidden="true"
                            />
                          ) : sorted === "desc" ? (
                            <ArrowDown
                              size={13}
                              strokeWidth={1.7}
                              aria-hidden="true"
                            />
                          ) : (
                            <span className="sort-placeholder" />
                          )}
                        </button>
                      ) : (
                        <table.FlexRender header={header} />
                      )}
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {table.getRowModel().rows.map((row) => (
              <tr key={row.id}>
                {row.getVisibleCells().map((cell) => {
                  const column = columns.find(
                    (column) => column.label === cell.column.id,
                  );
                  return (
                    <td key={cell.id} className={column?.className}>
                      {/* Render callbacks are not component types; preserve native details on refresh. */}
                      {column?.render(row.original)}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
        {table.getRowModel().rows.length === 0 && (
          <div className="table-empty">
            <div className="empty-icon">
              <EmptyIcon size={24} strokeWidth={1.7} aria-hidden="true" />
            </div>
            <strong>
              {filteredCount === 0
                ? empty
                : "This page no longer contains records"}
            </strong>
            <p>
              {globalFilter
                ? "Change your search or clear the filters."
                : "No matching records in the loaded data."}
            </p>
            {pagination.pageIndex > 0 && (
              <button className="button" onClick={() => table.firstPage()}>
                Go to first page
              </button>
            )}
          </div>
        )}
      </div>
      <div className="table-pagination">
        <label>
          Rows{" "}
          <select
            aria-label={`Rows per page for ${label}`}
            value={pagination.pageSize}
            onChange={(event) => table.setPageSize(Number(event.target.value))}
          >
            {[10, 25, 50, 100].map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
        </label>
        <span className="muted tabular-nums">
          Page {pagination.pageIndex + 1} of {pageCount}
        </span>
        <div className="button-group">
          <button
            className="icon-button"
            aria-label={`Previous page of ${label}`}
            disabled={!table.getCanPreviousPage()}
            onClick={() => table.previousPage()}
          >
            <ChevronLeft size={15} strokeWidth={1.7} aria-hidden="true" />
          </button>
          <button
            className="icon-button"
            aria-label={`Next page of ${label}`}
            disabled={!table.getCanNextPage()}
            onClick={() => table.nextPage()}
          >
            <ChevronRight size={15} strokeWidth={1.7} aria-hidden="true" />
          </button>
        </div>
      </div>
    </div>
  );
}
