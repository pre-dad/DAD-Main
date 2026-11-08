package store

import (
	"context"
	"errors"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RankingService owns the periodic ranking rollup (PRD §6.8). The refresh ranks
// the whole population from one snapshot and writes the result in short
// id-ordered chunks that touch only rows whose rank changed; the donor read is a
// single-row lookup. This keeps ranking off the per-request path at 1M donors
// (risk #6) and within the worker step budget.
type RankingService struct {
	DB *pgxpool.Pool
	// ChunkSize is how many ranked donors one write transaction covers
	// (defaultRankingChunk when zero).
	ChunkSize int

	afterChunk func(written int) error
}

const defaultRankingChunk = 5000

// Ranking is a donor's own position (K-3: never anyone else's, never a list).
type Ranking struct {
	NationalPercentile int       `json:"national_percentile"`
	City               *string   `json:"city"`
	CityRank           *int      `json:"city_rank"`
	ComputedAt         time.Time `json:"computed_at"`
	nationalPopulation int
	cityPopulation     int
}

// ErrRankingRefreshRunning reports that another instance holds the refresh.
var ErrRankingRefreshRunning = errors.New("ranking refresh already running")

// Refresh recomputes the rankings now, whatever the schedule says.
func (s RankingService) Refresh(ctx context.Context, now time.Time) error {
	ran, err := s.refresh(ctx, now, false)
	if err == nil && !ran {
		return ErrRankingRefreshRunning
	}
	return err
}

// ForDonor returns a donor's own ranking, or (nil, nil) when they are unranked
// (no streak, opted out, or not yet computed).
func ForDonor(ctx context.Context, q *queries.Queries, donorID pgtype.UUID) (*Ranking, error) {
	row, err := q.DonorRanking(ctx, donorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Ranking{
		NationalPercentile: int(row.NationalPercentile),
		ComputedAt:         row.ComputedAt.Time,
		nationalPopulation: int(row.NationalPopulation),
		cityPopulation:     int(row.CityPopulation),
	}
	if row.City.Valid {
		city := row.City.String
		r.City = &city
	}
	if row.CityRank.Valid {
		rank := int(row.CityRank.Int32)
		r.CityRank = &rank
	}
	return r, nil
}

// RankingInterval is how often the global ranking rollup is recomputed. Ranking
// is a periodic projection, not a per-request or per-tick computation (risk #6),
// so the projections worker refreshes it at most this often.
const RankingInterval = 15 * time.Minute

// RefreshIfDue recomputes the rankings only when the last computation is older
// than RankingInterval, so it rides the projections worker loop cheaply. It
// returns whether it refreshed. A failed refresh leaves the schedule due, so
// the next step resumes it.
func (s RankingService) RefreshIfDue(ctx context.Context, now time.Time) (bool, error) {
	return s.refresh(ctx, now, true)
}

// rankingSnapshot ranks the whole population in one set-based pass (PRD §13
// risk #6: periodic, never per request). The eligible population is donors with
// a current streak who have not opted out; ties break on the earlier streak
// start, so a longer-standing identical streak ranks higher. national_percentile
// is the top-percentile bucket (1 = top 1%); city_rank is 1-based within
// donor.city. Each row also carries the size of the population it was ranked
// in. Rows come back in donor id order so they can be written in keyset chunks.
const rankingSnapshot = `
WITH eligible AS (
  SELECT d.id AS donor_id, d.city AS city,
         ($1::timestamptz AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date
           - (p.streak_since AT TIME ZONE coalesce(d.timezone,'America/New_York'))::date AS streak,
         p.streak_since AS since
  FROM donors d
  JOIN donor_metric_projections p ON p.donor_id = d.id
  LEFT JOIN donor_preferences pref ON pref.donor_id = d.id
  WHERE d.disabled = false
    AND p.current_streak IS NOT NULL
    AND p.current_streak > 0
    AND p.streak_since IS NOT NULL
    AND p.next_refresh_at > $1::timestamptz
    AND COALESCE(pref.rankings_opted_out, false) = false
),
ranked AS (
  SELECT donor_id, city,
         GREATEST(1, LEAST(100,
           CEIL(PERCENT_RANK() OVER (ORDER BY streak DESC, since ASC) * 100)::int
         )) AS national_percentile,
         CASE WHEN city IS NOT NULL
              THEN RANK() OVER (PARTITION BY city ORDER BY streak DESC, since ASC)::int
         END AS city_rank,
         (count(*) OVER ())::int AS national_population,
         CASE WHEN city IS NOT NULL THEN (count(*) OVER (PARTITION BY city))::int ELSE 0 END AS city_population
  FROM eligible
)
SELECT donor_id, national_percentile, city, city_rank, national_population, city_population
FROM ranked
ORDER BY donor_id`

// rankingUpsert writes a chunk, skipping rows whose rank is unchanged so an
// unchanged population writes nothing. Arguments: see rankedChunk.args.
const rankingUpsert = `
INSERT INTO donor_rankings AS r (donor_id, national_percentile, city, city_rank, computed_at)
SELECT donor_id, national_percentile, city, city_rank, $7::timestamptz
FROM unnest($1::uuid[], $2::int[], $3::varchar[], $4::int[], $5::int[], $6::int[])
  AS t(donor_id, national_percentile, city, city_rank, national_population, city_population)
ON CONFLICT (donor_id) DO UPDATE SET
  national_percentile = EXCLUDED.national_percentile,
  city = EXCLUDED.city,
  city_rank = EXCLUDED.city_rank,
  computed_at = EXCLUDED.computed_at
WHERE (r.national_percentile, r.city, r.city_rank)
  IS DISTINCT FROM (EXCLUDED.national_percentile, EXCLUDED.city, EXCLUDED.city_rank)`

// rankingPruneRange removes rows in (lo, hi] whose donors left the population;
// a NULL bound is open. The chunks tile the id space, so together they delete
// every departed donor.
const rankingPruneRange = `
DELETE FROM donor_rankings
WHERE ($1::uuid IS NULL OR donor_id > $1::uuid)
  AND ($2::uuid IS NULL OR donor_id <= $2::uuid)
  AND NOT donor_id = ANY($3::uuid[])`

// rankingAward gives Top 2% and Top 50 City to every donor in the chunk ranked
// into them, queueing the badge-earned push for each new one. A rank taken in a
// population smaller than the rule's minimum awards nothing. The fallbacks match
// defaultBadgeRules for a rule staff never saved.
const rankingAward = `
WITH rules AS (
  SELECT coalesce((SELECT threshold FROM donor_badge_rules WHERE code='NAT2'),2) AS nat_threshold,
         coalesce((SELECT min_population FROM donor_badge_rules WHERE code='NAT2'),100) AS nat_population,
         coalesce((SELECT threshold FROM donor_badge_rules WHERE code='CITY50'),50) AS city_threshold,
         coalesce((SELECT min_population FROM donor_badge_rules WHERE code='CITY50'),100) AS city_population
), ranked AS (
  SELECT * FROM unnest($1::uuid[], $2::int[], $3::varchar[], $4::int[], $5::int[], $6::int[])
    AS t(donor_id, national_percentile, city, city_rank, national_population, city_population)
), qualifying AS (
  SELECT donor_id, 'NAT2'::varchar AS code, NULL::varchar AS city FROM ranked, rules
  WHERE national_percentile <= rules.nat_threshold AND national_population >= rules.nat_population
  UNION ALL
  SELECT donor_id, 'CITY50'::varchar, ranked.city::varchar FROM ranked, rules
  WHERE city_rank <= rules.city_threshold AND ranked.city_population >= rules.city_population
), earned AS (
  INSERT INTO donor_badges (donor_id, code, source, earned_at, city)
  SELECT donor_id, code, 'AUTO', $7::timestamptz, city FROM qualifying
  ON CONFLICT (donor_id, code) DO NOTHING
  RETURNING donor_id, code
)
INSERT INTO donor_push_notifications (id, donor_id, category, dedup_key, context, available_at, expires_at)
SELECT gen_random_uuid(), e.donor_id, 'BADGE_EARNED', 'push:badge:' || e.donor_id::text || ':' || e.code,
  json_build_object('code', e.code), $7::timestamptz, $8::timestamptz
FROM earned e
WHERE EXISTS (SELECT 1 FROM donor_sessions s WHERE s.donor_id = e.donor_id AND s.push_token IS NOT NULL
  AND coalesce(s.refresh_expires_at, s.expires_at) > $7::timestamptz)
ON CONFLICT (dedup_key) DO NOTHING`

const rankingPopulationUpsert = `
INSERT INTO donor_ranking_populations AS p (scope, city, population)
SELECT * FROM unnest($1::varchar[], $2::varchar[], $3::int[]) AS t(scope, city, population)
ON CONFLICT (scope, city) DO UPDATE SET population = EXCLUDED.population
WHERE p.population IS DISTINCT FROM EXCLUDED.population`

const rankingPopulationPrune = `
DELETE FROM donor_ranking_populations
WHERE NOT ((scope, city) IN (SELECT * FROM unnest($1::varchar[], $2::varchar[])))`

// rankedChunk is one id-ordered slice of the snapshot.
type rankedChunk struct {
	ids        []pgtype.UUID
	percentile []int32
	city       []pgtype.Text
	cityRank   []pgtype.Int4
	national   []int32
	cityPop    []int32
}

func (c *rankedChunk) len() int { return len(c.ids) }

func (c *rankedChunk) last() pgtype.UUID { return c.ids[len(c.ids)-1] }

func (c *rankedChunk) reset() { *c = rankedChunk{} }

func (c *rankedChunk) args() []any {
	return []any{c.ids, c.percentile, c.city, c.cityRank, c.national, c.cityPop}
}

// refresh ranks the population from a single repeatable-read snapshot, so
// every chunk is cut from the same ordering and the percentiles and ranks agree
// across chunks, then writes it chunk by chunk, each in its own short
// transaction. A transaction-scoped advisory lock held on the snapshot
// connection lets one instance refresh at a time and is released if the
// instance dies. A refresh that fails part-way leaves each chunk either old or
// new, never partial, and the next refresh converges because it recomputes
// everything from scratch. due gates the run on the shared schedule.
func (s RankingService) refresh(ctx context.Context, now time.Time, due bool) (ran bool, err error) {
	snapshot, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, err
	}
	defer func() { _ = snapshot.Rollback(context.WithoutCancel(ctx)) }()
	var locked bool
	if err := snapshot.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended('rankings-refresh', 0))").Scan(&locked); err != nil ||
		!locked {
		return false, err
	}
	schedule := queries.New(s.DB)
	next := timestamp(now.Add(RankingInterval))
	if due {
		claimed, claimErr := schedule.ClaimProjectionSchedule(
			ctx,
			queries.ClaimProjectionScheduleParams{ID: "rankings", NextRunAt: next, AsOf: timestamp(now)},
		)
		if claimErr != nil || claimed == 0 {
			return false, claimErr
		}
		defer func() {
			if err != nil {
				_ = schedule.SaveProjectionSchedule(
					context.WithoutCancel(ctx),
					queries.SaveProjectionScheduleParams{ID: "rankings", NextRunAt: timestamp(now)},
				)
			}
		}()
	} else if err := schedule.SaveProjectionSchedule(ctx, queries.SaveProjectionScheduleParams{ID: "rankings", NextRunAt: next}); err != nil {
		return false, err
	}
	return true, s.writeSnapshot(ctx, snapshot, now)
}

