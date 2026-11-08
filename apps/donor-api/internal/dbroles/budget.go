package dbroles

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// ReplicasVariable lists the most instances of each process that may run
	// at once, such as "api=16,projections=2". Roles it omits use DefaultReplicas.
	ReplicasVariable = "DONOR_ROLE_REPLICAS"
	// BudgetVariable overrides DefaultBudget.
	BudgetVariable = "DONOR_ROLE_CONNECTION_BUDGET"

	// DefaultBudget is the most server connections all logins together may
	// hold. RDS sets max_connections to DBInstanceClassMemory/9531392, about 900
	// on db.t3.large (8 GiB) and 1,800 on db.m6g.xlarge (16 GiB). Take off the 3
	// reserved for superusers and about 45 for the migration, the owner's
	// sessions, RDS's own, and a person at psql. 850 keeps that margin on a
	// db.t3.large; raise it with the instance class.
	DefaultBudget = 850
)

// DefaultReplicas is how many instances of a process its login admits when
// ReplicasVariable does not say. The API's is the production autoscaling
// ceiling of 12 tasks (DAD-infra, apps/dad-main/lib/config.ts) plus 4 for the
// extra tasks a rolling deployment starts before it stops the old ones. Every
// other process runs one instance, so 2 covers a deployment and a one-off run.
func DefaultReplicas(role Role) int32 {
	if role.Name == API.Name {
		return 16
	}
	return 2
}

// ParseReplicas reads a list such as "api=16,push=3" into replica counts by
// Role.Key. It refuses an unknown role, a count below one, or a repeat.
func ParseReplicas(list string) (map[string]int32, error) {
	replicas := map[string]int32{}
	if strings.TrimSpace(list) == "" {
		return replicas, nil
	}
	for _, entry := range strings.Split(list, ",") {
		key, count, found := strings.Cut(strings.TrimSpace(entry), "=")
		n, err := strconv.ParseInt(count, 10, 32)
		switch {
		case !found || err != nil || n < 1:
			return nil, fmt.Errorf("%s entry %q must be role=count with a count of at least 1", ReplicasVariable, entry)
		case !knownKey(key):
			return nil, fmt.Errorf("%s names unknown role %q", ReplicasVariable, key)
		}
		if _, repeated := replicas[key]; repeated {
			return nil, fmt.Errorf("%s names %q twice", ReplicasVariable, key)
		}
		replicas[key] = int32(n)
	}
	return replicas, nil
}

func knownKey(key string) bool {
	for _, role := range Roles {
		if role.Key() == key {
			return true
		}
	}
	return false
}

// ParseBudget reads BudgetVariable's value; empty means DefaultBudget.
func ParseBudget(value string) (int32, error) {
	if strings.TrimSpace(value) == "" {
		return DefaultBudget, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number of connections", BudgetVariable)
	}
	return int32(n), nil
}

// Plan sets each login's replicas, from replicas or else DefaultReplicas, and
// refuses when the connection limits together exceed budget, since the
// database could then not admit every login's full allowance.
func Plan(logins []Login, replicas map[string]int32, budget int32) error {
	var total int64
	for i := range logins {
		logins[i].Replicas = DefaultReplicas(logins[i].Role)
		if n, set := replicas[logins[i].Role.Key()]; set {
			logins[i].Replicas = n
		}
		total += int64(logins[i].ConnectionLimit())
	}
	if total > int64(budget) {
		return fmt.Errorf(
			"connection limits total %d, over the budget of %d (%s); lower %s or the pool sizes, or raise the budget with the database's max_connections",
			total,
			budget,
			BudgetVariable,
			ReplicasVariable,
		)
	}
	return nil
}
