"use client";

/**
 * The chart adapter: Bolu's chart specification to ECharts.
 *
 * The agent fills a small specification (kind, axes, series) and never writes
 * library configuration, so the library is swappable: rewriting this file is the
 * whole cost of moving to another chart library, and the tool, the JSON Schema,
 * and the prompt all stay as they are.
 */

import * as echarts from "echarts/core";
import { BarChart, LineChart, PieChart, ScatterChart } from "echarts/charts";
import {
  GridComponent,
  LegendComponent,
  TitleComponent,
  TooltipComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";

import type { ChartSpec } from "@/lib/api";

echarts.use([
  BarChart,
  LineChart,
  PieChart,
  ScatterChart,
  GridComponent,
  LegendComponent,
  TitleComponent,
  TooltipComponent,
  CanvasRenderer,
]);

/** The chart handle a caller disposes and resizes. */
export type ChartHandle = {
  dispose: () => void;
  resize: () => void;
};

/**
 * The categorical palette. It does not follow the theme: a series is the same
 * colour in both, so a screenshot of a chart and the chart beside it agree. Only
 * the furniture — text, axes, grid, the gap between slices — is themed.
 */
const PALETTE = ["#1F7FD6", "#2FA65A", "#E2602B", "#A855F7", "#EAB308", "#EC4899", "#14B8A6", "#8B88A0"];

/** The colours a chart reads out of the theme, resolved to values a canvas can paint. */
export type ChartPalette = {
  text: string;
  axis: string;
  border: string;
  grid: string;
  surface: string;
};

/**
 * readChartPalette reads the theme off the document.
 *
 * It reads rather than imports because ECharts paints into a canvas, and a canvas
 * cannot resolve `var(--x)` — the value has to be a colour by the time it is
 * drawn. Reading at draw time is also what makes a chart follow a theme change
 * without a second stylesheet.
 */
export function readChartPalette(): ChartPalette {
  const style = getComputedStyle(document.documentElement);
  const read = (name: string) => style.getPropertyValue(name).trim();
  return {
    text: read("--color-bolu-body"),
    axis: read("--color-bolu-rest"),
    border: read("--color-bolu-border"),
    grid: read("--color-bolu-line"),
    surface: read("--color-bolu-surface"),
  };
}

/**
 * toECharts translates the specification into an option object.
 *
 * It is exported so a test can assert the translation without a canvas, which is
 * what keeps the adapter verifiable: the alternative is a browser test for
 * arithmetic that has nothing to do with the browser.
 */
export function toECharts(spec: ChartSpec, palette: ChartPalette): echarts.EChartsCoreOption {
  const title = spec.title
    ? {
        text: spec.title,
        textStyle: { color: palette.text, fontSize: 14, fontWeight: 600 },
      }
    : undefined;

  const tooltip = {
    trigger: spec.kind === "pie" || spec.kind === "scatter" ? ("item" as const) : ("axis" as const),
    valueFormatter: spec.unit ? (value: number) => `${formatNumber(value)} ${spec.unit}` : undefined,
  };

  const legend =
    spec.kind === "pie" || (spec.series && spec.series.length > 1)
      ? { bottom: 0, textStyle: { color: palette.text } }
      : undefined;

  if (spec.kind === "pie") {
    return {
      title,
      tooltip,
      legend,
      color: PALETTE,
      series: [
        {
          type: "pie",
          radius: ["45%", "70%"],
          // A donut rather than a disc: the hole is where the total goes, and a
          // pie with many slices stays readable.
          avoidLabelOverlap: true,
          itemStyle: { borderColor: palette.surface, borderWidth: 2 },
          label: { color: palette.text },
          data: (spec.slices ?? []).map((slice) => ({ name: slice.name, value: slice.value })),
        },
      ],
    };
  }

  if (spec.kind === "scatter") {
    return {
      title,
      tooltip,
      grid: gridFor(spec),
      color: PALETTE,
      xAxis: axisFor(spec.x_label, "value", undefined, false, palette),
      yAxis: axisFor(spec.y_label, "value", undefined, false, palette),
      series: [
        {
          type: "scatter",
          name: spec.series?.[0]?.name ?? "Data",
          symbolSize: 9,
          data: (spec.points ?? []).map((point) => [point.x, point.y]),
        },
      ],
    };
  }

  // Bar, line, and area share the category-axis shape; only the series type and
  // the stacking differ.
  const seriesType = spec.kind === "bar" ? "bar" : "line";

  return {
    title,
    tooltip,
    legend,
    grid: gridFor(spec),
    color: PALETTE,
    xAxis: axisFor(spec.x_label, "category", spec.categories, false, palette),
    // A bar chart is read by comparing lengths, so its axis has to start at zero
    // or the difference between two bars is exaggerated. A line chart is read by
    // its shape, where a zoomed axis is the point.
    yAxis: axisFor(spec.y_label, "value", undefined, spec.kind !== "bar", palette),
    series: (spec.series ?? []).map((entry) => ({
      name: entry.name,
      type: seriesType,
      stack: spec.stacked ? "total" : undefined,
      // A line gets a smooth curve and visible points: a chat figure is read at
      // a glance, not studied.
      smooth: seriesType === "line",
      showSymbol: seriesType === "line" && entry.data.length <= 24,
      areaStyle: spec.kind === "area" ? { opacity: 0.18 } : undefined,
      barMaxWidth: 32,
      data: entry.data,
    })),
  };
}

/** gridFor leaves room for the title and the legend, and keeps labels inside. */
function gridFor(spec: ChartSpec): Record<string, number | boolean> {
  return {
    left: 8,
    right: 16,
    top: spec.title ? 48 : 16,
    bottom: spec.series && spec.series.length > 1 ? 32 : 8,
    containLabel: true,
  };
}

function axisFor(
  label: string | undefined,
  kind: "category" | "value",
  categories: string[] | undefined,
  zoomToData: boolean,
  palette: ChartPalette,
) {
  const base = {
    type: kind,
    name: label,
    nameTextStyle: { color: palette.axis },
    axisLine: { lineStyle: { color: palette.border } },
    axisLabel: { color: palette.axis },
    splitLine: { lineStyle: { color: palette.grid } },
  };

  if (kind === "category") {
    return { ...base, boundaryGap: true, data: categories ?? [] };
  }
  return { ...base, scale: zoomToData };
}

/** formatNumber renders a number the way the rest of the app does. */
function formatNumber(value: number): string {
  return new Intl.NumberFormat("id-ID", { maximumFractionDigits: 2 }).format(value);
}

/**
 * renderChart draws one specification into an element and returns its handle.
 *
 * The chart is created once per element and updated in place afterwards, which is
 * what keeps a streamed message from rebuilding the canvas on every token.
 */
export function renderChart(
  element: HTMLElement,
  spec: ChartSpec,
  palette: ChartPalette,
): ChartHandle {
  const existing = echarts.getInstanceByDom(element);
  const chart = existing ?? echarts.init(element, undefined, { renderer: "canvas" });
  chart.setOption(toECharts(spec, palette), true);

  return {
    dispose: () => chart.dispose(),
    resize: () => chart.resize(),
  };
}
