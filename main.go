package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/authenticvision/docker-registry-caching-proxy/cache"
	"github.com/authenticvision/docker-registry-caching-proxy/httputil"
	"github.com/authenticvision/docker-registry-caching-proxy/wwwauth"
	"github.com/authenticvision/util-go/httpp"
	"github.com/authenticvision/util-go/logutil"
	"github.com/authenticvision/util-go/mainutil"
	"github.com/spf13/cobra"
)

var packageScope = logutil.NewScope("coordinator")

type Config struct {
	mainutil.LogConfig
	mainutil.ServerConfig

	Registries []string `flag:"required" env:"-" usage:"docker.io, ghcr.io, etc"`
	CacheDir   string   `flag:"required"`
}

type App struct {
	client *http.Client
}

func main() {
	app := App{
		client: http.DefaultClient,
	}
	cmd := mainutil.RootCommand(nil, mainutil.Server(app.run), cobra.Command{
		Use: "docker-registry-caching-proxy",
	}, Config{
		LogConfig: mainutil.LogDefault,
		ServerConfig: mainutil.ServerConfig{
			BindAddr: "127.0.0.1:5000",
		},
	})
	mainutil.Run(cmd)
}

func (app *App) run(cfg *Config, cmd *cobra.Command, args []string) (httpp.Handler, error) {
	regs := map[string]string{}
	for _, reg := range cfg.Registries {
		if reg == "docker.io" {
			regs[reg] = "registry-1.docker.io"
		} else {
			regs[reg] = reg
		}
	}
	cache, err := cache.NewCache(cfg.CacheDir, 1<<30)
	if err != nil {
		return nil, fmt.Errorf("create cache: %w", err)
	}
	mux := httpp.NewServeMux()
	mux.HandleFunc("GET /v2/{$}", func(w http.ResponseWriter, r *http.Request) error {
		return nil
	})
	mux.HandleFunc("GET /v2/{registry}/{path...}", func(w http.ResponseWriter, r *http.Request) error {
		registry := r.PathValue("registry")
		path := r.PathValue("path")
		cachePath := filepath.Join(registry, path)

		scope := logutil.NewScope("proxy", slog.String("cache_path", cachePath))
		log := scope.Log(logutil.FromContext(r.Context()))

		if mimeType, err := cache.GetMIMEType(cachePath); err != nil {
			return logutil.NewError(err, "check cache")
		} else if mimeType != "" {
			log.Debug("serving from cache")
			w.Header().Set("Content-Type", mimeType)
			http.ServeFileFS(w, r, cache.FS(), cachePath)
			return nil
		}

		reg, ok := regs[registry]
		if !ok {
			return httpp.NotFound("registry not found")
		}

		upstreamURL := (&url.URL{
			Scheme: "https",
			Host:   reg,
			Path:   "/v2/",
		}).JoinPath(path)
		token, err := app.preflight(r.Context(), upstreamURL)
		if err != nil {
			return logutil.NewError(err, "preflight")
		}

		req, err := newRequest(r.Context(), http.MethodGet, upstreamURL)
		if err != nil {
			return logutil.NewError(err, "new request")
		}

		req.Header["Accept"] = r.Header.Values("Accept")
		//req.Header.Set("Accept-Encoding", "gzip")

		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := app.client.Do(req)
		if err != nil {
			return logutil.NewError(err, "do request")
		}

		if resp.StatusCode != http.StatusOK {
			err := httputil.ResponseAsError(resp)
			return logutil.NewError(err, "status not ok")
		}

		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		if v := resp.Header.Get("Content-Length"); v != "" {
			w.Header().Set("Content-Length", v)
		}

		f, err := cache.Store(cachePath, resp.Header.Get("Content-Type"))
		if err != nil {
			return logutil.NewError(err, "create cache file")
		}
		defer func() { _ = f.Close() }()
		body := io.TeeReader(resp.Body, f)
		// TODO store ETag, Expires headers? nope, docker doesn't do any caching besides just not pulling blobs it already has, thus bypassing http caching altogether

		httpp.DisableCompression(w)

		_, err = io.Copy(w, body)
		if err != nil {
			return logutil.NewError(err, "copy")
		}
		return nil
	})
	return mux, nil
}

func (app *App) preflight(ctx context.Context, upstreamURL *url.URL) (string, error) {
	preflightReq, err := newRequest(ctx, http.MethodHead, upstreamURL)
	if err != nil {
		return "", logutil.NewError(err, "new request")
	}
	resp, err := app.client.Do(preflightReq)
	if err != nil {
		return "", logutil.NewError(err, "do request")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		parsed, err := wwwauth.Parse(resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return "", logutil.NewError(
				err, "parse www-authenticate",
				slog.String("www_authenticate", resp.Header.Get("WWW-Authenticate")),
			)
		}

		tokenResp, err := app.fetchToken(ctx, parsed)
		if err != nil {
			return "", logutil.NewError(err, "fetch token")
		}
		return tokenResp.Token, nil
	} else if resp.StatusCode != http.StatusOK {
		err := httputil.ResponseAsError(resp)
		return "", logutil.NewError(err, "status not ok")
	}
	return "", nil
}

func (app *App) fetchToken(ctx context.Context, wwwAuth wwwauth.WWWAuthenticate) (Token, error) {
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
	return token, nil
}

type Token struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
	IssuedAt  string `json:"issued_at"`
}

func newRequest(ctx context.Context, method string, u *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "docker-registry-caching-proxy/0.1 (+https://github.com/authenticvision/docker-registry-caching-proxy)")
	return req, nil
}
