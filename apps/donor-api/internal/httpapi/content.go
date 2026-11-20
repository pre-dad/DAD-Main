package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"dad/donor-api/internal/store"
)

type ContentAdminSource interface {
	AuthorizeSourceUpload(context.Context, string) error
	Inventory(context.Context, string) (store.ContentInventory, error)
	StoreSource(context.Context, string, string, []byte) (store.StoredSource, error)
	ReadSource(context.Context, string, pgtype.UUID) (store.ContentDocument, error)
}

const maxSourceBytes = 10 << 20

func (a *api) contentAdmin() service {
	return service{a.ContentAdmin != nil, "Content administration is temporarily unavailable", readTimeout, staffStatus}
}

func (a *api) contentInventory(w http.ResponseWriter, r *http.Request, session string) {
	a.contentAdmin().serve(w, r, func(ctx context.Context) (any, error) { return a.ContentAdmin.Inventory(ctx, session) })
}

// Uploads are authorized before the body is read so anonymous clients cannot
// make the API buffer large documents.
func (a *api) uploadSource(w http.ResponseWriter, r *http.Request, session string) {
	name := r.URL.Query().Get("name")
	if strings.ContainsRune(name, 0) {
		invalidFields(w, r)
		return
	}
	if !a.contentAdmin().available(w, r) {
		return
	}
	authorizeCtx, cancelAuthorize := withTimeout(r, staffTimeout)
	err := a.ContentAdmin.AuthorizeSourceUpload(authorizeCtx, session)
	cancelAuthorize()
	if err != nil {
		respond(w, r, nil, err, staffStatus)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSourceBytes))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Source document exceeds 10 MB", nil)
		return
	}
	if err != nil {
		invalidFields(w, r)
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	body, err := a.ContentAdmin.StoreSource(ctx, session, name, data)
	respond(w, r, body, err, staffStatus)
}

func (a *api) downloadSource(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "source_id")
	if !ok {
		return
	}
	if !a.contentAdmin().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, staffTimeout)
	defer cancel()
	document, err := a.ContentAdmin.ReadSource(ctx, session, id)
	if err != nil {
		respond(w, r, nil, err, staffStatus)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": document.Name}))
	_, _ = w.Write(document.Data)
}
