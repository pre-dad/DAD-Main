package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/store"
	"dad/donor-api/internal/wallet"
)

type walletDonor struct {
	DonorSource
	link *string
}

func (s walletDonor) Sharing(_ context.Context, token string) (store.Sharing, error) {
	if token != "valid" {
		return store.Sharing{}, store.ErrDenied
	}
	return store.Sharing{IRLLink: s.link}, nil
}

func TestWalletHTTPRequiresSessionOriginAndSharing(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &wallet.Issuer{
		Origin: "https://dad.example",
		Google: &wallet.Google{Email: "wallet@test.iam.gserviceaccount.com", ClassID: "123.dad", Key: key},
	}
	link := "https://dad.example/r/code?via=irl"
	for _, tc := range []struct {
		path, method, token, origin string
		link                        *string
		status                      int
	}{
		{"/donor/me/wallet", "GET", "", "", &link, 401},
		{"/donor/me/wallet", "GET", "valid", "", &link, 200},
		{"/donor/me/wallet/google", "POST", "valid", "https://other.example", &link, 403},
		{"/donor/me/wallet/google", "POST", "", "https://dad.example", &link, 401},
		{"/donor/me/wallet/google", "POST", "valid", "https://dad.example", nil, 409},
		{"/donor/me/wallet/google", "POST", "valid", "https://dad.example", &link, 200},
		{"/donor/me/wallet/apple", "POST", "valid", "https://dad.example", &link, 503},
	} {
		server := NewServer("", Services{Origin: issuer.Origin, Wallet: issuer, Donor: walletDonor{link: tc.link}})
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Origin", tc.origin)
		request.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: tc.token})
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" ||
			strings.Contains(response.Body.String(), "PRIVATE KEY") {
			t.Errorf(
				"%s %s session %q origin %q: got %d %s, want %d",
				tc.method,
				tc.path,
				tc.token,
				tc.origin,
				response.Code,
				response.Body,
				tc.status,
			)
		}
		if tc.status == 200 && tc.method == "GET" && strings.TrimSpace(response.Body.String()) != `{"apple":false,"google":true}` {
			t.Errorf("%s: got %s, want the configured providers", tc.path, response.Body)
		}
		if tc.status == 200 && tc.method == "POST" && !strings.Contains(response.Body.String(), "https://pay.google.com/gp/v/save/") {
			t.Errorf("%s: got %s, want a signed Google Wallet link", tc.path, response.Body)
		}
	}
}

func TestApplePassDownloadIsAnAttachment(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Synthetic test CA", OrganizationalUnit: []string{"TESTTEAM"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &wallet.Issuer{
		Origin: "https://dad.example",
		Apple:  &wallet.Apple{Certificate: cert, Intermediate: cert, Key: key, PassType: "pass.org.example.dad", Team: "TESTTEAM"},
	}
	link := "https://dad.example/r/code?via=irl"
	server := NewServer("", Services{Origin: issuer.Origin, Wallet: issuer, Donor: walletDonor{link: &link}})
	request := httptest.NewRequest("POST", "/donor/me/wallet/apple", nil)
	request.Header.Set("Origin", issuer.Origin)
	request.AddCookie(&http.Cookie{Name: "dad_donor_session", Value: "valid"})
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		response.Header().Get("Content-Type") != "application/vnd.apple.pkpass" ||
		!strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") ||
		!strings.HasPrefix(response.Body.String(), "PK") {
		t.Fatalf("got %d %v, want a pkpass attachment", response.Code, response.Header())
	}
}
