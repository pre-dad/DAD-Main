package httpapi

import (
	"context"
	"net/http"
	"time"

	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
)

type ExportSource interface {
	ExportDatasets(context.Context, string) (store.ExportDatasets, error)
	RequestExport(context.Context, string, string) (store.ExportJob, error)
	ExportJob(context.Context, string, pgtype.UUID) (store.ExportJob, error)
	ExportJobs(context.Context, string) (store.ExportJobs, error)
	OpenExport(context.Context, string, pgtype.UUID) (store.ExportDownload, error)
}

const (
	// downloadTimeout bounds a whole download. The server's write timeout is
	// too short for a large file, so each write extends its own deadline.
	downloadTimeout = 10 * time.Minute
	downloadWindow  = 30 * time.Second
)

// exportWriter sends the CSV headers only once data is ready, so a failure
// before the first byte can still be reported as a JSON error.
type exportWriter struct {
	response http.ResponseWriter
	control  *http.ResponseController
	name     string
	started  bool
}

func (w *exportWriter) Write(data []byte) (int, error) {
	if !w.started {
		header := w.response.Header()
		header.Set("Content-Type", "text/csv; charset=utf-8")
		header.Set("Content-Disposition", `attachment; filename="dad-`+w.name+`.csv"`)
		header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.started = true
	}
	_ = w.control.SetWriteDeadline(time.Now().Add(downloadWindow))
	return w.response.Write(data)
}

func (a *api) exports() service {
	return service{a.Exports != nil, "Exports are temporarily unavailable", readTimeout, staffStatus}
}

func (a *api) exportDatasets(w http.ResponseWriter, r *http.Request, session string) {
	a.exports().serve(w, r, func(ctx context.Context) (any, error) { return a.Exports.ExportDatasets(ctx, session) })
}

func (a *api) requestExport(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Dataset string `json:"dataset"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.exports().serve(w, r, func(ctx context.Context) (any, error) { return a.Exports.RequestExport(ctx, session, body.Dataset) })
}

func (a *api) exportJobs(w http.ResponseWriter, r *http.Request, session string) {
	a.exports().serve(w, r, func(ctx context.Context) (any, error) { return a.Exports.ExportJobs(ctx, session) })
}

func (a *api) exportJob(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "job_id")
	if !ok {
		return
	}
	a.exports().missing("Export not found").serve(w, r, func(ctx context.Context) (any, error) {
		return a.Exports.ExportJob(ctx, session, id)
	})
}

// downloadExport streams a finished export. It is not rate limited like a
// mutation: each download is audited, and only the requester may fetch it.
func (a *api) downloadExport(w http.ResponseWriter, r *http.Request, session string) {
	id, ok := pathID(w, r, "job_id")
	if !ok {
		return
	}
	if !a.exports().available(w, r) {
		return
	}
	ctx, cancel := withTimeout(r, downloadTimeout)
	defer cancel()
	file, err := a.Exports.OpenExport(ctx, session, id)
	if err != nil {
		respond(w, r, nil, err, a.exports().missing("Export not found").classify)
		return
	}
	writer := &exportWriter{response: w, control: http.NewResponseController(w), name: file.Dataset}
	if err = file.WriteTo(ctx, writer); err != nil {
		if writer.started {
			// Abort a partial download; a truncated CSV must not look successful.
			panic(http.ErrAbortHandler)
		}
		respond(w, r, nil, err, staffStatus)
	}
}
