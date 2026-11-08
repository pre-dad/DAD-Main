package mail

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMessageMIMEAndInjectionBoundaries(t *testing.T) {
	message := Message{
		From:    "DAD <sender@example.org>",
		To:      "donor@example.org",
		Subject: "Your gift — thank you",
		Body:    "Line one\nA UTF-8 amount: €30\n.",
		ID:      "<dad-login-fixture@example.org>",
	}
	wire, from, to, err := message.wire()
	if err != nil || from != "sender@example.org" || to != "donor@example.org" {
		t.Fatal("bad envelopes", err)
	}
	parsed, err := netmail.ReadMessage(strings.NewReader(string(wire)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != message.Body {
		t.Fatal("body changed", string(body), err)
	}
	if parsed.Header.Get("Message-ID") != message.ID {
		t.Fatal("message identity changed")
	}
	date, err := time.Parse(time.RFC1123Z, parsed.Header.Get("Date"))
	if err != nil || time.Since(date) > time.Minute {
		t.Fatal("missing or stale Date header", parsed.Header.Get("Date"), err)
	}
	for name, recipients := range map[string]string{
		"two addresses":  "donor@example.org, other@example.org",
		"address groups": "list: a@example.org, b@example.org;",
	} {
		bad := message
		bad.To = recipients
		if _, _, _, err := bad.wire(); err == nil {
			t.Fatalf("%s accepted as one recipient", name)
		}
	}
	for _, edit := range []func(*Message){func(m *Message) { m.To += "\r\nBcc: bad@example.org" }, func(m *Message) { m.From += "\nX: bad" }, func(m *Message) { m.Subject += "\r\nBcc: bad@example.org" }, func(m *Message) { m.ID += "\nX: bad" }} {
		bad := message
		edit(&bad)
		if _, _, _, err := bad.wire(); err == nil {
			t.Fatal("header injection allowed")
		}
	}
	id, err := MessageID(message.From, "login", "fixture")
	if err != nil || id != "<dad-login-fixture@example.org>" {
		t.Fatal(id, err)
	}
}

func TestSMTPRefusesCleartextAndHonorsCancellation(t *testing.T) {
	for _, mode := range []string{"no TLS", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			host, portText, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			connected := make(chan struct{})
			commands := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					commands <- "accept failed"
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				close(connected)
				if mode == "cancel" {
					_, _ = io.Copy(io.Discard, conn)
					commands <- ""
					return
				}
				_, _ = io.WriteString(conn, "220 fixture SMTP\r\n")
				r := bufio.NewReader(conn)
				command, _ := r.ReadString('\n')
				if !strings.HasPrefix(command, "EHLO ") {
					commands <- command
					return
				}
				_, _ = io.WriteString(conn, "250 fixture\r\n")
				command, _ = r.ReadString('\n')
				if strings.HasPrefix(command, "STARTTLS") {
					_, _ = io.WriteString(conn, "502 unavailable\r\n")
					command, _ = r.ReadString('\n')
				}
				commands <- command
			}()
			transport := SMTPTransport{
				Settings: Settings{
					Host:     host,
					Port:     port,
					Username: "private-user",
					Password: "private-password",
					Sender:   "sender@example.org",
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- transport.Send(ctx, Message{From: "sender@example.org", To: "donor@example.org", Subject: "private subject", Body: "private body", ID: "<fixture@example.org>"})
			}()
			<-connected
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, ErrDelivery) {
					t.Fatal("expected safe delivery failure", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("send ignored cancellation")
			}
			if command := <-commands; command != "" {
				t.Fatal("cleartext command escaped TLS boundary", command)
			}
		})
	}
}

func fixtureTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fixture SMTP"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

type fixtureSession struct {
	commands []string
	data     []string
	err      error
}

func serveSTARTTLS(listener net.Listener, config *tls.Config, rcptReply string) fixtureSession {
	var session fixtureSession
	conn, err := listener.Accept()
	if err != nil {
		session.err = err
		return session
	}
	defer func() { conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	reply := func(lines string) { _, _ = io.WriteString(conn, lines) }
	reply("220 fixture SMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			session.err = err
			return session
		}
		command := strings.TrimSuffix(line, "\r\n")
		session.commands = append(session.commands, command)
		verb, _, _ := strings.Cut(command, " ")
		switch {
		case verb == "EHLO" && len(session.commands) == 1:
			reply("250-fixture\r\n250 STARTTLS\r\n")
		case verb == "EHLO":
			reply("250-fixture\r\n250 AUTH PLAIN\r\n")
		case verb == "STARTTLS":
			reply("220 ready\r\n")
			secure := tls.Server(conn, config)
			if err := secure.Handshake(); err != nil {
				session.err = err
				return session
			}
			conn, r = secure, bufio.NewReader(secure)
		case verb == "AUTH":
			reply("235 accepted\r\n")
		case verb == "MAIL":
			reply("250 ok\r\n")
		case verb == "RCPT":
			reply(rcptReply)
		case verb == "DATA":
			reply("354 send\r\n")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					session.err = err
					return session
				}
				if line == ".\r\n" {
					break
				}
				session.data = append(session.data, strings.TrimSuffix(line, "\r\n"))
			}
			reply("250 queued\r\n")
		case verb == "QUIT":
			reply("221 bye\r\n")
			return session
		default:
			reply("502 unexpected\r\n")
		}
	}
}

