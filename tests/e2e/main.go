package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/netcat-ai/webox/tests/e2e/runner"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("webox-e2e", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scenario := flags.String("scenario", "direct", "E2E scenario (currently: direct)")
	sutURL := flags.String("sut-url", envOr("WEBOX_E2E_SUT_URL", "http://127.0.0.1:38080"), "SUT iLink base URL")
	peerURL := flags.String("peer-url", envOr("WEBOX_E2E_PEER_URL", "http://127.0.0.1:38081"), "peer iLink base URL")
	sutContainer := flags.String("sut-container", envOr("WEBOX_E2E_SUT_CONTAINER", "webox-sut"), "SUT Docker container")
	peerContainer := flags.String("peer-container", envOr("WEBOX_E2E_PEER_CONTAINER", "webox-peer"), "peer Docker container")
	peerTarget := flags.String("peer-target", os.Getenv("WEBOX_E2E_PEER_TARGET"), "peer-side unique remark for the SUT account")
	dockerBinary := flags.String("docker", envOr("WEBOX_E2E_DOCKER", "docker"), "Docker CLI binary")
	timeout := flags.Duration("timeout", 3*time.Minute, "overall scenario timeout")
	artifactsRoot := flags.String("artifacts", envOr("WEBOX_E2E_ARTIFACTS", "tests/e2e/artifacts"), "failure artifact directory")
	if err := flags.Parse(arguments); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if *scenario != "direct" {
		fmt.Fprintf(stderr, "unsupported scenario %q\n", *scenario)
		return 2
	}
	if strings.TrimSpace(*peerTarget) == "" {
		fmt.Fprintln(stderr, "--peer-target or WEBOX_E2E_PEER_TARGET is required")
		return 2
	}
	sutToken, err := token(ctx, *dockerBinary, *sutContainer, os.Getenv("WEBOX_E2E_SUT_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "load SUT token: %v\n", err)
		return 1
	}
	peerToken, err := token(ctx, *dockerBinary, *peerContainer, os.Getenv("WEBOX_E2E_PEER_TOKEN"))
	if err != nil {
		fmt.Fprintf(stderr, "load peer token: %v\n", err)
		return 1
	}
	driver, err := runner.NewDockerPeerDriver(runner.DockerPeerConfig{
		DockerBinary: *dockerBinary, Container: *peerContainer,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	testRunner, err := runner.New(runner.Config{
		SUTURL: *sutURL, PeerURL: *peerURL, SUTToken: sutToken, PeerToken: peerToken,
		PeerTarget: *peerTarget, Timeout: *timeout,
		Progress: func(message string) { fmt.Fprintf(stderr, "[e2e] %s\n", message) },
	}, driver)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result, err := testRunner.RunDirect(ctx)
	if err != nil {
		artifactCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		directory, artifactErr := collectArtifacts(
			artifactCtx, *dockerBinary, *artifactsRoot,
			[]containerEndpoint{{*sutContainer, *sutURL}, {*peerContainer, *peerURL}},
		)
		fmt.Fprintf(stderr, "E2E failed: %v\n", err)
		if artifactErr != nil {
			fmt.Fprintf(stderr, "collect failure artifacts: %v\n", artifactErr)
		} else {
			fmt.Fprintf(stderr, "failure artifacts: %s\n", directory)
		}
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(stderr, "print result: %v\n", err)
		return 1
	}
	return 0
}

func token(ctx context.Context, dockerBinary, container, configured string) (string, error) {
	if value := strings.TrimSpace(configured); value != "" {
		return value, nil
	}
	output, err := exec.CommandContext(
		ctx, dockerBinary, "exec", container, "cat", "/webox/state/weagent/api-token",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read %s token: %w: %s", container, err, strings.TrimSpace(string(output)))
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", errors.New("token is empty")
	}
	return value, nil
}

type containerEndpoint struct {
	container string
	baseURL   string
}

func collectArtifacts(ctx context.Context, dockerBinary, root string, endpoints []containerEndpoint) (string, error) {
	directory := filepath.Join(root, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	var failures []string
	for _, endpoint := range endpoints {
		name := safeName(endpoint.container)
		logs, err := exec.CommandContext(ctx, dockerBinary, "logs", "--since", "10m", endpoint.container).CombinedOutput()
		if writeErr := os.WriteFile(filepath.Join(directory, name+".log"), logs, 0o600); writeErr != nil {
			failures = append(failures, writeErr.Error())
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("logs %s: %v", endpoint.container, err))
		}
		health, healthErr := fetchHealth(ctx, endpoint.baseURL)
		if healthErr != nil {
			health = []byte(healthErr.Error() + "\n")
		}
		if err := os.WriteFile(filepath.Join(directory, name+"-health.json"), health, 0o600); err != nil {
			failures = append(failures, err.Error())
		}
		remote := "/tmp/webox-e2e-screen.png"
		capture := exec.CommandContext(ctx, dockerBinary, "exec", endpoint.container, "bash", "-lc",
			"ffmpeg -loglevel error -f x11grab -video_size 1280x800 -i :1 -frames:v 1 "+remote+" -y")
		if output, err := capture.CombinedOutput(); err != nil {
			failures = append(failures, fmt.Sprintf("screenshot %s: %v: %s", endpoint.container, err, strings.TrimSpace(string(output))))
			continue
		}
		if output, err := exec.CommandContext(
			ctx, dockerBinary, "cp", endpoint.container+":"+remote, filepath.Join(directory, name+"-screen.png"),
		).CombinedOutput(); err != nil {
			failures = append(failures, fmt.Sprintf("copy screenshot %s: %v: %s", endpoint.container, err, strings.TrimSpace(string(output))))
		}
	}
	if len(failures) != 0 {
		return directory, errors.New(strings.Join(failures, "; "))
	}
	return directory, nil
}

func fetchHealth(ctx context.Context, baseURL string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(io.LimitReader(response.Body, 4096))
}

func safeName(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' {
			return character
		}
		return '_'
	}, value)
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
