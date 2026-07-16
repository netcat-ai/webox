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
	"github.com/netcat-ai/webox/internal/qrsource"
	"github.com/netcat-ai/webox/internal/sender"
	"github.com/netcat-ai/webox/internal/wechat"
)

type postLoginUIState struct {
	dismissed bool
}

func (state *postLoginUIState) shouldDismiss(initialization wechat.InitializationState) bool {
	if initialization == wechat.WaitingForLogin {
		state.dismissed = false
		return false
	}
	return !state.dismissed
}

func (state *postLoginUIState) markDismissed() { state.dismissed = true }

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
	wechatState := wechat.New(configuration.StateDir, configuration.CursorKey, configuration.RemarkFilterEnabled)
	if err := wechatState.EnsureStateDir(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	qr := qrsource.New(configuration.QRScreenshotPath)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		runInitializer(ctx, wechatState, qr, logger)
	}()

	protocol := ilink.New(
		configuration.APIToken,
		configuration.ProviderAccountID,
		configuration.PublicBaseURL,
		wechatState,
		sender.New(wechatState),
		qr,
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

func runInitializer(ctx context.Context, state *wechat.State, source qrsource.Source, logger *slog.Logger) {
	if !wait(ctx, 3*time.Second) {
		return
	}
	readyLogged, noQRChecks := false, 0
	var postLogin postLoginUIState
	for {
		initialization, err := state.InitializeIfReady()
		if err != nil {
			readyLogged = false
			state.RecordInitError(err)
			logger.Warn("wechat automatic initialization is not ready", "error", err)
			if !wait(ctx, 2*time.Second) {
				return
			}
		} else if initialization == wechat.Ready {
			if postLogin.shouldDismiss(wechat.Ready) {
				if !wait(ctx, 300*time.Millisecond) {
					return
				}
				if dismissed, err := state.DismissPostLoginOverlay(); err != nil {
					logger.Warn("could not dismiss post-login WeChat overlay", "error", err)
				} else if dismissed {
					postLogin.markDismissed()
					logger.Info("dismissed post-login WeChat overlay")
				}
			}
			if !readyLogged {
				logger.Info("wechat automatic initialization is ready")
				readyLogged = true
			}
			noQRChecks = 0
		} else {
			readyLogged = false
			postLogin.shouldDismiss(wechat.WaitingForLogin)
			code, err := source.Latest()
			if err != nil {
				logger.Warn("could not inspect WeChat login QR code", "error", err)
			}
			if code != nil {
				noQRChecks = 0
			} else {
				noQRChecks++
				if noQRChecks >= 3 {
					clicked, err := state.ClickSavedAccountLogin()
					if err != nil {
						logger.Warn("could not activate saved-account login", "error", err)
					} else if clicked {
						logger.Info("activated saved-account WeChat login")
					}
					noQRChecks = 0
				}
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
