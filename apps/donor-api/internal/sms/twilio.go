package sms

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	twilioAccount = regexp.MustCompile(`^AC[0-9a-f]{32}$`)
	// twilioSender is an E.164 number or a Messaging Service SID.
	twilioSender = regexp.MustCompile(`^(\+[1-9][0-9]{1,14}|MG[0-9a-f]{32})$`)
)

const twilioAPI = "https://api.twilio.com"

// TwilioSender sends through Twilio's Messages API.
type TwilioSender struct {
	account string
	token   string
	from    string
	base    string
	client  *http.Client
}

func newTwilioSender(s *Settings) *TwilioSender {
	return &TwilioSender{
		account: s.AccountID, token: s.AuthToken, from: s.Sender,
		base: twilioAPI, client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Send posts one message. Any refusal is ErrUnavailable; neither the message,
// which holds a sign-in code, nor Twilio's reply is included in the error.
func (s *TwilioSender) Send(ctx context.Context, phone, message string) error {
	if phone == "" || message == "" {
		return ErrUnavailable
	}
	form := url.Values{"To": {phone}, "Body": {message}}
	if strings.HasPrefix(s.from, "MG") {
		form.Set("MessagingServiceSid", s.from)
	} else {
		form.Set("From", s.from)
	}
	endpoint := s.base + "/2010-04-01/Accounts/" + s.account + "/Messages.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(s.account, s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: Twilio answered %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}
