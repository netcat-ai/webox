package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 3 * time.Minute

type Config struct {
	SUTURL     string
	PeerURL    string
	SUTToken   string
	PeerToken  string
	PeerTarget string
	Timeout    time.Duration
	Progress   func(string)
}

type PeerDriver interface {
	Send(ctx context.Context, target, text string) error
}

type Result struct {
	RequestText    string
	ReplyText      string
	IncomingID     string
	ReplyMessageID string
	GroupID        string
	ReplyFrom      string
}

type Runner struct {
	config Config
	sut    *iLinkClient
	peer   *iLinkClient
	driver PeerDriver
}

func New(config Config, driver PeerDriver) (*Runner, error) {
	if driver == nil {
		return nil, errors.New("peer driver is required")
	}
	if strings.TrimSpace(config.PeerTarget) == "" {
		return nil, errors.New("peer target remark is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	sut, err := newILinkClient(config.SUTURL, config.SUTToken)
	if err != nil {
		return nil, fmt.Errorf("configure SUT iLink client: %w", err)
	}
	peer, err := newILinkClient(config.PeerURL, config.PeerToken)
	if err != nil {
		return nil, fmt.Errorf("configure peer iLink client: %w", err)
	}
	return &Runner{config: config, sut: sut, peer: peer, driver: driver}, nil
}

func (runner *Runner) RunDirect(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, runner.config.Timeout)
	defer cancel()
	if err := runner.checkReadiness(ctx); err != nil {
		return Result{}, err
	}
	runner.progress("establishing both message baselines; this can take up to 35 seconds")
	sutCursor, peerCursor, err := runner.baselines(ctx)
	if err != nil {
		return Result{}, err
	}
	requestText := uniqueText()
	runner.progress("sending peer message " + requestText)
	if err := runner.driver.Send(ctx, runner.config.PeerTarget, requestText); err != nil {
		return Result{}, fmt.Errorf("send peer test message: %w", err)
	}
	runner.progress("waiting for SUT to receive the peer message")
	incoming, _, err := runner.sut.waitForText(ctx, sutCursor, requestText)
	if err != nil {
		return Result{}, fmt.Errorf("wait for SUT inbound message: %w", err)
	}
	replyText := "ACK_" + requestText
	runner.progress("sending SUT reply " + replyText)
	if err := runner.sut.sendText(ctx, incoming.ContextToken, "reply-"+requestText, replyText); err != nil {
		return Result{}, fmt.Errorf("send SUT reply: %w", err)
	}
	runner.progress("waiting for peer to receive the SUT reply")
	reply, _, err := runner.peer.waitForText(ctx, peerCursor, replyText)
	if err != nil {
		return Result{}, fmt.Errorf("wait for peer reply: %w", err)
	}
	return Result{
		RequestText: requestText, ReplyText: replyText,
		IncomingID: incoming.MessageID, ReplyMessageID: reply.MessageID,
	}, nil
}

func (runner *Runner) RunOpenClawDirect(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, runner.config.Timeout)
	defer cancel()
	if err := runner.checkReadiness(ctx); err != nil {
		return Result{}, err
	}
	runner.progress("establishing peer message baseline; this can take up to 35 seconds")
	updates, err := runner.peer.getUpdates(ctx, "")
	if err != nil {
		return Result{}, fmt.Errorf("establish peer baseline: %w", err)
	}
	if strings.TrimSpace(updates.Cursor) == "" {
		return Result{}, errors.New("establish peer baseline: response has no cursor")
	}
	replyText := uniqueTextWithPrefix("WEBOX_OPENCLAW_E2E_")
	requestText := "Reply with exactly this token and nothing else: " + replyText
	runner.progress("sending OpenClaw prompt for " + replyText)
	if err := runner.driver.Send(ctx, runner.config.PeerTarget, requestText); err != nil {
		return Result{}, fmt.Errorf("send peer OpenClaw prompt: %w", err)
	}
	runner.progress("waiting for peer to receive the OpenClaw agent reply")
	reply, _, err := runner.peer.waitForText(ctx, updates.Cursor, replyText)
	if err != nil {
		return Result{}, fmt.Errorf("wait for OpenClaw agent reply: %w", err)
	}
	if strings.HasSuffix(reply.GroupID, "@chatroom") {
		return Result{}, fmt.Errorf("matching reply is not a direct message: group_id=%q", reply.GroupID)
	}
	return Result{RequestText: requestText, ReplyText: replyText, ReplyMessageID: reply.MessageID}, nil
}

