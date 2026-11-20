package httpapi

import (
	"context"
	"errors"
	"net/http"

	"dad/donor-api/internal/store"
)

type FoundingClaimSource interface {
	Authorize(context.Context, string) error
	Import(context.Context, string, string) (store.FoundingClaimImport, error)
}

const maxFoundingClaimBody = 5 << 20

type foundingClaimBody struct {
	CSV string `json:"claims_csv"`
}

// importFoundingClaims authorizes before reading its large body, as publish does.
func (a *api) importFoundingClaims(w http.ResponseWriter, r *http.Request, session string) {
	if !(service{a.FoundingClaims != nil, "Badge administration is temporarily unavailable", staffTimeout, staffStatus}).available(w, r) {
		return
	}
	authorizeCtx, cancelAuthorize := withTimeout(r, staffTimeout)
	err := a.FoundingClaims.Authorize(authorizeCtx, session)
	cancelAuthorize()
	if err != nil {
		respond(w, r, nil, err, staffStatus)
		return
	}
	var body foundingClaimBody
	if !decodeJSONLimit(w, r, &body, maxFoundingClaimBody) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	result, err := a.FoundingClaims.Import(ctx, session, body.CSV)
	if refused, ok := errors.AsType[*store.PublicationError](err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, problem{Detail: "The spreadsheet has problems", Errors: refused.Problems})
		return
	}
	respond(w, r, result, err, staffStatus)
}
