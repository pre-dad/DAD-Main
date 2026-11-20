package httpapi

import (
	"context"
	"net/http"
	"time"
)

const walletNeedsName = "Choose a public nickname before adding your code to Wallet"

func (a *api) walletProviders(w http.ResponseWriter, r *http.Request, session string) {
	a.donor().serve(w, r, func(ctx context.Context) (any, error) {
		// Sharing is read only to validate the session.
		_, err := a.Donor.Sharing(ctx, session)
		return map[string]bool{
			"apple":  a.Wallet != nil && a.Wallet.Apple != nil,
			"google": a.Wallet != nil && a.Wallet.Google != nil,
		}, err
	})
}

func (a *api) googlePass(w http.ResponseWriter, r *http.Request, session string) {
	if a.Wallet == nil || a.Wallet.Google == nil {
		writeError(w, r, http.StatusServiceUnavailable, "Google Wallet is unavailable", nil)
		return
	}
	link, ok := a.irlLink(w, r, session, walletNeedsName)
	if !ok {
		return
	}
	url, err := a.Wallet.GoogleURL(link, time.Now())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "Google Wallet is unavailable", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

func (a *api) applePass(w http.ResponseWriter, r *http.Request, session string) {
	if a.Wallet == nil || a.Wallet.Apple == nil {
		writeError(w, r, http.StatusServiceUnavailable, "Apple Wallet is unavailable", nil)
		return
	}
	link, ok := a.irlLink(w, r, session, walletNeedsName)
	if !ok {
		return
	}
	data, err := a.Wallet.ApplePass(link, time.Now())
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "Apple Wallet is unavailable", err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.pkpass")
	w.Header().Set("Content-Disposition", `attachment; filename="dad-referral.pkpass"`)
	_, _ = w.Write(data)
}