func (runner *Runner) RunOpenClawGroup(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, runner.config.Timeout)
	defer cancel()
	if err := runner.checkReadiness(ctx); err != nil {
		return Result{}, err
	}
	runner.progress("establishing peer message baseline; this can take up to 35 seconds")
	updates, err := runner.peer.getUpdates(ctx, "")
	if err != nil {
		return Result{}, fmt.Errorf("establish peer baseline: %w", err)
	}
	if strings.TrimSpace(updates.Cursor) == "" {
		return Result{}, errors.New("establish peer baseline: response has no cursor")
	}
	replyText := uniqueTextWithPrefix("WEBOX_OPENCLAW_GROUP_E2E_")
	requestText := "虾虾，reply with exactly this token and nothing else: " + replyText
	runner.progress("sending OpenClaw group prompt for " + replyText)
	if err := runner.driver.Send(ctx, runner.config.PeerTarget, requestText); err != nil {
		return Result{}, fmt.Errorf("send peer OpenClaw group prompt: %w", err)
	}
	runner.progress("waiting for peer to receive the OpenClaw group reply")
	reply, _, err := runner.peer.waitForText(ctx, updates.Cursor, replyText)
	if err != nil {
		return Result{}, fmt.Errorf("wait for OpenClaw group reply: %w", err)
	}
	if !strings.HasSuffix(reply.GroupID, "@chatroom") {
		return Result{}, fmt.Errorf("matching reply is not a group message: group_id=%q", reply.GroupID)
	}
	if reply.SessionID != reply.GroupID {
		return Result{}, fmt.Errorf("group reply session mismatch: session_id=%q group_id=%q", reply.SessionID, reply.GroupID)
	}
	return Result{
		RequestText: requestText, ReplyText: replyText, ReplyMessageID: reply.MessageID,
		GroupID: reply.GroupID, ReplyFrom: reply.FromUserID,
	}, nil
}

func (runner *Runner) checkReadiness(ctx context.Context) error {
	runner.progress("checking SUT and peer readiness")
	for _, endpoint := range []struct {
		name   string
		client *iLinkClient
	}{{"SUT", runner.sut}, {"peer", runner.peer}} {
		ready, err := endpoint.client.ready(ctx)
		if err != nil {
			return fmt.Errorf("check %s health: %w", endpoint.name, err)
		}
		if !ready {
			return fmt.Errorf("%s is not ready; complete WeChat login before running E2E", endpoint.name)
		}
	}
	return nil
}

func (runner *Runner) progress(message string) {
	if runner.config.Progress != nil {
		runner.config.Progress(message)
	}
}