func (s RankingService) writeSnapshot(ctx context.Context, snapshot pgx.Tx, now time.Time) error {
	size := s.ChunkSize
	if size <= 0 {
		size = defaultRankingChunk
	}
	rows, err := snapshot.Query(ctx, rankingSnapshot, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		chunk    rankedChunk
		low      pgtype.UUID
		national int32
		cities   = map[string]int32{}
		written  int
	)
	flush := func() error {
		if err := s.writeChunk(ctx, &chunk, low, now); err != nil {
			return err
		}
		low = chunk.last()
		written += chunk.len()
		chunk.reset()
		if s.afterChunk != nil {
			return s.afterChunk(written)
		}
		return nil
	}
	for rows.Next() {
		var (
			id                         pgtype.UUID
			percentile, nat, cityCount int32
			city                       pgtype.Text
			cityRank                   pgtype.Int4
		)
		if err := rows.Scan(&id, &percentile, &city, &cityRank, &nat, &cityCount); err != nil {
			return err
		}
		chunk.ids = append(chunk.ids, id)
		chunk.percentile = append(chunk.percentile, percentile)
		chunk.city = append(chunk.city, city)
		chunk.cityRank = append(chunk.cityRank, cityRank)
		chunk.national = append(chunk.national, nat)
		chunk.cityPop = append(chunk.cityPop, cityCount)
		national = nat
		if city.Valid {
			cities[city.String] = cityCount
		}
		if chunk.len() == size {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if chunk.len() > 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	return s.finish(ctx, low, national, cities)
}

// writeChunk upserts the changed rows of the chunk, prunes the departed donors
// whose ids fall in the chunk's id range, and awards its ranking badges, all in
// one transaction.
func (s RankingService) writeChunk(ctx context.Context, c *rankedChunk, low pgtype.UUID, now time.Time) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		args := append(c.args(), timestamp(now))
		if _, err := tx.Exec(ctx, rankingUpsert, args...); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, rankingPruneRange, nilIfInvalid(low), c.last(), c.ids); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, rankingAward, append(args, timestamp(now.Add(pushLifetime)))...)
		return err
	})
}

// finish prunes the departed donors past the last chunk and stores the
// population sizes the chunks were ranked in.
func (s RankingService) finish(ctx context.Context, low pgtype.UUID, national int32, cities map[string]int32) error {
	scopes, names, sizes := []string{}, []string{}, []int32{}
	if national > 0 {
		scopes, names, sizes = append(scopes, "NATIONAL"), append(names, ""), append(sizes, national)
	}
	for city, size := range cities {
		scopes, names, sizes = append(scopes, "CITY"), append(names, city), append(sizes, size)
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, rankingPruneRange, nilIfInvalid(low), nil, []pgtype.UUID{}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, rankingPopulationUpsert, scopes, names, sizes); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, rankingPopulationPrune, scopes, names)
		return err
	})
}

// nilIfInvalid is an open bound for the first chunk.
func nilIfInvalid(id pgtype.UUID) any {
	if !id.Valid {
		return nil
	}
	return id
}
