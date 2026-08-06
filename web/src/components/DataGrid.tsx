import {Table} from "@heroui/react/table";
import type {ReactNode} from "react";

export interface DataGridColumn<T extends object> {
  id: string;
  header: string;
  isRowHeader?: boolean;
  align?: "start" | "center" | "end";
  accessorKey?: keyof T;
  cell?: (item: T) => ReactNode;
}

interface DataGridProps<T extends object> {
  "aria-label": string;
  columns: DataGridColumn<T>[];
  contentClassName?: string;
  data: T[];
  getRowId: (item: T) => string | number;
  renderEmptyState: () => ReactNode;
  scrollContainerClassName?: string;
  variant?: "primary" | "secondary";
}

function alignmentClass(align: DataGridColumn<object>["align"]): string | undefined {
  switch (align) {
    case "center":
      return "text-center";
    case "end":
      return "text-end";
    default:
      return undefined;
  }
}

function cellContent<T extends object>(column: DataGridColumn<T>, item: T): ReactNode {
  if (column.cell) {
    return column.cell(item);
  }
  if (column.accessorKey === undefined) {
    return null;
  }
  const value = item[column.accessorKey];
  return value === null || value === undefined ? "" : String(value);
}

// GeneralsX @bugfix OpenAI 06/08/2026 Keep tables on the public HeroUI package exports used by clean CI installs.
export function DataGrid<T extends object>({
  "aria-label": ariaLabel,
  columns,
  contentClassName,
  data,
  getRowId,
  renderEmptyState,
  scrollContainerClassName,
  variant = "secondary",
}: DataGridProps<T>) {
  return (
    <Table variant={variant}>
      <Table.ScrollContainer className={scrollContainerClassName}>
        <Table.Content aria-label={ariaLabel} className={contentClassName}>
          <Table.Header columns={columns}>
            {(column) => (
              <Table.Column
                className={alignmentClass(column.align)}
                id={column.id}
                isRowHeader={column.isRowHeader}
              >
                {column.header}
              </Table.Column>
            )}
          </Table.Header>
          <Table.Body items={data} renderEmptyState={renderEmptyState}>
            {(item) => (
              <Table.Row columns={columns} id={getRowId(item)}>
                {(column) => (
                  <Table.Cell className={alignmentClass(column.align)}>
                    {cellContent(column, item)}
                  </Table.Cell>
                )}
              </Table.Row>
            )}
          </Table.Body>
        </Table.Content>
      </Table.ScrollContainer>
    </Table>
  );
}
