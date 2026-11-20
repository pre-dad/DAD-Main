package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
)

// EmployeeDonorSource is the donor-facing employee surface (EM-2/EM-4/EM-5).
type EmployeeDonorSource interface {
	Enroll(ctx context.Context, token, code string) (string, error)
	Team(ctx context.Context, token string) (store.DonorTeam, error)
	JoinTeam(ctx context.Context, token, name string) (string, error)
}

// EmployeePartnerSource is the partner-facing employee surface (EM-3/EM-5/EM-9).
type EmployeePartnerSource interface {
	OurPeople(ctx context.Context, token string) (store.PartnerOurPeople, error)
	Teams(ctx context.Context, token string) ([]store.EmployeeTeamRow, error)
	SetWorkforce(ctx context.Context, token string, size int32) error
}

// EmployeeStaffSource is the staff-side matching surface (EM-6/EM-7).
type EmployeeStaffSource interface {
	Match(ctx context.Context, token string, partnerID pgtype.UUID, year int32) (store.MatchStatus, error)
	RecordMatchPayment(ctx context.Context, token string, partnerID pgtype.UUID, year int32, amountMinor int64, requestID pgtype.UUID) error
}

func employeeStatus(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrEmployeeLink):
		return http.StatusUnprocessableEntity, "That company code is not recognised"
	case errors.Is(err, store.ErrEmployerLinked):
		return http.StatusConflict, "Your account is already linked to another employer"
	case errors.Is(err, store.ErrTeam):
		return http.StatusUnprocessableEntity, "Enter a team name of up to 80 characters"
	default:
		return donorStatus(err)
	}
}

func (a *api) employeeDonor() service {
	return service{a.EmployeeDonor != nil, "Employee giving is temporarily unavailable", readTimeout, employeeStatus}
}

func (a *api) employeePartner() service {
	return service{a.EmployeePartner != nil, "Employee giving is temporarily unavailable", readTimeout, partnerStatusCode}
}

func (a *api) employeeStaff() service {
	return service{a.EmployeeStaff != nil, "Partner matching is temporarily unavailable", staffTimeout, staffStatus}
}

func (a *api) employeeEnroll(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		CompanyCode string `json:"company_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.employeeDonor().serve(w, r, func(ctx context.Context) (any, error) {
		org, err := a.EmployeeDonor.Enroll(ctx, session, body.CompanyCode)
		if err != nil {
			return nil, err
		}
		return map[string]any{"organization": org}, nil
	})
}

func (a *api) employeeTeam(w http.ResponseWriter, r *http.Request, session string) {
	a.employeeDonor().serve(w, r, func(ctx context.Context) (any, error) { return a.EmployeeDonor.Team(ctx, session) })
}

func (a *api) employeeJoinTeam(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.employeeDonor().serve(w, r, func(ctx context.Context) (any, error) {
		id, err := a.EmployeeDonor.JoinTeam(ctx, session, body.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"team_id": id}, nil
	})
}

func (a *api) employeeOurPeople(w http.ResponseWriter, r *http.Request, session string) {
	a.employeePartner().serve(w, r, func(ctx context.Context) (any, error) { return a.EmployeePartner.OurPeople(ctx, session) })
}

func (a *api) employeeTeams(w http.ResponseWriter, r *http.Request, session string) {
	a.employeePartner().serve(w, r, func(ctx context.Context) (any, error) {
		rows, err := a.EmployeePartner.Teams(ctx, session)
		return map[string]any{"teams": rows}, err
	})
}

func (a *api) employeeWorkforce(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		WorkforceSize int32 `json:"workforce_size"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.employeePartner().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"saved": true}, a.EmployeePartner.SetWorkforce(ctx, session, body.WorkforceSize)
	})
}

// validYear bounds a matching year to the platform's lifetime.
func validYear(year int) bool { return year >= 2024 && year < 3000 }

func yearParam(w http.ResponseWriter, r *http.Request) (int32, bool) {
	year, err := strconv.Atoi(r.URL.Query().Get("year"))
	if err != nil || !validYear(year) {
		invalidFields(w, r)
		return 0, false
	}
	return int32(year), true
}

func (a *api) staffPartnerMatchStatus(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	year, ok := yearParam(w, r)
	if !ok {
		return
	}
	a.employeeStaff().serve(w, r, func(ctx context.Context) (any, error) {
		return a.EmployeeStaff.Match(ctx, session, id, year)
	})
}

func (a *api) staffPartnerMatchPayment(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "partnerID")
	if !ok {
		return
	}
	var body struct {
		Year        int32 `json:"year"`
		AmountMinor int64 `json:"amount_minor"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validYear(int(body.Year)) || body.AmountMinor <= 0 {
		invalidFields(w, r)
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	a.employeeStaff().serve(w, r, func(ctx context.Context) (any, error) {
		return map[string]any{"recorded": true}, a.EmployeeStaff.RecordMatchPayment(ctx, session, id, body.Year, body.AmountMinor, key)
	})
}
