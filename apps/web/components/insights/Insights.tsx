import Link from "next/link";
import { Component, type ReactNode } from "react";
import type { UiMetricCaption } from "@/lib/ui-catalog";
import { uiMetricCaptions } from "@/lib/ui-catalog-data";
import { linkLabel, metricText, resolvePointer, scalarText } from "./resolve";

type Node = Record<string, unknown>;
const MAX_LIST_ITEMS = 8;

const isNode = (value: unknown): value is Node => value !== null && typeof value === "object" && !Array.isArray(value);
const text = (value: unknown) => (typeof value === "string" && value.trim() ? value : null);

const metricOf = (node: Node, dashboard: unknown) => metricText(resolvePointer(dashboard, node.bind), node.format);

function Metric({ node, dashboard }: { node: Node; dashboard: unknown }) {
  const value = metricOf(node, dashboard);
  if (value === null) return null;
  const caption =
    typeof node.caption === "string" && Object.hasOwn(uiMetricCaptions, node.caption)
      ? uiMetricCaptions[node.caption as UiMetricCaption]
      : null;
  return (
    <article className="metric">
      <p className="metricValue">{value}</p>
      {caption && <p>{caption}</p>}
    </article>
  );
}

/** Whether a List row would draw anything, so an empty list is not drawn as an empty box. */
const rowDraws = (node: unknown, dashboard: unknown): node is Node =>
  isNode(node) &&
  ((node.type === "Text" && scalarText(resolvePointer(dashboard, node.bind)) !== null) ||
    (node.type === "LinkButton" && linkLabel(node.navigate) !== undefined));

function Block({ node, dashboard, nested = false }: { node: Node; dashboard: unknown; nested?: boolean }) {
  switch (node.type) {
    case "Heading": {
      const title = text(node.title) ?? scalarText(resolvePointer(dashboard, node.bind));
      return title ? <h3 className="insightsHeading">{title}</h3> : null;
    }
    case "Text": {
      const value = scalarText(resolvePointer(dashboard, node.bind));
      return value === null ? null : <p>{value}</p>;
    }
    case "Metric":
      if (metricOf(node, dashboard) === null) return null;
      return nested ? (
        <Metric node={node} dashboard={dashboard} />
      ) : (
        <div className="metrics">
          <Metric node={node} dashboard={dashboard} />
        </div>
      );
    case "MetricGrid": {
      if (!Array.isArray(node.children)) return null;
      const cells = node.children.filter(
        (child): child is Node => isNode(child) && child.type === "Metric" && metricOf(child, dashboard) !== null,
      );
      if (!cells.length) return null;
      return (
        <div className="metrics">
          {cells.map((cell, index) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: nodes carry no id
            <Metric key={index} node={cell} dashboard={dashboard} />
          ))}
        </div>
      );
    }
    case "Callout": {
      const title = text(node.title);
      const body = text(node.body) ?? scalarText(resolvePointer(dashboard, node.bind));
      if (!title || !body) return null;
      const tone = node.tone === "success" ? " highlight" : node.tone === "warning" ? " warn" : "";
      return (
        <div className={`callout${tone}`}>
          <h3 className="insightsCalloutTitle">{title}</h3>
          <p>{body}</p>
        </div>
      );
    }
    case "List": {
      const items = resolvePointer(dashboard, node.bind);
      if (!Array.isArray(items) || !items.length || !Array.isArray(node.children)) return null;
      const rows = node.children.slice(0, MAX_LIST_ITEMS).filter((child): child is Node => rowDraws(child, dashboard));
      if (!rows.length) return null;
      return (
        <ul className="insightsList">
          {rows.map((child, index) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: nodes carry no id
            <li key={index}>
              <Block node={child} dashboard={dashboard} nested />
            </li>
          ))}
        </ul>
      );
    }
    case "LinkButton": {
      const target = linkLabel(node.navigate);
      return target ? (
        <Link className="secondary" href={target.href}>
          {target.label}
        </Link>
      ) : null;
    }
    default:
      return null;
  }
}

/** Draws nothing when a layout somehow breaks rendering; the dashboard around it stays intact. */
class Contain extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    return this.state.failed ? null : this.props.children;
  }
}

/**
 * Draws an approved layout against the dashboard response it arrived in. Unknown node types, binds that do not
 * resolve, and values of the wrong type draw nothing for that node; this component never throws.
 */
export function Insights({ spec, dashboard }: { spec: unknown; dashboard: unknown }) {
  if (!Array.isArray(spec)) return null;
  return (
    <Contain>
      {spec.map((node, index) =>
        isNode(node) ? (
          // biome-ignore lint/suspicious/noArrayIndexKey: nodes carry no id
          <Block key={index} node={node} dashboard={dashboard} />
        ) : null,
      )}
    </Contain>
  );
}
