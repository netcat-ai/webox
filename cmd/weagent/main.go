package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/netcat-ai/webox/internal/config"
	"github.com/netcat-ai/webox/internal/ilink"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wechat"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("weagent stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configuration, err := config.Load()
	if err != nil {
		return err
	}
	wechatState := wechat.New(configuration.StateDir)
	if err := wechatState.EnsureStateDir(); err != nil {
		return err
	}
	media, err := sharedmedia.New(configuration.SharedDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		runInitializer(ctx, wechatState, logger)
	}()
	protocol := ilink.New(
		configuration.APIToken,
		media,
		wechatState,
		sender.New(wechatState, media),
		logger,
	)
	server := &http.Server{Handler: requestLogger(protocol.Handler(), logger), ReadHeaderTimeout: 5 * time.Second}
	listener, err := net.Listen("tcp", configuration.ListenAddr)
	if err != nil {
		stop()
		workers.Wait()
		return err
	}
	logger.Info("weagent listening", "address", configuration.ListenAddr)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			workers.Wait()
			return err
		}
	}
	stop()
	workers.Wait()
	return nil
}

func runInitializer(ctx context.Context, state *wechat.State, logger *slog.Logger) {
	if !wait(ctx, 3*time.Second) {
		return
	}
	readyLogged, loginRequiredLogged := false, false
	for {
		initialization, err := state.InitializeIfReady()
		if err != nil {
			readyLogged = false
			state.MarkUninitialized()
			logger.Warn("wechat automatic initialization is not ready", "error", err)
			if !wait(ctx, 2*time.Second) {
				return
			}
		} else if initialization == wechat.Ready {
			if !readyLogged {
				logger.Info("wechat automatic initialization is ready")
				readyLogged = true
			}
			loginRequiredLogged = false
		} else {
			readyLogged = false
			if !loginRequiredLogged {
				logger.Info("wechat login is required; complete it through noVNC")
				loginRequiredLogged = true
			}
		}
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		logger.Info("http request", "method", request.Method, "path", request.URL.Path, "elapsed_ms", time.Since(started).Milliseconds())
	})
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
