package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/authenticvision/docker-registry-caching-proxy/httputil"
	"github.com/authenticvision/docker-registry-caching-proxy/wwwauth"
	"github.com/authenticvision/util-go/logutil"
)

func (app *App) fetchToken(ctx context.Context, wwwAuth wwwauth.WWWAuthenticate) (Token, error) {
	log := logutil.FromContext(ctx).With(slog.Any("www_authenticate", wwwAuth))
	if token, ok := app.tokenCache.Load(wwwAuth); ok {
		log.Debug("loaded token from cache")
		return token, nil
	}
	u, err := url.Parse(wwwAuth.Realm)
	if err != nil {
		return Token{}, logutil.NewError(err, "parse realm")
	}
	q := u.Query()
	q.Set("scope", wwwAuth.Scope)
	q.Set("service", wwwAuth.Service)
	u.RawQuery = q.Encode()

	tokenReq, err := newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return Token{}, logutil.NewError(err, "new request")
	}
	resp, err := app.client.Do(tokenReq)
	if err != nil {
		return Token{}, logutil.NewError(err, "do request")
	}

	if resp.StatusCode != http.StatusOK {
		err := httputil.ResponseAsError(resp)
		return Token{}, logutil.NewError(err, "status not ok")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("Content-Type") != "application/json" {
		return Token{}, logutil.NewError(nil, "unexpected content type")
	}

	var token Token
	err = json.NewDecoder(resp.Body).Decode(&token)
	if err != nil {
		return Token{}, logutil.NewError(err, "unmarshal token")
	}

	slog.Debug("fetched token")
	app.tokenCache.Store(wwwAuth, token)
	return token, nil
}

type Token struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
	IssuedAt  string `json:"issued_at"`
}
