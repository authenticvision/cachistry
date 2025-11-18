package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/authenticvision/cachistry/cache"
	"github.com/authenticvision/cachistry/httputil"
	"github.com/authenticvision/cachistry/wwwauth"
	"github.com/authenticvision/util-go/fmtutil"
	"github.com/authenticvision/util-go/httpp"
	"github.com/authenticvision/util-go/logutil"
	"github.com/authenticvision/util-go/mainutil"
	"github.com/mologie/ttlmap-go"
	"github.com/spf13/cobra"
)

var packageScope = logutil.NewScope("coordinator")

type Config struct {
	mainutil.LogConfig
	mainutil.ServerConfig

	Registries             []string `flag:"required" env:"-" usage:"docker.io, ghcr.io, etc"`
	CacheDir               string   `flag:"required"`
	CacheSize              fmtutil.Bytes
	UnconditionalCacheTime time.Duration
	UpstreamTimeout        time.Duration
}

type App struct {
	client     *http.Client
	cache      *cache.Cache
	regs       map[string]string
	tokenCache *ttlmap.TTLMap[wwwauth.WWWAuthenticate, Token]
}

func main() {
	app := App{
		client:     http.DefaultClient,
		regs:       make(map[string]string),
		tokenCache: ttlmap.New[wwwauth.WWWAuthenticate, Token](5 * time.Minute),
	}
	cmd := mainutil.RootCommand(app.setup, mainutil.Server(app.run), cobra.Command{
		Use: "cachistry",
	}, Config{
		LogConfig: mainutil.LogDefault,
		ServerConfig: mainutil.ServerConfig{
			BindAddr: "127.0.0.1:5000",
		},
		CacheSize:              1 << 30,
		UnconditionalCacheTime: 5 * time.Minute,
		UpstreamTimeout:        10 * time.Second,
	})
	mainutil.Run(cmd)
}

func (app *App) setup(cfg *Config, cmd *cobra.Command, args []string) (err error) {
	app.cache, err = cache.NewCache(cfg.CacheDir, uint64(cfg.CacheSize))
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
			return scope.Err(err, "check cache")
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

		upstreamCtx, upstreamCancel := context.WithTimeout(r.Context(), cfg.UpstreamTimeout)
		defer upstreamCancel()
		upstreamURL := (&url.URL{
			Scheme: "https",
			Host:   reg,
			Path:   "/v2/",
		}).JoinPath(path)
		token, err := app.preflight(upstreamCtx, upstreamURL)
		if revalidate && err != nil {
			log.Warn("preflight failed, serving from cache")
			return serveFromCache()
		}
		if err != nil {
			return scope.Err(err, "preflight")
		}

		req, err := newRequest(upstreamCtx, http.MethodGet, upstreamURL)
		if err != nil {
			return scope.Err(err, "new request")
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
			return scope.Err(err, "do request")
		}

		if resp.StatusCode == http.StatusNotModified {
			log.Debug("successfully revalidated cache")
			err := app.cache.UpdateValidated(cachePath)
			if err != nil {
				return scope.Err(err, "update cache expiry")
			}
			return serveFromCache()
		}

		if revalidate {
			log.Debug("failed to revalidate cache, proxying request")
		} else {
			log.Debug("proxying request")
		}

		eTag := resp.Header.Get("ETag")
		contentType := resp.Header.Get("Content-Type")
		contentLengthStr := resp.Header.Get("Content-Length")
		contentLength, err := strconv.ParseUint(contentLengthStr, 10, 64)
		if contentLengthStr == "" || err != nil {
			return scope.Err(err, "proxied response has no content-length, this is unsupported")
		}
		w.Header().Set("ETag", eTag)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", contentLengthStr)

		// Note: ETag from the client isn't taken into account because neither
		// docker nor podman use it at all. We can still use it to check
		// upstreams though.

		f, cleanup, err := app.cache.Create(contentType, eTag)
		if err != nil {
			return scope.Err(err, "create cache file")
		}
		defer cleanup()

		body := io.TeeReader(resp.Body, f)

		httpp.DisableCompression(w)

		_, err = io.Copy(w, body)
		if err != nil {
			return scope.Err(err, "copy")
		}

		err = app.cache.Store(f, cachePath, contentLength)
		if err != nil {
			return scope.Err(err, "store cache file")
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

func newRequest(ctx context.Context, method string, u *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cachistry/0.1 (+https://github.com/authenticvision/cachistry)")
	return req, nil
}
