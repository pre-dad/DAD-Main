package wallet

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
)

func testIssuer(t *testing.T) (*Issuer, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Synthetic test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			OrganizationalUnit: []string{"TESTTEAM"},
			ExtraNames: []pkix.AttributeTypeAndValue{
				{Type: asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}, Value: "pass.org.example.dad"},
			},
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	apple, err := newApple(
		der,
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		caDER,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Issuer{
		Origin: "https://dad.example",
		Apple:  apple,
		Google: &Google{Email: "wallet@test.iam.gserviceaccount.com", ClassID: "123.dad", Key: key},
	}, now
}

func TestSignedAppleBundleAndGoogleClaims(t *testing.T) {
	issuer, now := testIssuer(t)
	link := "https://dad.example/r/public-code?via=irl"
	data, err := issuer.ApplePass(link, now)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range archive.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != 6 {
		t.Fatal("unexpected pass contents", len(files))
	}
	var manifest map[string]string
	if err = json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	for name, digest := range manifest {
		sum := sha1.Sum(files[name])
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatal("manifest does not cover file", name)
		}
	}
	signed, err := pkcs7.Parse(files["signature"])
	if err != nil {
		t.Fatal(err)
	}
	signed.Content = files["manifest.json"]
	if err = signed.Verify(); err != nil {
		t.Fatal("invalid signature", err)
	}
	if len(signed.Certificates) != 2 {
		t.Fatal("missing intermediate")
	}
	var pass struct {
		SerialNumber       string
		TeamIdentifier     string
		PassTypeIdentifier string
		Barcodes           []struct{ Message string }
	}
	if err = json.Unmarshal(files["pass.json"], &pass); err != nil || pass.TeamIdentifier != "TESTTEAM" ||
		pass.PassTypeIdentifier != "pass.org.example.dad" ||
		len(pass.Barcodes) != 1 ||
		pass.Barcodes[0].Message != link {
		t.Fatal(pass, err)
	}
	url, err := issuer.GoogleURL(link, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(url, "https://pay.google.com/gp/v/save/"), ".")
	if len(parts) != 3 {
		t.Fatal(url)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(&issuer.Google.Key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iss, Aud, Typ string
		Origins       []string
		Payload       struct {
			GenericObjects []struct {
				ID, ClassID string
				Barcode     struct{ Value string }
			}
		}
	}
	if err = json.Unmarshal(payload, &claims); err != nil || claims.Aud != "google" || claims.Typ != "savetowallet" ||
		claims.Iss != issuer.Google.Email ||
		len(claims.Origins) != 1 ||
		claims.Origins[0] != issuer.Origin {
		t.Fatal(claims, err)
	}
	object := claims.Payload.GenericObjects[0]
	if object.Barcode.Value != link || object.ClassID != "123.dad" || !strings.HasSuffix(object.ID, pass.SerialNumber) {
		t.Fatal(object)
	}
	for _, raw := range [][]byte{payload, files["pass.json"]} {
		if !bytes.Contains(raw, []byte("$30/month")) || !bytes.Contains(raw, []byte("April 2027")) ||
			bytes.Contains(raw, []byte("donor_id")) {
			t.Fatal("disclosure missing or internal identity exposed")
		}
	}
}

func TestWalletRejectsWrongURLsAndExpiredCertificate(t *testing.T) {
	issuer, now := testIssuer(t)
	for _, link := range []string{"https://evil.example/r/code?via=irl", "https://dad.example/r/code", "https://dad.example/r/code?via=link", "https://user@dad.example/r/code?via=irl", "https://dad.example/r/code?via=irl#fragment"} {
		if _, err := issuer.ApplePass(link, now); err == nil {
			t.Fatal(link)
		}
		if _, err := issuer.GoogleURL(link, now); err == nil {
			t.Fatal(link)
		}
	}
	if _, err := issuer.ApplePass("https://dad.example/r/code?via=irl", now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired signer accepted")
	}
	var absent *Issuer
	if _, err := absent.ApplePass("", now); err != ErrUnavailable {
		t.Fatal(err)
	}
	if value, err := FromEnvironment(func(string) string { return "" }, "http://localhost:3000"); err != nil || value != nil {
		t.Fatal(value, err)
	}
	if _, err := FromEnvironment(func(k string) string {
		if k == "DONOR_WALLET_GOOGLE_CLASS_ID" {
			return "123.dad"
		}
		return ""
	}, "https://dad.example"); err == nil {
		t.Fatal("partial credentials accepted")
	}
}

func TestWalletConfigurationLoadsPrivateFilesAndRejectsInsecureOrigin(t *testing.T) {
	issuer, now := testIssuer(t)
	dir := t.TempDir()
	files := map[string][]byte{
		"DONOR_WALLET_APPLE_CERT_FILE": issuer.Apple.Certificate.Raw,
		"DONOR_WALLET_APPLE_WWDR_FILE": issuer.Apple.Intermediate.Raw,
		"DONOR_WALLET_APPLE_KEY_FILE": pem.EncodeToMemory(
			&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(issuer.Apple.Key)},
		),
	}
	serviceAccount, err := json.Marshal(
		map[string]string{
			"type":         "service_account",
			"client_email": issuer.Google.Email,
			"private_key":  string(pkcs8Pem(t, issuer.Apple.Key)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	files["DONOR_WALLET_GOOGLE_KEY_FILE"] = serviceAccount
	settings := map[string]string{"DONOR_WALLET_GOOGLE_CLASS_ID": issuer.Google.ClassID}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		settings[name] = path
	}
	getenv := func(name string) string { return settings[name] }
	loaded, err := FromEnvironment(getenv, issuer.Origin)
	if err != nil || loaded.Apple == nil || loaded.Google == nil {
		t.Fatal(loaded, err)
	}
	if _, err = loaded.ApplePass("https://dad.example/r/test?via=irl", now); err != nil {
		t.Fatal(err)
	}
	if _, err = FromEnvironment(getenv, "http://dad.example"); err == nil {
		t.Fatal("insecure issuance origin accepted")
	}
	if _, err = newApple(issuer.Apple.Certificate.Raw, files["DONOR_WALLET_APPLE_KEY_FILE"], issuer.Apple.Certificate.Raw, now); err == nil {
		t.Fatal("invalid certificate chain accepted")
	}
}

func TestPrivateKeyFormats(t *testing.T) {
	rsaKey := func(bits int) *rsa.PrivateKey {
		key, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	pkcs8 := func(key any) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	small := rsaKey(1024)
	good := rsaKey(2048)
	if got, err := privateKey(pkcs8(good)); err != nil || !got.Equal(good) {
		t.Fatal("PKCS#8 RSA key rejected", err)
	}
	for name, data := range map[string][]byte{
		"1024-bit PKCS#8":  pkcs8(small),
		"1024-bit PKCS#1":  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}),
		"ed25519 PKCS#8":   pkcs8(edKey),
		"unknown PEM type": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(good)}),
	} {
		if _, err := privateKey(data); !errors.Is(err, ErrConfiguration) {
			t.Errorf("%s: got %v, want ErrConfiguration", name, err)
		}
	}
}

func pkcs8Pem(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
