// Generated from contracts/mobile/ui-catalog.json; run `make generate` after changing it.
import type { UiMetricCaption, UiNavigatePath } from "./ui-catalog";

export const supportedCatalogVersion = 1;

export const uiMetricCaptions: Record<UiMetricCaption, string> = {
  "given": "Total given",
  "initiatives_funded": "Initiatives funded",
  "events_volunteered": "Events volunteered",
  "referral_total": "Given through your referrals",
  "referral_link": "Referred by link",
  "referral_irl": "Referred in person",
  "active_network": "Active in your network",
  "annualized_network": "Network giving per year",
  "streak": "Current streak",
  "days_to_next": "Days to next milestone",
  "last_milestone": "Last milestone",
  "national_percentile": "National percentile",
  "city_rank": "City rank",
};

export const uiNavigateLabels: Record<UiNavigatePath, string> = {
  "/account/dashboard": "Your dashboard",
  "/account/profile": "Your profile",
  "/account/payment-method": "Payment method",
  "/account/gifts": "Your gifts",
};
