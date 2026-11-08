-- The size of each ranking population lives in one small table instead of being
-- repeated on every donor_rankings row. A donor joining or leaving the ranked
-- population changes the national count for everyone, so per-row copies made
-- every refresh rewrite the whole table; the chunked refresh now writes only
-- rows whose own rank changed. Apply before binaries that read the table.
CREATE TABLE donor_ranking_populations (
	scope VARCHAR(8) NOT NULL CHECK (scope IN ('NATIONAL', 'CITY')),
	city VARCHAR(80) NOT NULL DEFAULT '',
	population INTEGER NOT NULL CHECK (population > 0),
	PRIMARY KEY (scope, city),
	CHECK (scope = 'CITY' OR city = '')
);

INSERT INTO donor_ranking_populations (scope, city, population)
SELECT 'NATIONAL', '', max(national_population) FROM donor_rankings HAVING max(national_population) > 0;

INSERT INTO donor_ranking_populations (scope, city, population)
SELECT 'CITY', city, max(city_population) FROM donor_rankings WHERE city IS NOT NULL AND city_population > 0 GROUP BY city;

ALTER TABLE donor_rankings DROP COLUMN national_population, DROP COLUMN city_population;
