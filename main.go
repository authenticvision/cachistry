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
	"time"

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

	Registries             []string `flag:"required" env:"-" usage:"docker.io, ghcr.io, etc"`
	CacheDir               string   `flag:"required"`
	UnconditionalCacheTime time.Duration
}

type App struct {
	client *http.Client
	cache  *cache.Cache
	regs   map[string]string
}

func main() {
	app := App{
		client: http.DefaultClient,
		regs:   map[string]string{},
	}
	cmd := mainutil.RootCommand(app.setup, mainutil.Server(app.run), cobra.Command{
		Use: "docker-registry-caching-proxy",
	}, Config{
		LogConfig: mainutil.LogDefault,
		ServerConfig: mainutil.ServerConfig{
			BindAddr: "127.0.0.1:5000",
		},
		UnconditionalCacheTime: 5 * time.Minute,
	})
	mainutil.Run(cmd)
}

func (app *App) setup(cfg *Config, cmd *cobra.Command, args []string) (err error) {
	app.cache, err = cache.NewCache(cfg.CacheDir, 1<<30)
	if err != nil {
		return fmt.Errorf("create cache: %w", err)
	}

	for _, reg := range cfg.Registries {
		if reg == "docker.io" {
			app.regs[reg] = "registry-1.docker.io"
		} else {
			app.regs[reg] = reg
		}
	}
	return nil
}

func (app *App) run(cfg *Config, cmd *cobra.Command, args []string) (httpp.Handler, error) {
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

		cached, err := app.cache.Get(cachePath)
		if err != nil {
			return logutil.NewError(err, "check cache")
		}
		serveFromCache := func() error {
			log.Debug("serving from cache")
			w.Header().Set("Content-Type", cached.MIMEType)
			w.Header().Set("ETag", cached.ETag)
			http.ServeFileFS(w, r, app.cache.FS(), cachePath)
			return nil
		}
		revalidate := false
		if cached != nil {
			revalidate = cached.Validated.Add(cfg.UnconditionalCacheTime).Before(time.Now())
			if !revalidate {
				return serveFromCache()
			}
		}

		reg, ok := app.regs[registry]
		if !ok {
			return httpp.NotFound("registry not found")
		}

		upstreamURL := (&url.URL{
			Scheme: "https",
			Host:   reg,
			Path:   "/v2/",
		}).JoinPath(path)
		token, err := app.preflight(r.Context(), upstreamURL)
		if revalidate && err != nil {
			log.Warn("preflight failed, serving from cache")
			return serveFromCache()
		}
		if err != nil {
			return logutil.NewError(err, "preflight")
		}

		req, err := newRequest(r.Context(), http.MethodGet, upstreamURL)
		if err != nil {
			return logutil.NewError(err, "new request")
		}
		if revalidate {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		req.Header["Accept"] = r.Header.Values("Accept")
		//req.Header.Set("Accept-Encoding", "gzip")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := app.client.Do(req)
		if err == nil &&
			!(resp.StatusCode == http.StatusOK ||
				resp.StatusCode == http.StatusNotModified) {
			err = httputil.ResponseAsError(resp)
		}
		if revalidate && err != nil {
			log.Warn("proxying request failed, serving from cache")
			return serveFromCache()
		}
		if err != nil {
			return logutil.NewError(err, "do request")
		}

		if resp.StatusCode == http.StatusNotModified {
			log.Debug("successfully revalidated cache")
			err := app.cache.UpdateValidated(cachePath)
			if err != nil {
				return logutil.NewError(err, "update cache expiry")
			}
			return serveFromCache()
		}

		if resp.StatusCode != http.StatusOK {
			err := httputil.ResponseAsError(resp)
			return logutil.NewError(err, "status not ok")
		}

		if revalidate {
			log.Debug("failed to revalidate cache, proxying request")
		} else {
			log.Debug("proxying request")
		}

		eTag := resp.Header.Get("ETag")
		contentType := resp.Header.Get("Content-Type")
		w.Header().Set("ETag", eTag)
		w.Header().Set("Content-Type", contentType)
		if v := resp.Header.Get("Content-Length"); v != "" {
			w.Header().Set("Content-Length", v)
		}

		// Note: ETag from the client isn't taken into account because neither
		// docker nor podman use it at all. We can still use it to check
		// upstreams though.

		f, cleanup, err := app.cache.Create(contentType, eTag)
		if err != nil {
			return logutil.NewError(err, "create cache file")
		}
		defer cleanup()

		body := io.TeeReader(resp.Body, f)

		httpp.DisableCompression(w)

		_, err = io.Copy(w, body)
		if err != nil {
			return logutil.NewError(err, "copy")
		}

		err = app.cache.Store(f, cachePath)
		if err != nil {
			return logutil.NewError(err, "store cache file")
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
