"use client";

/**
 * The chart view: one specification, drawn by the adapter.
 *
 * This module is loaded only in the browser because ECharts measures an element
 * to lay a chart out, and there is no element during server rendering. It is
 * reached through `next/dynamic` with `ssr: false` for that reason; the library
 * itself is a static import here.
 */

import { useEffect, useRef, useState } from "react";

import type { ChartBlock } from "@/lib/api";

import { renderChart, type ChartHandle } from "./echarts-adapter";

export default function ChartView({ block }: { block: ChartBlock }) {
  const container = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let disposed = false;
    let chart: ChartHandle | null = null;
    const element = container.current;
    if (!element) {
      return;
    }

    try {
      chart = renderChart(element, block.spec);
    } catch (cause) {
      // Reported off the effect body: the draw belongs here, the state update
      // does not, and a failed draw has no follow-up work to lose.
      const message = cause instanceof Error ? cause.message : "Grafik tidak bisa dirender.";
      queueMicrotask(() => {
        if (!disposed) {
          setError(message);
        }
      });
      return;
    }

    // The conversation column and the window both change size; the chart follows.
    const observer = new ResizeObserver(() => {
      if (!disposed) {
        chart?.resize();
      }
    });
    observer.observe(element);

    return () => {
      disposed = true;
      observer.disconnect();
      chart?.dispose();
    };
  }, [block.spec]);

  if (error) {
    return (
      <p className="rounded-md border border-bolu-belum/30 bg-bolu-belum/8 px-3 py-2 text-sm text-bolu-belum">
        {error}
      </p>
    );
  }

  return (
    <figure className="my-1">
      {block.title ? (
        <figcaption className="mb-1 text-sm font-semibold">{block.title}</figcaption>
      ) : null}
      <div ref={container} className="h-64 w-full min-w-0" />
    </figure>
  );
}
