// Package mail delivers sandbox account and financial messages over verified TLS.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"dad/donor-api/internal/credentials"
)

var ErrDelivery = errors.New("email delivery failed")
var ErrConfiguration = errors.New("invalid email configuration")

type Settings struct {
	Host                       string
	Port                       int
	Username, Password, Sender string
	EncryptionKeys             credentials.Keyring
	// CAPath, when set, is a PEM file whose certificates are the only roots
	// trusted for the STARTTLS handshake, replacing the system trust store.
	// It exists for a mail server with a private CA — a local capture inbox
	// (Mailpit) in development, or an internal relay in production. STARTTLS
	// and full certificate verification remain mandatory either way; this
	// never enables cleartext or skips verification.
	CAPath string
}

// RootCAs builds the certificate pool for the STARTTLS handshake. It returns
// nil (verify against the system trust store) when no CAPath is set, and an
// error if the file is missing or holds no valid certificate.
func (s Settings) RootCAs() (*x509.CertPool, error) {
	if s.CAPath == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(s.CAPath)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read DONOR_SMTP_CA", ErrConfiguration)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: DONOR_SMTP_CA holds no valid certificate", ErrConfiguration)
	}
	return pool, nil
}

type Message struct{ From, To, Subject, Body, ID string }
type Transport interface {
	Send(context.Context, Message) error
}

func address(value string) (*netmail.Address, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, ErrConfiguration
	}
	parsed, err := netmail.ParseAddress(value)
	if err != nil || !strings.Contains(parsed.Address, "@") {
		return nil, ErrConfiguration
	}
	return parsed, nil
}

// Validate checks everything a delivery worker needs: the server, the
// sender, and the key that encrypts queued sign-in links.
func (s Settings) Validate() error {
	if err := s.validateServer(); err != nil {
		return err
	}
	if !s.EncryptionKeys.Configured() {
		return ErrConfiguration
	}
	return nil
}

func validAddress(value string) bool {
	_, err := address(value)
	return err == nil
}

func (s Settings) validateServer() error {
	if s.Host == "" || strings.ContainsAny(s.Host, "\r\n") || s.Port < 1 || s.Port > 65535 {
		return ErrConfiguration
	}
	if s.Port == 465 {
		return fmt.Errorf("%w: port 465 requires implicit TLS, which is unsupported; use STARTTLS on 587", ErrConfiguration)
	}
	_, err := address(s.Sender)
	return err
}

func MessageID(sender, kind, id string) (string, error) {
	from, err := address(sender)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(kind+id, "\r\n<>@ \t") {
		return "", ErrConfiguration
	}
	_, domain, _ := strings.Cut(from.Address, "@")
	return "<dad-" + kind + "-" + id + "@" + domain + ">", nil
}

func (m Message) wire() ([]byte, string, string, error) {
	from, err := address(m.From)
	if err != nil {
		return nil, "", "", err
	}
	to, err := address(m.To)
	if err != nil {
		return nil, "", "", err
	}
	if strings.ContainsAny(m.Subject+m.ID, "\r\n") || !strings.HasPrefix(m.ID, "<") || !strings.HasSuffix(m.ID, ">") {
		return nil, "", "", ErrConfiguration
	}
	var b bytes.Buffer
	fmt.Fprintf(
		&b,
		"Date: %s\r\nFrom: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		time.Now().Format(time.RFC1123Z),
		from.String(),
		to.String(),
		mime.QEncoding.Encode("utf-8", m.Subject),
		m.ID,
	)
	writer := quotedprintable.NewWriter(&b)
	if _, err = writer.Write([]byte(m.Body)); err != nil {
		return nil, "", "", err
	}
	if err = writer.Close(); err != nil {
		return nil, "", "", err
	}
	return b.Bytes(), from.Address, to.Address, nil
}

type SMTPTransport struct {
	Settings Settings
	// RootCAs overrides the system trust store; nil verifies against system roots.
	RootCAs *x509.CertPool
}

// Send delivers message once over STARTTLS. Any failure after validation is
// ErrDelivery: SMTP replies can echo addresses and must not reach logs.
func (s SMTPTransport) Send(ctx context.Context, message Message) error {
	if err := s.Settings.validateServer(); err != nil {
		return err
	}
	wire, from, to, err := message.wire()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = s.deliver(ctx, wire, from, to); err != nil {
		return ErrDelivery
	}
	return nil
}

func (s SMTPTransport) deliver(ctx context.Context, wire []byte, from, to string) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(s.Settings.Host, strconv.Itoa(s.Settings.Port)))
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err = connection.SetDeadline(deadline); err != nil {
			return err
		}
	}
	client, err := smtp.NewClient(connection, s.Settings.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	// STARTTLS is mandatory; never fall back to cleartext credentials or content.
	if err = client.StartTLS(&tls.Config{ServerName: s.Settings.Host, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if s.Settings.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", s.Settings.Username, s.Settings.Password, s.Settings.Host)); err != nil {
			return err
		}
	}
	if err = client.Mail(from); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = data.Write(wire); err != nil {
		return err
	}
	if err = data.Close(); err != nil {
		return err
	}
	// The server has accepted the message; a failed QUIT must not report a
	// delivery failure, which would make the worker send it again.
	_ = client.Quit()
	return nil
}
