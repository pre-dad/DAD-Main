package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dad/donor-api/internal/dbroles"
	"dad/donor-api/internal/mail"
	"dad/donor-api/internal/migrations"
	"dad/donor-api/internal/payments"
	"dad/donor-api/internal/push"
	"dad/donor-api/internal/store"
	"dad/donor-api/internal/store/queries"
	"dad/donor-api/internal/workers"
)

// configError messages are written by this program and are safe to log.
type configError string

func (e configError) Error() string { return string(e) }

func run(ctx context.Context, args []string, getenv func(string) string) error {
	flags := flag.NewFlagSet("donor-worker", flag.ContinueOnError)
	kinds := strings.Join(slices.Sorted(maps.Keys(dbroles.Workers)), ", ")
	kind := flags.String("kind", "", kinds)
	once := flags.Bool("once", false, "process at most one item")
	concurrency := flags.Int("concurrency", 1, "parallel claim loops for queue workers (1-"+strconv.Itoa(maxConcurrency)+")")
	retry := flags.String("retry-event", "", "reset an unprocessed Stripe event's attempt budget")
	if err := flags.Parse(args); err != nil {
		return configError("invalid worker arguments")
	}
	role, known := dbroles.Workers[*kind]
	if flags.NArg() != 0 || !known {
		return configError("--kind must be one of " + kinds)
	}
	if *retry != "" && (*kind != "stripe" || !strings.HasPrefix(*retry, "evt_")) {
		return configError("--retry-event requires --kind stripe and an evt_ identifier")
	}
	if *concurrency < 1 || *concurrency > maxConcurrency || (*concurrency > 1 && !parallelSafe[*kind]) {
		return configError(
			"--concurrency must be 1, or 2 to " + strconv.Itoa(maxConcurrency) + " for login-email, email-changes, messages and push",
		)
	}
	if getenv("DONOR_GO_WORKERS_ENABLED") != "true" {
		return configError("workers require DONOR_GO_WORKERS_ENABLED=true")
	}
	dsn, err := role.URL(getenv)
	if err != nil {
		return configError(err.Error())
	}
	if dsn == "" {
		return configError(role.Variable() + " is required")
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	pool, err := store.Open(startup, dsn, role.Pool)
	cancel()
	if err != nil {
		return configError("worker database is unreachable")
	}
	defer pool.Close()
	if int32(*concurrency) > pool.Config().MaxConns {
		return configError("--concurrency exceeds the pool size; raise pool_max_conns in " + role.Variable() + " and re-run provisioning")
	}
	check, finish := context.WithTimeout(ctx, 10*time.Second)
	defer finish()
	if err = migrations.Verify(check, pool); err != nil {
		return configError("donor schema is not current; run migrations before starting workers")
	}
	if err = role.Check(check, pool, getenv(dbroles.AllowOwnerVariable) == "true"); err != nil {
		return configError(err.Error())
	}
	if *retry != "" {
		retryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		count, err := queries.New(pool).RetryStripeEvent(retryCtx, *retry)
		cancel()
		if err != nil || count != 1 {
			return configError("expected an unprocessed Stripe event")
		}
	}
	step, err := stepFor(*kind, pool, getenv)
	if err != nil {
		return err
	}
	budget := workers.StepBudget
	if *kind == "stripe" {
		budget = workers.ProcessorStepBudget
	}
	return workers.RunConcurrent(ctx, *concurrency, *once, 2*time.Second, budget, step)
}

const maxConcurrency = 8

// parallelSafe lists the kinds whose steps claim a single outbox row with
// FOR UPDATE SKIP LOCKED, so parallel loops never deliver the same row twice.
var parallelSafe = map[string]bool{"login-email": true, "email-changes": true, "messages": true, "push": true}

type deliverer interface {
	DeliverNext(context.Context, time.Time) (bool, error)
}

func stepFor(kind string, pool *pgxpool.Pool, getenv func(string) string) (func(context.Context) (bool, error), error) {
	switch kind {
	case "stripe":
		gateway, err := payments.NewStripeGateway(getenv("STRIPE_SECRET_KEY"))
		if err != nil {
			return nil, configError("STRIPE_SECRET_KEY must be a Stripe test-mode secret key")
		}
		ceiling, err := payments.RequestsPerSecond(getenv)
		if err != nil {
			return nil, configError(err.Error())
		}
		gateway.UseBudget(store.StripeBudget{DB: pool, PerSecond: ceiling})
		events := store.EventWorker{DB: pool, Gateway: gateway}
		backfill := &store.AnchorBackfill{DB: pool, Gateway: gateway}
		reversals := store.DeletedAccountReversals{DB: pool, Gateway: gateway}
		return func(ctx context.Context) (bool, error) {
			worked, err := events.ProcessNext(ctx)
			if worked || err != nil {
				return worked, err
			}
			if worked, err = reversals.RunNext(ctx, time.Now().UTC()); worked || err != nil {
				return worked, err
			}
			return backfill.SyncNext(ctx, time.Now().UTC())
		}, nil
	case "projections":
		retention := &store.TouchRetention{DB: pool}
		rankings := store.RankingService{DB: pool}
		reminders := store.EventReminderService{DB: pool}
		monthlyPartners := store.PartnerMonthlyService{DB: pool}
		nearby := store.EventAlertService{DB: pool}
		teamMonths := store.TeamMonthService{DB: pool}
		exports := &store.ExportWorker{DB: pool}
		return func(ctx context.Context) (bool, error) {
			now := time.Now().UTC()
			return runAll(ctx,
				func(ctx context.Context) (bool, error) { return retention.PurgeNext(ctx, now) },
				// Staff exports share this worker (no extra service to deploy): one
				// bounded batch per step, so projections are never starved.
				func(ctx context.Context) (bool, error) { return exports.RunNext(ctx, now), nil },
				func(ctx context.Context) (bool, error) {
					return counted(store.CatchUpProjections(ctx, pool, func() time.Time { return time.Now().UTC() }, store.CatchUpSlice))
				},
				func(ctx context.Context) (bool, error) { return rankings.RefreshIfDue(ctx, now) },
				func(ctx context.Context) (bool, error) { return nearby.QueueNext(ctx, now) },
				// Idempotent per window, so a frequent sweep is cheap and each 24h/2h window fires once.
				func(ctx context.Context) (bool, error) { return counted(reminders.QueueDueReminders(ctx, now)) },
				// Idempotent per partner per calendar month via the intent's dedup key.
				func(ctx context.Context) (bool, error) { return counted(monthlyPartners.QueueDueMonthly(ctx, now)) },
				func(ctx context.Context) (bool, error) { return counted(teamMonths.CloseDue(ctx, now)) },
			)
		}, nil
	case "push":
		config, err := push.FromEnvironment(getenv)
		if err != nil {
			return nil, configError(err.Error())
		}
		// Without a provider the worker still runs, so a deployment can start
		// every worker: it completes notifications for unconfigured platforms
		// as suppressed instead of letting them wait.
		providers := config.Providers()
		for platform, feature := range map[string]string{push.IOS: "iOS app notifications (DONOR_PUSH_APNS_*)", push.Android: "Android app notifications (DONOR_PUSH_FCM_KEY_FILE)"} {
			if providers[platform] == nil {
				slog.Info("feature disabled until configured", "feature", feature)
			}
		}
		worker := store.PushWorker{DB: pool, Providers: providers}
		partner := store.PartnerPushWorker{DB: pool, Providers: providers}
		return func(ctx context.Context) (bool, error) {
			now := time.Now().UTC()
			return runAll(ctx, func(ctx context.Context) (bool, error) { return worker.DeliverNext(ctx, now) },
				func(ctx context.Context) (bool, error) { return partner.DeliverNext(ctx, now) })
		}, nil
	}
	settings, err := mail.FromEnvironment(getenv)
	if err != nil || settings == nil {
		return nil, configError("SMTP settings are missing or invalid")
	}
	origin := getenv("DONOR_PUBLIC_ORIGIN")
	if err := payments.ValidateOrigin(origin); err != nil {
		return nil, configError("DONOR_PUBLIC_ORIGIN, the origin of email links, " + err.Error())
	}
	roots, err := settings.RootCAs()
	if err != nil {
		return nil, configError("DONOR_SMTP_CA could not be loaded")
	}
	transport := mail.SMTPTransport{Settings: *settings, RootCAs: roots}
	var queues []deliverer
	switch kind {
	case "email-changes":
		queues = []deliverer{store.EmailChangeWorker{DB: pool, Settings: *settings, Origin: origin, Transport: transport}}
	case "login-email":
		queues = []deliverer{
			store.LoginEmailWorker{DB: pool, Settings: *settings, Origin: origin, Transport: transport},
			store.PartnerLoginEmailWorker{DB: pool, Settings: *settings, Origin: origin, Transport: transport},
		}
	default:
		queues = []deliverer{
			store.MessageWorker{DB: pool, Settings: *settings, Origin: origin, Transport: transport},
			store.PartnerMessageWorker{DB: pool, Settings: *settings, Origin: origin, Transport: transport},
		}
	}
	return rotating(queues), nil
}

// rotating delivers at most one item per step. It starts at a different queue each step, so a busy or failing
// queue cannot starve the ones after it.
func rotating(queues []deliverer) func(context.Context) (bool, error) {
	var start atomic.Uint64
	return func(ctx context.Context) (bool, error) {
		now := time.Now().UTC()
		first := int(start.Add(1) % uint64(len(queues)))
		var failures error
		for i := range queues {
			worked, err := queues[(first+i)%len(queues)].DeliverNext(ctx, now)
			if worked {
				return true, err
			}
			failures = errors.Join(failures, err)
		}
		return false, failures
	}
}

func counted[N ~int | ~int64](n N, err error) (bool, error) { return n > 0, err }

// runAll runs every step even when an earlier one fails, so one persistent error cannot starve the others.
func runAll(ctx context.Context, steps ...func(context.Context) (bool, error)) (bool, error) {
	var worked bool
	var failures error
	for _, step := range steps {
		stepWorked, err := step(ctx)
		worked = worked || stepWorked
		failures = errors.Join(failures, err)
	}
	return worked, failures
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], os.Getenv)
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if reason, ok := errors.AsType[configError](err); ok {
		slog.Error("donor worker stopped", "reason", string(reason))
	} else {
		// Database errors can carry row values. Keep service logs free of donor data.
		slog.Error("donor worker stopped; inspect configuration and inbox state")
	}
	os.Exit(1)
}