func TestSMTPDeliversOverVerifiedSTARTTLS(t *testing.T) {
	config, roots := fixtureTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	sessions := make(chan fixtureSession, 1)
	go func() { sessions <- serveSTARTTLS(listener, config, "250 ok\r\n") }()
	transport := SMTPTransport{
		Settings: Settings{
			Host:     host,
			Port:     port,
			Username: "private-user",
			Password: "private-password",
			Sender:   "DAD <sender@example.org>",
		},
		RootCAs: roots,
	}
	message := Message{
		From:    "DAD <sender@example.org>",
		To:      "donor@example.org",
		Subject: "Receipt",
		Body:    "Thank you\n.hidden line\n.",
		ID:      "<dad-message-fixture@example.org>",
	}
	if err = transport.Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	session := <-sessions
	if session.err != nil {
		t.Fatal(session.err)
	}
	plain := base64.StdEncoding.EncodeToString([]byte("\x00private-user\x00private-password"))
	want := []string{
		"EHLO localhost",
		"STARTTLS",
		"EHLO localhost",
		"AUTH PLAIN " + plain,
		"MAIL FROM:<sender@example.org>",
		"RCPT TO:<donor@example.org>",
		"DATA",
		"QUIT",
	}
	if !slices.Equal(session.commands, want) {
		t.Fatalf("commands = %q", session.commands)
	}
	if !slices.Contains(session.data, "..hidden line") || !slices.Contains(session.data, "..") {
		t.Fatalf("leading dots not stuffed: %q", session.data)
	}
	unstuffed := make([]string, len(session.data))
	for i, line := range session.data {
		unstuffed[i] = strings.TrimPrefix(line, ".")
	}
	parsed, err := netmail.ReadMessage(strings.NewReader(strings.Join(unstuffed, "\r\n") + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	for header, value := range map[string]string{"From": `"DAD" <sender@example.org>`, "To": "<donor@example.org>", "Subject": "Receipt", "Message-ID": message.ID, "Content-Transfer-Encoding": "quoted-printable"} {
		if got := parsed.Header.Get(header); got != value {
			t.Fatalf("%s = %q", header, got)
		}
	}
	if _, err = time.Parse(time.RFC1123Z, parsed.Header.Get("Date")); err != nil {
		t.Fatal("Date header", err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	// The DATA terminator requires the final line to end in CRLF.
	if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != message.Body+"\n" {
		t.Fatalf("body = %q, %v", body, err)
	}
}

func startSTARTTLS(t *testing.T, rcptReply string) (SMTPTransport, *x509.CertPool, chan fixtureSession) {
	t.Helper()
	config, roots := fixtureTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	sessions := make(chan fixtureSession, 1)
	go func() { sessions <- serveSTARTTLS(listener, config, rcptReply) }()
	return SMTPTransport{
		Settings: Settings{Host: host, Port: port, Username: "private-user", Password: "private-password", Sender: "sender@example.org"},
	}, roots, sessions
}

var deliverable = Message{
	From: "sender@example.org", To: "donor@example.org", Subject: "private subject", Body: "private body", ID: "<fixture@example.org>",
}

// A server whose certificate is not signed by a trusted root is never sent
// credentials or content.
func TestSMTPRefusesUntrustedCertificates(t *testing.T) {
	transport, _, sessions := startSTARTTLS(t, "250 ok\r\n")
	if err := transport.Send(context.Background(), deliverable); !errors.Is(err, ErrDelivery) {
		t.Fatalf("untrusted certificate: %v; want ErrDelivery", err)
	}
	session := <-sessions
	for _, command := range session.commands {
		if verb, _, _ := strings.Cut(command, " "); slices.Contains([]string{"AUTH", "MAIL", "RCPT", "DATA"}, verb) {
			t.Fatalf("%q was sent over an unverified connection", command)
		}
	}
}

// A refused recipient fails the delivery without leaking the server's reply,
// the address or the credentials, and sends no message body.
func TestSMTPRecipientRefusalIsASafeDeliveryFailure(t *testing.T) {
	transport, roots, sessions := startSTARTTLS(t, "550 5.1.1 donor@example.org does not exist\r\n")
	transport.RootCAs = roots
	err := transport.Send(context.Background(), deliverable)
	if !errors.Is(err, ErrDelivery) {
		t.Fatalf("refused recipient: %v; want ErrDelivery", err)
	}
	for _, secret := range []string{"donor@example.org", "does not exist", "private-password", "private-user"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("delivery error %q leaks %q", err, secret)
		}
	}
	if session := <-sessions; slices.Contains(session.commands, "DATA") || len(session.data) != 0 {
		t.Fatalf("a message was sent to a refused recipient: %q", session.commands)
	}
}
