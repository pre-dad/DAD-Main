import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = JSON.parse(await readFile(new URL("../apps/donor-api/api/openapi.json", import.meta.url)));
const template = await readFile(new URL("../deploy/nginx/templates/ingress.conf.template", import.meta.url), "utf8");

const limitedLocations = [...template.matchAll(/location ~\*? (\S+) \{([^}]*)\}/g)]
  .map(([, pattern, body]) => ({ pattern: new RegExp(pattern), zone: body.match(/limit_req zone=(\w+)/)?.[1] }))
  .filter(({ zone }) => zone);

const classified = (zone) =>
  Object.entries(source.paths).flatMap(([path, item]) =>
    Object.values(item)
      .filter((operation) => operation?.["x-rate-limit"] === zone)
      .map(() => `/api${path.replace(/\{[^{}]+\}/g, "example")}`),
  );

for (const zone of ["auth", "visitor"]) {
  test(`every ${zone}-class operation reaches the ${zone} zone`, () => {
    const paths = classified(zone);
    assert.notEqual(paths.length, 0);
    for (const path of paths) {
      const match = limitedLocations.find(({ pattern }) => pattern.test(path));
      assert.equal(match?.zone, zone, `${path} is limited by ${match?.zone ?? "no dedicated zone"}`);
    }
  });
}
