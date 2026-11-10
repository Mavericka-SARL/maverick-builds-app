import { lazy } from "react";

// The chart widget, loaded when a dashboard first shows a chart: the chart
// library is most of the console's main file otherwise (2026-11-09).
export const ChartWidget = lazy(() => import("./ChartWidget").then(m => ({ default: m.ChartWidget })));
