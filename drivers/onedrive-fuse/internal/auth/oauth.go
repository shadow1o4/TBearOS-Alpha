// Package auth implements the Microsoft identity platform OAuth2 device-code
// flow for a public client and secure token persistence.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const Scopes = "offline_access Files.ReadWrite.All User.Read"

type OAuth struct {
	AuthURL  string // e.g. https://login.microsoftonline.com
	Tenant   string
	ClientID string
	HTTP     *http.Client
}

func (o *OAuth) endpoint(p string) string {
	return strings.TrimRight(o.AuthURL, "/") + "/" + url.PathEscape(o.Tenant) + "/oauth2/v2.0/" + p
}

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// OAuthError is an error response from the token endpoint.
type OAuthError struct{ Code, Description string }

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

func (o *OAuth) post(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint(endpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (o *OAuth) StartDeviceCode(ctx context.Context) (*DeviceCode, error) {
	if o.ClientID == "" {
		return nil, errors.New("no client ID configured: pass --client-id or set client_id in the config file")
	}
	var dc struct {
		DeviceCode
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	err := o.post(ctx, "devicecode", url.Values{"client_id": {o.ClientID}, "scope": {Scopes}}, &dc)
	if err != nil {
		return nil, err
	}
	if dc.Error != "" {
		return nil, &OAuthError{dc.Error, dc.ErrorDescription}
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc.DeviceCode, nil
}

// PollDeviceCode waits until the user completes sign-in in the browser.
func (o *OAuth) PollDeviceCode(ctx context.Context, dc *DeviceCode) (*Token, error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for {
		if dc.ExpiresIn > 0 && time.Now().After(deadline) {
			return nil, errors.New("device code expired; run login again")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		t, err := o.token(ctx, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {o.ClientID},
			"device_code": {dc.DeviceCode},
		})
		var oe *OAuthError
		if errors.As(err, &oe) {
			switch oe.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			}
		}
		return t, err
	}
}

func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	return o.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {o.ClientID},
		"refresh_token": {refreshToken},
		"scope":         {Scopes},
	})
}

func (o *OAuth) token(ctx context.Context, form url.Values) (*Token, error) {
	var tr tokenResponse
	if err := o.post(ctx, "token", form, &tr); err != nil {
		return nil, err
	}
	if tr.Error != "" {
		return nil, &OAuthError{tr.Error, tr.ErrorDescription}
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// TokenSource hands out valid access tokens, refreshing and persisting them
// as needed. It is safe for concurrent use.
type TokenSource struct {
	OAuth *OAuth
	Store Store

	mu  sync.Mutex
	tok *Token
}

func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.tok == nil {
		t, err := ts.Store.Load()
		if err != nil {
			return "", err
		}
		ts.tok = t
	}
	if ts.tok.Valid(2 * time.Minute) {
		return ts.tok.AccessToken, nil
	}
	if ts.tok.RefreshToken == "" {
		return "", ErrNoToken
	}
	nt, err := ts.OAuth.Refresh(ctx, ts.tok.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = ts.tok.RefreshToken
	}
	if err := ts.Store.Save(nt); err != nil {
		return "", fmt.Errorf("save refreshed token: %w", err)
	}
	ts.tok = nt
	return nt.AccessToken, nil
}
