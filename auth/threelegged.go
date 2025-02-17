// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/auth/internal"
)

// AuthorizationHandler is a 3-legged-OAuth helper that prompts the user for
// OAuth consent at the specified auth code URL and returns an auth code and
// state upon approval.
type AuthorizationHandler func(authCodeURL string) (code string, state string, err error)

// Options3LO are the options for doing a 3-legged OAuth2 flow.
type Options3LO struct {
	// ClientID is the application's ID.
	ClientID string
	// ClientSecret is the application's secret.
	ClientSecret string
	// AuthURL is the URL for authenticating.
	AuthURL string
	// TokenURL is the URL for retrieving a token.
	TokenURL string
	// RedirectURL is the URL to redirect users to.
	RedirectURL string
	// Scopes specifies requested permissions for the Token. Optional.
	Scopes []string

	// URLParams are the set of values to apply to the token exchange. Optional.
	URLParams url.Values
	// Client is the client to be used to make the underlying token requests.
	// Optional.
	Client *http.Client
	// AuthStyle is used to describe how to client info in the token request.
	AuthStyle Style
	// EarlyTokenExpiry is the time before the token expires that it should be
	// refreshed. If not set the default value is 10 seconds. Optional.
	EarlyTokenExpiry time.Duration

	// AuthHandlerOpts provides a set of options for doing a
	// 3-legged OAuth2 flow with a custom [AuthorizationHandler]. Optional.
	AuthHandlerOpts *AuthorizationHandlerOptions
}

// PKCEConfig holds parameters to support PKCE.
type PKCEConfig struct {
	// Challenge is the un-padded, base64-url-encoded string of the encrypted code verifier.
	Challenge string // The un-padded, base64-url-encoded string of the encrypted code verifier.
	// ChallengeMethod is the encryption method (ex. S256).
	ChallengeMethod string
	// Verifier is the original, non-encrypted secret.
	Verifier string // The original, non-encrypted secret.
}

type tokenJSON struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	// error fields
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorURI         string `json:"error_uri"`
}

func (e *tokenJSON) expiry() (t time.Time) {
	if v := e.ExpiresIn; v != 0 {
		return time.Now().Add(time.Duration(v) * time.Second)
	}
	return
}

func (c *Options3LO) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return internal.CloneDefaultClient()
}

// authCodeURL returns a URL that points to a OAuth2 consent page.
func (c *Options3LO) authCodeURL(state string, values url.Values) string {
	var buf bytes.Buffer
	buf.WriteString(c.AuthURL)
	v := url.Values{
		"response_type": {"code"},
		"client_id":     {c.ClientID},
	}
	if c.RedirectURL != "" {
		v.Set("redirect_uri", c.RedirectURL)
	}
	if len(c.Scopes) > 0 {
		v.Set("scope", strings.Join(c.Scopes, " "))
	}
	if state != "" {
		v.Set("state", state)
	}
	if c.AuthHandlerOpts != nil {
		if c.AuthHandlerOpts.PKCEConfig != nil &&
			c.AuthHandlerOpts.PKCEConfig.Challenge != "" {
			v.Set(codeChallengeKey, c.AuthHandlerOpts.PKCEConfig.Challenge)
		}
		if c.AuthHandlerOpts.PKCEConfig != nil &&
			c.AuthHandlerOpts.PKCEConfig.ChallengeMethod != "" {
			v.Set(codeChallengeMethodKey, c.AuthHandlerOpts.PKCEConfig.ChallengeMethod)
		}
	}
	for k := range values {
		v.Set(k, v.Get(k))
	}
	if strings.Contains(c.AuthURL, "?") {
		buf.WriteByte('&')
	} else {
		buf.WriteByte('?')
	}
	buf.WriteString(v.Encode())
	return buf.String()
}

// New3LOTokenProvider returns a [TokenProvider] based on the 3-legged OAuth2
// configuration. The TokenProvider is caches and auto-refreshes tokens by
// default.
func New3LOTokenProvider(refreshToken string, opts *Options3LO) (TokenProvider, error) {
	if opts.AuthHandlerOpts != nil {
		return new3LOTokenProviderWithAuthHandler(opts), nil
	}
	// TODO(codyoss): validate the things
	return NewCachedTokenProvider(&tokenProvider3LO{opts: opts, refreshToken: refreshToken, client: opts.client()}, &CachedTokenProviderOptions{
		ExpireEarly: opts.EarlyTokenExpiry,
	}), nil
}

// AuthorizationHandlerOptions provides a set of options to specify for doing a
// 3-legged OAuth2 flow with a custom [AuthorizationHandler].
type AuthorizationHandlerOptions struct {
	// AuthorizationHandler specifies the handler used to for the authorization
	// part of the flow.
	Handler AuthorizationHandler
	// State is used verify that the "state" is identical in the request and
	// response before exchanging the auth code for OAuth2 token.
	State string
	// PKCEConfig allows setting configurations for PKCE. Optional.
	PKCEConfig *PKCEConfig
}

func new3LOTokenProviderWithAuthHandler(opts *Options3LO) TokenProvider {
	return NewCachedTokenProvider(&tokenProviderWithHandler{opts: opts, state: opts.AuthHandlerOpts.State}, &CachedTokenProviderOptions{
		ExpireEarly: opts.EarlyTokenExpiry,
	})
}

