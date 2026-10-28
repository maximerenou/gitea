// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agents

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	agents_model "gitea.dev/models/agents"
	"gitea.dev/modules/egress/policy"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/proxy"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/util"
)

func endpointPolicy() *policy.Policy {
	mode := policy.Lax
	if setting.Security.EgressMode == "strict" {
		mode = policy.Strict
	}
	return policy.NewPolicy("agents", mode,
		policy.WithAllow(setting.Agents.AllowedHostList, "agents.ALLOWED_HOST_LIST"),
		policy.WithLocalNeedsIPAllow(),
		policy.WithProxy(proxy.Proxy()))
}

// NormalizeBaseURL validates a provider base URL against the egress policy, private networks need an explicit allow entry
func NormalizeBaseURL(provider agents_model.Provider, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if provider == agents_model.ProviderOpenAI && rawURL == "" {
		rawURL = agents_model.OpenAIBaseURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", util.NewInvalidArgumentErrorf("the base URL must be an http(s) URL without credentials, query or fragment")
	}
	if err := endpointPolicy().CheckHostIPs(u); err != nil {
		return "", util.NewInvalidArgumentErrorf("%v", err)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// CheckResult is the outcome of a connection and compatibility test
type CheckResult struct {
	OK      bool
	Message string
}

// CheckConnection streams a minimal Responses API request, the protocol Codex CLI uses, so a pass means Codex can use the endpoint and model
func CheckConnection(ctx context.Context, baseURL, model, apiKey string) CheckResult {
	if _, err := NormalizeBaseURL(agents_model.ProviderCustom, baseURL); err != nil {
		return CheckResult{Message: err.Error()}
	}
	body, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "Reply with OK.",
		"stream":            true,
		"store":             false,
		"max_output_tokens": 32,
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return CheckResult{Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Transport: endpointPolicy().NewHTTPTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Message: redactKey(err.Error(), apiKey)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return CheckResult{Message: redactKey(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(excerpt))), apiKey)}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return CheckResult{Message: "the endpoint does not stream Server-Sent Events, Codex CLI requires a streaming Responses API"}
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1024*1024))
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Response *struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			return CheckResult{OK: true, Message: "Responses API streaming works with model " + model}
		case "response.failed":
			msg := "the model failed to respond"
			if event.Response != nil && event.Response.Error != nil {
				msg = event.Response.Error.Message
			}
			return CheckResult{Message: redactKey(msg, apiKey)}
		case "error":
			msg := "the endpoint returned an error event"
			if event.Error != nil {
				msg = event.Error.Message
			}
			return CheckResult{Message: redactKey(msg, apiKey)}
		}
	}
	return CheckResult{Message: "the stream ended without a response.completed event, the endpoint is not compatible with the Responses API used by Codex CLI"}
}

func redactKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// modelAPIs are the provider endpoints Codex needs, a model token must not reach the rest of the account of the key's owner
var modelAPIs = []string{"POST responses", "POST responses/compact", "GET models"}

// forwardedModelHeaders are the request headers Codex sends that the provider needs, its Authorization is replaced by the user's key
var forwardedModelHeaders = []string{"Content-Type", "Accept", "OpenAI-Beta", "Session_id", "Conversation_id", "Originator", "User-Agent", "Version"}

// clientTools are the tool types Codex runs itself, hosted tools would act on the key owner's account or reach
// the network from the provider, past the firewall
var clientTools = []any{"function", "custom", "local_shell", "namespace", "tool_search"}

