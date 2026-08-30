package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/netcat-ai/webox/internal/config"
	"github.com/netcat-ai/webox/internal/sharedmedia"
	"github.com/netcat-ai/webox/internal/wecomarchive"
	"github.com/netcat-ai/webox/internal/wecomproxy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("wecomproxy stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	configuration, err := config.LoadWeComProxy()
	if err != nil {
		return err
	}
	media, err := sharedmedia.New(configuration.SharedDir)
	if err != nil {
		return err
	}
	privateKey, err := os.ReadFile(configuration.Archive.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("read WeCom archive private key: %w", err)
	}
	client, err := wecomarchive.OpenClient(configuration.Archive.CorpID, configuration.Archive.Secret, string(privateKey))
	if err != nil {
		return fmt.Errorf("open WeCom archive client: %w", err)
	}
	defer client.Free()
	source, err := wecomarchive.New(client, media, wecomarchive.Config{
		AccountID: configuration.Archive.AccountID, WeChatID: configuration.Archive.WeChatID,
		Nickname: configuration.Archive.Nickname, InitialSequence: configuration.Archive.InitialSeq, Logger: logger,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	protocol := wecomproxy.New(configuration.APIToken, source, logger)
	server := &http.Server{Handler: requestLogger(protocol.Handler(), logger), ReadHeaderTimeout: 5 * time.Second}
	listener, err := net.Listen("tcp", configuration.ListenAddr)
	if err != nil {
		return err
	}
	logger.Info("wecomproxy listening", "address", configuration.ListenAddr)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		logger.Info("http request", "method", request.Method, "path", request.URL.Path, "elapsed_ms", time.Since(started).Milliseconds())
	})
}
