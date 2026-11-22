// Generated from contracts/mobile/ui-catalog.json; run `make generate` after changing it.

export const uiCatalogVersion = 1;

/** RFC 6901 pointer into the DonorDashboard response. */
export type UiBind = string;
export type UiMetricFormat = "integer" | "usd_minor" | "days" | "percent";
export type UiMetricCaption = "given" | "initiatives_funded" | "events_volunteered" | "referral_total" | "referral_link" | "referral_irl" | "active_network" | "annualized_network" | "streak" | "days_to_next" | "last_milestone" | "national_percentile" | "city_rank";
export type UiNavigatePath = "/account/dashboard" | "/account/profile" | "/account/payment-method" | "/account/gifts" | "/partner";

export type UiHeading = { type: "Heading"; title: string } | { type: "Heading"; bind: string };
export type UiText = { type: "Text"; bind: string };
export type UiMetric = { type: "Metric"; bind: string; format: UiMetricFormat; caption?: UiMetricCaption };
export type UiMetricGrid = { type: "MetricGrid"; children: Array<UiMetric | UiUnknownNode> };
export type UiCallout = { type: "Callout"; tone?: "info" | "success" | "warning"; title: string; body: string } | { type: "Callout"; tone?: "info" | "success" | "warning"; title: string; bind: string };
export type UiList = { type: "List"; bind: string; children: Array<UiText | UiLinkButton | UiUnknownNode> };
export type UiLinkButton = { type: "LinkButton"; navigate: UiNavigatePath };

/** A node whose type this catalog version does not know. Renderers draw nothing for it. */
export interface UiUnknownNode {
  type: string;
  [key: string]: unknown;
}

export type UiBlock = UiHeading | UiText | UiMetric | UiMetricGrid | UiCallout | UiList | UiLinkButton | UiUnknownNode;
export type UiSpec = UiBlock[];