func (client *iLinkClient) ready(ctx context.Context) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/healthz", nil)
	if err != nil {
		return false, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return false, fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var body struct {
		OK    bool `json:"ok"`
		Ready bool `json:"ready"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("decode response: %w", err)
	}
	if !body.OK {
		return false, errors.New("health response has ok=false")
	}
	return body.Ready, nil
}

func (runner *Runner) baselines(ctx context.Context) (string, string, error) {
	type result struct {
		name   string
		cursor string
		err    error
	}
	results := make(chan result, 2)
	baseline := func(name string, client *iLinkClient) {
		updates, err := client.getUpdates(ctx, "")
		results <- result{name: name, cursor: updates.Cursor, err: err}
	}
	go baseline("sut", runner.sut)
	go baseline("peer", runner.peer)
	var sutCursor, peerCursor string
	for range 2 {
		item := <-results
		if item.err != nil {
			return "", "", fmt.Errorf("establish %s baseline: %w", item.name, item.err)
		}
		if strings.TrimSpace(item.cursor) == "" {
			return "", "", fmt.Errorf("establish %s baseline: response has no cursor", item.name)
		}
		if item.name == "sut" {
			sutCursor = item.cursor
		} else {
			peerCursor = item.cursor
		}
	}
	return sutCursor, peerCursor, nil
}

type iLinkClient struct {
	baseURL string
	token   string
	uin     string
	http    *http.Client
}

type updatesResponse struct {
	Ret      int       `json:"ret"`
	ErrCode  int       `json:"errcode"`
	Errmsg   string    `json:"errmsg"`
	Cursor   string    `json:"get_updates_buf"`
	Messages []message `json:"msgs"`
}

type message struct {
	MessageID    string `json:"msgid"`
	Text         string `json:"text"`
	ContextToken string `json:"context_token"`
	FromUserID   string `json:"from_user_id"`
	SessionID    string `json:"session_id"`
	GroupID      string `json:"group_id"`
}

func newILinkClient(rawURL, token string) (*iLinkClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", rawURL)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("bot token is required")
	}
	uinBytes := make([]byte, 4)
	if _, err := rand.Read(uinBytes); err != nil {
		copy(uinBytes, []byte{1, 2, 3, 4})
	}
	return &iLinkClient{
		baseURL: strings.TrimRight(parsed.String(), "/"), token: strings.TrimSpace(token),
		uin: base64.StdEncoding.EncodeToString(uinBytes), http: &http.Client{},
	}, nil
}

func (client *iLinkClient) getUpdates(ctx context.Context, cursor string) (updatesResponse, error) {
	var response updatesResponse
	err := client.post(ctx, "/ilink/bot/getupdates", map[string]any{
		"get_updates_buf": cursor,
		"base_info":       map[string]any{},
	}, &response)
	if err != nil {
		return response, err
	}
	if response.Ret != 0 {
		return response, fmt.Errorf("iLink ret=%d errcode=%d errmsg=%s", response.Ret, response.ErrCode, response.Errmsg)
	}
	return response, nil
}

func (client *iLinkClient) waitForText(ctx context.Context, cursor, expected string) (message, string, error) {
	for {
		updates, err := client.getUpdates(ctx, cursor)
		if err != nil {
			return message{}, cursor, err
		}
		if strings.TrimSpace(updates.Cursor) == "" {
			return message{}, cursor, errors.New("getupdates response has no cursor")
		}
		cursor = updates.Cursor
		for _, candidate := range updates.Messages {
			if candidate.Text == expected {
				if strings.TrimSpace(candidate.ContextToken) == "" {
					return message{}, cursor, errors.New("matching message has no context_token")
				}
				return candidate, cursor, nil
			}
		}
	}
}

func (client *iLinkClient) sendText(ctx context.Context, contextToken, clientID, text string) error {
	var response struct {
		Ret     int    `json:"ret"`
		ErrCode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	err := client.post(ctx, "/ilink/bot/sendmessage", map[string]any{
		"msg": map[string]any{
			"client_id": clientID, "context_token": contextToken,
			"item_list": []any{map[string]any{
				"type": 1, "text_item": map[string]any{"text": text},
			}},
		},
	}, &response)
	if err != nil {
		return err
	}
	if response.Ret != 0 {
		return fmt.Errorf("iLink ret=%d errcode=%d errmsg=%s", response.Ret, response.ErrCode, response.Errmsg)
	}
	return nil
}

func (client *iLinkClient) post(ctx context.Context, path string, body, response any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("AuthorizationType", "ilink_bot_token")
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("X-WECHAT-UIN", client.uin)
	httpResponse, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = httpResponse.Body.Close() }()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", httpResponse.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.NewDecoder(httpResponse.Body).Decode(response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func uniqueText() string {
	return uniqueTextWithPrefix("WEBOX_E2E_")
}

func uniqueTextWithPrefix(prefix string) string {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return prefix + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return prefix + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(random)
}