// checkResponsesRequest re-encodes a Responses request with the run's model, without hosted tools and refusing remote inputs
func checkResponsesRequest(body io.Reader, model, subPath string) (io.Reader, error) {
	const maxRequestSize = 64 * 1024 * 1024
	content, err := io.ReadAll(io.LimitReader(body, maxRequestSize+1))
	if err != nil || len(content) > maxRequestSize {
		return nil, util.NewInvalidArgumentErrorf("the model request cannot be read or exceeds its size limit")
	}
	var request map[string]any
	if err := json.Unmarshal(content, &request); err != nil || request == nil {
		return nil, util.NewInvalidArgumentErrorf("the model request is not a JSON object")
	}
	if request["tools"] != nil {
		tools, err := clientToolsOf(request["tools"])
		if err != nil {
			return nil, err
		}
		request["tools"] = tools
	}
	if choice, ok := request["tool_choice"].(map[string]any); ok && !slices.Contains(clientTools, choice["type"]) {
		request["tool_choice"] = "auto"
	}
	if hasRemoteInput(request["input"]) {
		return nil, util.NewInvalidArgumentErrorf("agents cannot send remote images or files to the model")
	}
	request["model"] = model
	request["store"], request["background"] = false, false
	delete(request, "previous_response_id")
	delete(request, "conversation")
	limit := float64(setting.Agents.MaxModelOutputTokens)
	if requested, ok := request["max_output_tokens"].(float64); ok && requested > 0 {
		limit = min(limit, requested)
	}
	request["max_output_tokens"] = limit
	if subPath == "responses/compact" {
		for key := range request {
			if !slices.Contains([]string{"model", "input", "instructions"}, key) {
				delete(request, key)
			}
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(encoded), nil
}

// clientToolsOf drops the hosted tools, so a Codex version enabling one still works without it
func clientToolsOf(tools any) ([]any, error) {
	list, ok := tools.([]any)
	if !ok {
		return nil, util.NewInvalidArgumentErrorf("the model request has invalid tools")
	}
	kept := make([]any, 0, len(list))
	for _, tool := range list {
		spec, _ := tool.(map[string]any)
		if !slices.Contains(clientTools, spec["type"]) {
			continue
		}
		if spec["tools"] != nil { // a namespace groups tools
			nested, err := clientToolsOf(spec["tools"])
			if err != nil {
				return nil, err
			}
			spec["tools"] = nested
		}
		kept = append(kept, spec)
	}
	return kept, nil
}

// hasRemoteInput finds the image and file URLs the provider would fetch, Codex only sends data URLs
func hasRemoteInput(v any) bool {
	switch v := v.(type) {
	case []any:
		return slices.ContainsFunc(v, hasRemoteInput)
	case map[string]any:
		for key, value := range v {
			if key == "image_url" || key == "file_url" {
				if nested, ok := value.(map[string]any); ok {
					value = nested["url"]
				}
				if s, _ := value.(string); s != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "data:") {
					return true
				}
			} else if hasRemoteInput(value) {
				return true
			}
		}
	}
	return false
}

// ProxyModel forwards a model request of a run's job to the agent's endpoint with the trigger user's API key,
// so the key never enters the job: the repository's setup steps run as root in the same container
func ProxyModel(ctx context.Context, r *agents_model.Run, method, subPath string, header http.Header, body io.Reader) (*http.Response, error) {
	subPath = strings.TrimPrefix(subPath, "/")
	if !slices.Contains(modelAPIs, method+" "+subPath) {
		return nil, util.NewPermissionDeniedErrorf("agents cannot use the model API %s %s", method, subPath)
	}
	claimed, err := agents_model.ClaimModelRequest(ctx, r.ID, setting.Agents.MaxModelRequestsPerRun)
	if err != nil {
		return nil, err
	} else if !claimed {
		return nil, util.NewPermissionDeniedErrorf("the run is finished, has a model request in flight or has reached its model request limit")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	finished := make(chan struct{})
	go monitorModelRun(requestCtx, cancel, r.ID, finished)
	cleanup := func() {
		cancel()
		<-finished
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		if err := agents_model.ReleaseModelRequest(cleanupCtx, r.ID); err != nil {
			log.Error("agents: release model request of run %d: %v", r.ID, err)
		}
	}
	transferred := false
	defer func() {
		if !transferred {
			cleanup()
		}
	}()
	s, err := agents_model.GetSessionByID(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	agent, err := agents_model.GetAgentByID(ctx, s.AgentID)
	if err != nil {
		return nil, err
	}
	if agent.IsDisabled {
		return nil, util.NewPermissionDeniedErrorf("the agent is disabled")
	}
	userKey, err := agents_model.GetUserKey(ctx, agent.ID, r.TriggerUserID)
	if err != nil {
		return nil, err
	}
	key, err := userKey.APIKey()
	if err != nil {
		return nil, err
	}
	baseURL, err := NormalizeBaseURL(agent.Provider, agent.BaseURL)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		if closer, ok := body.(io.Closer); ok {
			stop := context.AfterFunc(requestCtx, func() { _ = closer.Close() })
			defer stop()
		}
		if body, err = checkResponsesRequest(body, userKey.EffectiveModel(agent), subPath); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(requestCtx, method, baseURL+"/"+subPath, body)
	if err != nil {
		return nil, err
	}
	for _, name := range forwardedModelHeaders {
		if v := header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Transport: endpointPolicy().NewHTTPTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New(redactKey(err.Error(), key))
	}
	transferred = true
	resp.Body = &modelResponseBody{
		ReadCloser: resp.Body, ctx: context.WithoutCancel(ctx), runID: r.ID,
		stream: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), cleanup: cleanup,
	}
	return resp, nil
}