// exchange handles the final exchange portion of the 3lo flow. Returns a Token,
// refreshToken, and error.
func (c *Options3LO) exchange(ctx context.Context, code string) (*Token, string, error) {
	// Build request
	v := url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
	}
	if c.RedirectURL != "" {
		v.Set("redirect_uri", c.RedirectURL)
	}
	if c.AuthHandlerOpts != nil &&
		c.AuthHandlerOpts.PKCEConfig != nil &&
		c.AuthHandlerOpts.PKCEConfig.Verifier != "" {
		v.Set(codeVerifierKey, c.AuthHandlerOpts.PKCEConfig.Verifier)
	}
	for k := range c.URLParams {
		v.Set(k, c.URLParams.Get(k))
	}
	return fetchToken(ctx, c, v)
}

// This struct is not safe for concurrent access alone, but the way it is used
// in this package by wrapping it with a cachedTokenProvider makes it so.
type tokenProvider3LO struct {
	opts         *Options3LO
	client       *http.Client
	refreshToken string
}

func (tp *tokenProvider3LO) Token(ctx context.Context) (*Token, error) {
	if tp.refreshToken == "" {
		return nil, errors.New("auth: token expired and refresh token is not set")
	}
	v := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tp.refreshToken},
	}
	for k := range tp.opts.URLParams {
		v.Set(k, tp.opts.URLParams.Get(k))
	}

	tk, rt, err := fetchToken(ctx, tp.opts, v)
	if err != nil {
		return nil, err
	}
	if tp.refreshToken != rt && rt != "" {
		tp.refreshToken = rt
	}
	return tk, err
}

type tokenProviderWithHandler struct {
	opts  *Options3LO
	state string
}

func (tp tokenProviderWithHandler) Token(ctx context.Context) (*Token, error) {
	url := tp.opts.authCodeURL(tp.state, nil)
	code, state, err := tp.opts.AuthHandlerOpts.Handler(url)
	if err != nil {
		return nil, err
	}
	if state != tp.state {
		return nil, errors.New("auth: state mismatch in 3-legged-OAuth flow")
	}
	tok, _, err := tp.opts.exchange(ctx, code)
	return tok, err
}

// fetchToken returns a Token, refresh token, and/or an error.
func fetchToken(ctx context.Context, c *Options3LO, v url.Values) (*Token, string, error) {
	var refreshToken string
	if c.AuthStyle == StyleUnknown {
		return nil, refreshToken, fmt.Errorf("auth: missing required field AuthStyle")
	}
	if c.AuthStyle == StyleInParams {
		if c.ClientID != "" {
			v.Set("client_id", c.ClientID)
		}
		if c.ClientSecret != "" {
			v.Set("client_secret", c.ClientSecret)
		}
	}
	req, err := http.NewRequest("POST", c.TokenURL, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, refreshToken, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.AuthStyle == StyleInHeader {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}

	// Make request
	r, err := c.client().Do(req.WithContext(ctx))
	if err != nil {
		return nil, refreshToken, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body.Close()
	if err != nil {
		return nil, refreshToken, fmt.Errorf("auth: cannot fetch token: %w", err)
	}

	failureStatus := r.StatusCode < 200 || r.StatusCode > 299
	tokError := &Error{
		Response: r,
		Body:     body,
	}

	var token *Token
	// errors ignored because of default switch on content
	content, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch content {
	case "application/x-www-form-urlencoded", "text/plain":
		// some endpoints return a query string
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			if failureStatus {
				return nil, refreshToken, tokError
			}
			return nil, refreshToken, fmt.Errorf("auth: cannot parse response: %w", err)
		}
		tokError.code = vals.Get("error")
		tokError.description = vals.Get("error_description")
		tokError.uri = vals.Get("error_uri")
		token = &Token{
			Value:    vals.Get("access_token"),
			Type:     vals.Get("token_type"),
			Metadata: make(map[string]interface{}, len(vals)),
		}
		for k, v := range vals {
			token.Metadata[k] = v
		}
		refreshToken = vals.Get("refresh_token")
		e := vals.Get("expires_in")
		expires, _ := strconv.Atoi(e)
		if expires != 0 {
			token.Expiry = time.Now().Add(time.Duration(expires) * time.Second)
		}
	default:
		var tj tokenJSON
		if err = json.Unmarshal(body, &tj); err != nil {
			if failureStatus {
				return nil, refreshToken, tokError
			}
			return nil, refreshToken, fmt.Errorf("auth: cannot parse json: %w", err)
		}
		tokError.code = tj.ErrorCode
		tokError.description = tj.ErrorDescription
		tokError.uri = tj.ErrorURI
		token = &Token{
			Value:    tj.AccessToken,
			Type:     tj.TokenType,
			Expiry:   tj.expiry(),
			Metadata: make(map[string]interface{}),
		}
		json.Unmarshal(body, &token.Metadata) // optional field, skip err check
		refreshToken = tj.RefreshToken
	}
	// according to spec, servers should respond status 400 in error case
	// https://www.rfc-editor.org/rfc/rfc6749#section-5.2
	// but some unorthodox servers respond 200 in error case
	if failureStatus || tokError.code != "" {
		return nil, refreshToken, tokError
	}
	if token.Value == "" {
		return nil, refreshToken, errors.New("auth: server response missing access_token")
	}
	return token, refreshToken, nil
}
