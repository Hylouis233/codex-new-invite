package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// normalizeCredentialBaseURL prevents management requests or plugin YAML from
// redirecting credential-bearing traffic to an attacker-controlled origin.
// Proxying remains supported through the separate proxy_url setting.
func normalizeCredentialBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid ChatGPT base URL: %w", err)
	}
	path := parsed.EscapedPath()
	if parsed.Scheme != "https" ||
		parsed.User != nil ||
		!strings.EqualFold(parsed.Hostname(), "chatgpt.com") ||
		parsed.Port() != "" ||
		(path != "" && path != "/") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", fmt.Errorf("ChatGPT base URL must be %s", defaultBaseURL)
	}
	return defaultBaseURL, nil
}

// UnmarshalJSON validates the base_url before a request can carry a selected or
// manually supplied ChatGPT credential to an upstream endpoint.
func (request *inviteRequest) UnmarshalJSON(data []byte) error {
	type wireInviteRequest inviteRequest
	var decoded wireInviteRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	baseURL, err := normalizeCredentialBaseURL(decoded.BaseURL)
	if err != nil {
		return err
	}
	decoded.BaseURL = baseURL
	*request = inviteRequest(decoded)
	return nil
}

// queryRequest is shared by usage, referral, probe, and redeem handlers, so the
// same origin invariant must apply to every credential-bearing query route.
func (request *queryRequest) UnmarshalJSON(data []byte) error {
	type wireQueryRequest queryRequest
	var decoded wireQueryRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	baseURL, err := normalizeCredentialBaseURL(decoded.BaseURL)
	if err != nil {
		return err
	}
	decoded.BaseURL = baseURL
	*request = queryRequest(decoded)
	return nil
}

// UnmarshalYAML applies the same restriction to lifecycle configuration. A
// custom network path should use proxy_url rather than replacing the upstream
// origin that receives Authorization and Cookie headers.
func (config *pluginConfig) UnmarshalYAML(node *yaml.Node) error {
	type wirePluginConfig pluginConfig
	var decoded wirePluginConfig
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	baseURL, err := normalizeCredentialBaseURL(decoded.BaseURL)
	if err != nil {
		return err
	}
	decoded.BaseURL = baseURL
	*config = pluginConfig(decoded)
	return nil
}

// autoAssignRequest carries the same credential-bearing base_url as the invite and query
// requests, so the identical origin invariant applies to the auto-assign route.
func (request *autoAssignRequest) UnmarshalJSON(data []byte) error {
	type wireAutoAssignRequest autoAssignRequest
	var decoded wireAutoAssignRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	baseURL, err := normalizeCredentialBaseURL(decoded.BaseURL)
	if err != nil {
		return err
	}
	decoded.BaseURL = baseURL
	*request = autoAssignRequest(decoded)
	return nil
}
