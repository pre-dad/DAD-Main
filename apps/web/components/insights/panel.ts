import { supportedCatalogVersion } from "@/lib/ui-catalog-data";

type Panel = { catalog_version: number; spec: unknown };

/**
 * True for a panel this build can draw. The API omits `insights` when no layout is live and older servers never
 * send it, so a missing field, null, an empty layout, and one written for a newer catalog all mean no panel.
 */
export function canRender(panel: Panel | null | undefined): panel is Panel {
  return (
    !!panel &&
    typeof panel.catalog_version === "number" &&
    panel.catalog_version <= supportedCatalogVersion &&
    Array.isArray(panel.spec) &&
    panel.spec.length > 0
  );
}
