package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"os"
	"os/exec"
	"time"
	"unsafe"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v3"
)

const (
	pluginID                      = "codex-invite"
	defaultReferralKey            = "codex_referral_persistent_invite"
	defaultBaseURL                = "https://chatgpt.com"
	defaultLanguage               = "zh-CN"
	defaultOriginator             = "Codex Desktop"
	defaultUserAgent              = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	defaultMaxEmails              = 10
	upperMaxEmails                = 50
	maxManagementBodyBytes        = 1 << 20
	managementAccountsPath        = "/v0/management/codex-invite/accounts"
	managementInvitePath          = "/v0/management/codex-invite/invite"
	managementUsagePath           = "/v0/management/codex-invite/usage"
	managementReferralsPath       = "/v0/management/codex-invite/referrals"
	managementProbePath           = "/v0/management/codex-invite/probe"
	managementRedeemPath          = "/v0/management/codex-invite/redeem"
	managementActivatePath        = "/v0/management/codex-invite/activate"
	managementCDPActivatePath     = "/v0/management/codex-invite/cdp-activate"
	managementDesktopActivatePath = "/v0/management/codex-invite/desktop-activate"
	resourceInvitePath            = "/v0/resource/plugins/codex-invite/invite"
	resourceUsagePath             = "/v0/resource/plugins/codex-invite/usage"
	authFilesPath                 = "/v0/management/auth-files"
	authFileDownloadPath          = "/v0/management/auth-files/download"
	inviteEndpointPath            = "/backend-api/wham/referrals/invite"
	// inviteEndpointPathV2 is the new referral invite endpoint used by the Codex desktop app.
	// Body: {program_id, entrypoint, emails}. Reverse-engineered from app.asar.
	inviteEndpointPathV2          = "/backend-api/referrals/invite"
	usageEndpointPath             = "/backend-api/codex/usage"
	referralsStatusEndpointPath   = "/backend-api/wham/referrals/status"
	referralsCreditsEndpointPath  = "/backend-api/wham/referrals/credits"
	// resetCreditsEndpointPath lists banked rate-limit reset credits (referral-granted).
	// Reverse-engineered from the openai.chatgpt VS Code extension webview bundle.
	resetCreditsEndpointPath        = "/backend-api/wham/rate-limit-reset-credits"
	// consumeCreditsEndpointPath redeems one banked reset credit. Body: {credit_id, redeem_request_id}.
	consumeCreditsEndpointPath      = "/backend-api/wham/rate-limit-reset-credits/consume"
	// inviteEligibilityEndpointPath probes invite eligibility. Reverse-engineered from
	// the ChatGPT Codex desktop app (app.asar). Requires program_id + entrypoint query
	// params; values depend on account type (consumer vs workspace) and trigger context.
	inviteEligibilityEndpointPath   = "/backend-api/referrals/invite/eligibility"
	// inviteTrackingEndpointPath lists sent invite records. Reverse-engineered from the
	// ChatGPT Codex desktop app.
	inviteTrackingEndpointPath      = "/backend-api/referrals/invite/tracking"
	// programId values for the referral eligibility/tracking endpoints.
	programIDConsumer               = "codex_referral_consumer"
	programIDWorkspace              = "codex_referral_workspace"
	// entrypoint values.
	entrypointPersistent            = "persistent"
	entrypointRateLimit             = "rate_limit"
	requestManagementOrigin       = "X-Codex-Invite-Origin"
	contentTypeJSON               = "application/json; charset=utf-8"
	contentTypeHTML               = "text/html; charset=utf-8"
	upstreamBodyLimit       int64 = 1 << 20
)

var pluginVersion = "0.2.0"

var (
	activeConfig atomic.Value
	emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

func init() {
	activeConfig.Store(defaultConfig())
}

func main() {}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type pluginConfig struct {
	ReferralKey         string `yaml:"referral_key"`
	BaseURL             string `yaml:"base_url"`
	Language            string `yaml:"language"`
	Originator          string `yaml:"originator"`
	UserAgent           string `yaml:"user_agent"`
	Cookie              string `yaml:"cookie"`
	MaxEmailsPerRequest int    `yaml:"max_emails_per_request"`
}

type accountInfo struct {
	AuthIndex        string `json:"auth_index,omitempty"`
	Name             string `json:"name"`
	Label            string `json:"label,omitempty"`
	Email            string `json:"email,omitempty"`
	Account          string `json:"account,omitempty"`
	ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`
	Status           string `json:"status,omitempty"`
	Source           string `json:"source,omitempty"`
}

type accountsResponse struct {
	Accounts []accountInfo `json:"accounts"`
}

type inviteRequest struct {
	AuthIndex           string   `json:"auth_index,omitempty"`
	AuthName            string   `json:"auth_name,omitempty"`
	Emails              []string `json:"emails,omitempty"`
	EmailsText          string   `json:"emails_text,omitempty"`
	ReferralKey         string   `json:"referral_key,omitempty"`
	BaseURL             string   `json:"base_url,omitempty"`
	ProxyURL            string   `json:"proxy_url,omitempty"`
	Language            string   `json:"language,omitempty"`
	Originator          string   `json:"originator,omitempty"`
	UserAgent           string   `json:"user_agent,omitempty"`
	Cookie              string   `json:"cookie,omitempty"`
	MaxEmailsPerRequest int      `json:"max_emails_per_request,omitempty"`
	ManagementOrigin    string   `json:"management_origin,omitempty"`
	// Manual credential mode: when AccessToken is set, the handler skips CPA auth-file
	// lookup and uses these fields directly. This lets users invite/query with a
	// credential not managed by CPA.
	AccessToken string `json:"access_token,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	ManualEmail string `json:"manual_email,omitempty"`
}

type inviteLink struct {
	ReferralID string `json:"referral_id,omitempty"`
	Email      string `json:"email,omitempty"`
	InviteURL  string `json:"invite_url,omitempty"`
}

type inviteResponse struct {
	OK          bool         `json:"ok"`
	StatusCode  int          `json:"status_code"`
	RequestID   string       `json:"request_id,omitempty"`
	Account     accountInfo  `json:"account"`
	Emails      []string     `json:"emails"`
	ReferralKey string       `json:"referral_key"`
	Invites     []inviteLink `json:"invites,omitempty"`
	Upstream    any          `json:"upstream,omitempty"`
	UpstreamRaw string       `json:"upstream_raw,omitempty"`
}

type codexCredential struct {
	AccessToken string
	AccountID   string
	Email       string
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/codex-invite/accounts"},
			{Method: http.MethodPost, Path: "/codex-invite/invite"},
				{Method: http.MethodPost, Path: "/codex-invite/usage"},
				{Method: http.MethodPost, Path: "/codex-invite/referrals"},
				{Method: http.MethodPost, Path: "/codex-invite/probe"},
				{Method: http.MethodPost, Path: "/codex-invite/redeem"},
				{Method: http.MethodPost, Path: "/codex-invite/activate"},
				{Method: http.MethodPost, Path: "/codex-invite/cdp-activate"},
				{Method: http.MethodPost, Path: "/codex-invite/desktop-activate"},
		},
			Resources: []pluginapi.ResourceRoute{
				{
					Path:        "/invite",
					Menu:        "Codex Invite",
					Description: "Send Codex invite emails with a selected Codex credential.",
				},
				{
					Path:        "/usage",
					Menu:        "Codex Usage",
					Description: "Query Codex account credit balance, rate-limit usage, and referral credits.",
				},
			},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}

	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		var decoded pluginConfig
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &decoded); errUnmarshal != nil {
			return errUnmarshal
		}
		cfg = mergeConfig(cfg, decoded)
	}
	activeConfig.Store(normalizeConfig(cfg))
	return nil
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		ReferralKey:         defaultReferralKey,
		BaseURL:             defaultBaseURL,
		Language:            defaultLanguage,
		Originator:          defaultOriginator,
		UserAgent:           defaultUserAgent,
		MaxEmailsPerRequest: defaultMaxEmails,
	}
}

func mergeConfig(base, override pluginConfig) pluginConfig {
	if strings.TrimSpace(override.ReferralKey) != "" {
		base.ReferralKey = override.ReferralKey
	}
	if strings.TrimSpace(override.BaseURL) != "" {
		base.BaseURL = override.BaseURL
	}
	if strings.TrimSpace(override.Language) != "" {
		base.Language = override.Language
	}
	if strings.TrimSpace(override.Originator) != "" {
		base.Originator = override.Originator
	}
	if strings.TrimSpace(override.UserAgent) != "" {
		base.UserAgent = override.UserAgent
	}
	if strings.TrimSpace(override.Cookie) != "" {
		base.Cookie = override.Cookie
	}
	if override.MaxEmailsPerRequest != 0 {
		base.MaxEmailsPerRequest = override.MaxEmailsPerRequest
	}
	return base
}

func normalizeConfig(cfg pluginConfig) pluginConfig {
	cfg.ReferralKey = strings.TrimSpace(cfg.ReferralKey)
	if cfg.ReferralKey == "" {
		cfg.ReferralKey = defaultReferralKey
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.Language = strings.TrimSpace(cfg.Language)
	if cfg.Language == "" {
		cfg.Language = defaultLanguage
	}
	cfg.Originator = strings.TrimSpace(cfg.Originator)
	if cfg.Originator == "" {
		cfg.Originator = defaultOriginator
	}
	cfg.UserAgent = strings.TrimSpace(cfg.UserAgent)
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	cfg.Cookie = strings.TrimSpace(cfg.Cookie)
	if cfg.MaxEmailsPerRequest <= 0 {
		cfg.MaxEmailsPerRequest = defaultMaxEmails
	}
	if cfg.MaxEmailsPerRequest > upperMaxEmails {
		cfg.MaxEmailsPerRequest = upperMaxEmails
	}
	return cfg
}

func currentConfig() pluginConfig {
	raw := activeConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Codex Invite",
			Version:          pluginVersion,
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/cpa-plugin-codex-invite",
		},
		Capabilities: registrationCapabilities{ManagementAPI: true},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	if path == "" {
		path = "/"
	}

	switch {
	case strings.EqualFold(req.Method, http.MethodGet) && path == resourceInvitePath:
		return okEnvelope(htmlResponse(http.StatusOK, renderInvitePage(currentConfig())))
	case strings.EqualFold(req.Method, http.MethodGet) && path == resourceUsagePath:
		return okEnvelope(htmlResponse(http.StatusOK, renderUsagePage(currentConfig())))
	case strings.EqualFold(req.Method, http.MethodGet) && path == managementAccountsPath:
		return okEnvelope(handleAccounts(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementInvitePath:
		return okEnvelope(handleInvite(req.ManagementRequest))
	case (strings.EqualFold(req.Method, http.MethodGet) || strings.EqualFold(req.Method, http.MethodPost)) && path == managementUsagePath:
		return okEnvelope(handleUsage(req.ManagementRequest))
	case (strings.EqualFold(req.Method, http.MethodGet) || strings.EqualFold(req.Method, http.MethodPost)) && path == managementReferralsPath:
		return okEnvelope(handleReferrals(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementProbePath:
		return okEnvelope(handleProbe(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementRedeemPath:
		return okEnvelope(handleRedeem(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementActivatePath:
		return okEnvelope(handleActivate(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementCDPActivatePath:
		return okEnvelope(handleCDPActivate(req.ManagementRequest))
	case strings.EqualFold(req.Method, http.MethodPost) && path == managementDesktopActivatePath:
		return okEnvelope(handleDesktopActivate(req.ManagementRequest))
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{"error": "plugin route not found"}))
	}
}

func handleAccounts(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	accounts, errAccounts := fetchCodexAccounts(req, "")
	if errAccounts != nil {
		return jsonResponse(statusForError(errAccounts), map[string]any{"error": errAccounts.Error()})
	}
	return jsonResponse(http.StatusOK, accountsResponse{Accounts: accounts})
}

func handleInvite(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > maxManagementBodyBytes {
		return jsonResponse(http.StatusRequestEntityTooLarge, map[string]any{"error": "request body is too large"})
	}
	var payload inviteRequest
	if errUnmarshal := json.Unmarshal(req.Body, &payload); errUnmarshal != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid JSON request body"})
	}

	cfg := currentConfig()
	requestCfg := mergeConfig(cfg, pluginConfig{
		BaseURL:    payload.BaseURL,
		Language:   payload.Language,
		Originator: payload.Originator,
		UserAgent:  payload.UserAgent,
	})
	requestCfg = normalizeConfig(requestCfg)

	maxEmails := cfg.MaxEmailsPerRequest
	if payload.MaxEmailsPerRequest > 0 && payload.MaxEmailsPerRequest < maxEmails {
		maxEmails = payload.MaxEmailsPerRequest
	}
	emails, errEmails := collectEmails(payload, maxEmails)
	if errEmails != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": errEmails.Error()})
	}

	// Manual credential mode: when an access_token is supplied, use it directly instead
	// of looking the credential up through CPA auth files.
	credential, account, manual := resolveManualCredential(payload.AccessToken, payload.AccountID, payload.ManualEmail)
	if !manual {
		accounts, errAccounts := fetchCodexAccounts(req, payload.ManagementOrigin)
		if errAccounts != nil {
			return jsonResponse(statusForError(errAccounts), map[string]any{"error": errAccounts.Error()})
		}
		var errAccount error
		account, errAccount = selectAccount(accounts, payload)
		if errAccount != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errAccount.Error()})
		}
		var errCredential error
		credential, errCredential = fetchCodexCredential(req, payload.ManagementOrigin, account)
		if errCredential != nil {
			return jsonResponse(statusForError(errCredential), map[string]any{"error": errCredential.Error()})
		}
	}
	if credential.AccountID == "" {
		credential.AccountID = account.ChatGPTAccountID
	}
	if credential.Email == "" {
		credential.Email = account.Email
	}

	referralKey := strings.TrimSpace(payload.ReferralKey)
	if referralKey == "" {
		referralKey = requestCfg.ReferralKey
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, errSend := sendInvite(ctx, requestCfg, credential, account, emails, referralKey, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL))
	if errSend != nil {
		return jsonResponse(statusForError(errSend), map[string]any{"error": errSend.Error()})
	}
	return jsonResponse(http.StatusOK, result)
}

// queryRequest is the shared input shape for the usage and referrals query endpoints.
type queryRequest struct {
	AuthIndex        string `json:"auth_index,omitempty"`
	AuthName         string `json:"auth_name,omitempty"`
	BaseURL          string `json:"base_url,omitempty"`
	ProxyURL         string `json:"proxy_url,omitempty"`
	Language         string `json:"language,omitempty"`
	Originator       string `json:"originator,omitempty"`
	UserAgent        string `json:"user_agent,omitempty"`
	Cookie           string `json:"cookie,omitempty"`
	ManagementOrigin string `json:"management_origin,omitempty"`
	// Manual credential mode (see inviteRequest.AccessToken).
	AccessToken string `json:"access_token,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	ManualEmail string `json:"manual_email,omitempty"`
	// Activate endpoint fields
	Email string `json:"email,omitempty"`
	Card  string `json:"card,omitempty"`
}

// parseQueryRequest decodes a query request body, tolerant of empty bodies so a plain
// GET with no JSON still works (the account can be auto-selected when only one exists).
func parseQueryRequest(body []byte) queryRequest {
	var payload queryRequest
	if len(body) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	return payload
}

// selectQueryAccount reuses the invite account lister and selector for query endpoints,
// auto-picking the first account when the caller did not specify one.
// resolveManualCredential checks whether the request carries a manually-supplied
// access_token. When present, it returns a codexCredential + a synthetic accountInfo
// directly, bypassing CPA auth-file lookup entirely. The second return is true when
// manual mode is active.
func resolveManualCredential(accessToken, accountID, manualEmail string) (codexCredential, accountInfo, bool) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return codexCredential{}, accountInfo{}, false
	}
	cred := codexCredential{
		AccessToken: accessToken,
		AccountID:   strings.TrimSpace(accountID),
		Email:       strings.TrimSpace(manualEmail),
	}
	acc := accountInfo{
		Name:    "(manual credential)",
		Email:   cred.Email,
		Account: cred.Email,
		Source:  "manual",
		Status:  "active",
	}
	return cred, acc, true
}

// resolveQueryCredential resolves the credential for usage/referrals/redeem queries.
// If a manual access_token is supplied it is used directly; otherwise the credential is
// looked up through CPA auth files (auto-selecting the first account when none requested).
func resolveQueryCredential(req pluginapi.ManagementRequest, payload queryRequest) (codexCredential, accountInfo, error) {
	if cred, acc, manual := resolveManualCredential(payload.AccessToken, payload.AccountID, payload.ManualEmail); manual {
		return cred, acc, nil
	}
	account, errAccount := selectQueryAccount(req, payload)
	if errAccount != nil {
		return codexCredential{}, accountInfo{}, errAccount
	}
	credential, errCredential := fetchCodexCredential(req, payload.ManagementOrigin, account)
	if errCredential != nil {
		return codexCredential{}, accountInfo{}, errCredential
	}
	if credential.AccountID == "" {
		credential.AccountID = account.ChatGPTAccountID
	}
	if credential.Email == "" {
		credential.Email = account.Email
	}
	return credential, account, nil
}

func selectQueryAccount(req pluginapi.ManagementRequest, payload queryRequest) (accountInfo, error) {
	accounts, errAccounts := fetchCodexAccounts(req, payload.ManagementOrigin)
	if errAccounts != nil {
		return accountInfo{}, errAccounts
	}
	// Reuse the same selection logic as invite (auth_index / auth_name), but fall back to
	// the first available account when nothing was requested, which is the common case for
	// a read-only usage query.
	manual := inviteRequest{AuthIndex: payload.AuthIndex, AuthName: payload.AuthName}
	if strings.TrimSpace(payload.AuthIndex) != "" || strings.TrimSpace(payload.AuthName) != "" {
		return selectAccount(accounts, manual)
	}
	if len(accounts) == 0 {
		return accountInfo{}, httpStatusError{status: http.StatusNotFound, msg: "no available Codex credential found"}
	}
	return accounts[0], nil
}

// usageCredits captures the credit-balance section of GET /backend-api/codex/usage.
type usageCredits struct {
	Balance        float64 `json:"balance"`
	HasSubscription bool   `json:"has_subscription,omitempty"`
}

// usageRateWindow captures one rate-limit window (primary or secondary).
type usageRateWindow struct {
	UsedPercent      float64 `json:"used_percent"`
	ResetAfterSeconds float64 `json:"reset_after_seconds,omitempty"`
}

// usageRateLimit captures the rate_limit block from the usage endpoint.
type usageRateLimit struct {
	PrimaryWindow   *usageRateWindow `json:"primary_window,omitempty"`
	SecondaryWindow *usageRateWindow `json:"secondary_window,omitempty"`
}

// usageResetCredits captures the rate_limit_reset_credits block (the credits granted via referrals).
type usageResetCredits struct {
	AvailableCount int `json:"available_count"`
	UsedCount      int `json:"used_count,omitempty"`
}

// usageResponse is the structured view returned to the management center.
type usageResponse struct {
	OK                bool               `json:"ok"`
	StatusCode        int                `json:"status_code"`
	RequestID         string             `json:"request_id,omitempty"`
	Account           accountInfo        `json:"account"`
	Credits           *usageCredits      `json:"credits,omitempty"`
	RateLimit         *usageRateLimit    `json:"rate_limit,omitempty"`
	ResetCredits      *usageResetCredits `json:"rate_limit_reset_credits,omitempty"`
	Upstream          any                `json:"upstream,omitempty"`
	UpstreamRaw       string             `json:"upstream_raw,omitempty"`
}

// referralsResponse is the structured view of remaining invite capacity.
type referralsResponse struct {
	OK                 bool        `json:"ok"`
	Account            accountInfo `json:"account"`
	RemainingInvites   any         `json:"remaining_invites,omitempty"`
	MaxInvites         any         `json:"max_invites,omitempty"`
	Status             any         `json:"status,omitempty"`
	UsageEndpointUsed  bool        `json:"usage_endpoint_used,omitempty"`
	StatusEndpointHit  bool        `json:"status_endpoint_hit,omitempty"`
	StatusStatusCode   int         `json:"status_endpoint_status_code,omitempty"`
	// Eligibility probe (GET /backend-api/referrals/invite/eligibility).
	Eligibility        any         `json:"eligibility,omitempty"`
	EligibilityHit     bool        `json:"eligibility_endpoint_hit,omitempty"`
	EligibilityStatus  int         `json:"eligibility_status_code,omitempty"`
	// Tracking probe (GET /backend-api/referrals/invite/tracking).
	Tracking           any         `json:"tracking,omitempty"`
	TrackingHit        bool        `json:"tracking_endpoint_hit,omitempty"`
	TrackingCount      int         `json:"tracking_invite_count,omitempty"`
	// Banked reset credits (GET /backend-api/wham/rate-limit-reset-credits).
	ResetCredits       any         `json:"reset_credits,omitempty"`
	ResetCreditsHit    bool        `json:"reset_credits_endpoint_hit,omitempty"`
	Upstream           any         `json:"upstream,omitempty"`
	UpstreamRaw        string      `json:"upstream_raw,omitempty"`
	Note               string      `json:"note,omitempty"`
}

func handleUsage(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)

	credential, account, errAccount := resolveQueryCredential(req, payload)
	if errAccount != nil {
		return jsonResponse(statusForError(errAccount), map[string]any{"error": errAccount.Error()})
	}

	cfg := normalizeConfig(mergeConfig(currentConfig(), pluginConfig{
		BaseURL:    payload.BaseURL,
		Language:   payload.Language,
		Originator: payload.Originator,
		UserAgent:  payload.UserAgent,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, errQuery := fetchCodexUsage(ctx, cfg, credential, account, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL))
	if errQuery != nil {
		return jsonResponse(statusForError(errQuery), map[string]any{"error": errQuery.Error()})
	}
	return jsonResponse(http.StatusOK, result)
}

func handleReferrals(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)

	credential, account, errAccount := resolveQueryCredential(req, payload)
	if errAccount != nil {
		return jsonResponse(statusForError(errAccount), map[string]any{"error": errAccount.Error()})
	}

	cfg := normalizeConfig(mergeConfig(currentConfig(), pluginConfig{
		BaseURL:    payload.BaseURL,
		Language:   payload.Language,
		Originator: payload.Originator,
		UserAgent:  payload.UserAgent,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, errRefs := fetchReferralCapacity(ctx, cfg, credential, account, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL))
	if errRefs != nil {
		return jsonResponse(statusForError(errRefs), map[string]any{"error": errRefs.Error()})
	}
	return jsonResponse(http.StatusOK, result)
}

// handleProbe is a diagnostic endpoint that probes a caller-supplied list of
// ChatGPT backend-api paths with the selected credential's uTLS transport,
// returning status + a body preview for each. It exists to reverse-engineer
// which (if any) endpoint exposes referral invite counts / credit rewards.
func handleProbe(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)

	var endpoints []string
	if len(req.Body) > 0 {
		var raw struct {
			Endpoints []string `json:"endpoints"`
		}
		_ = json.Unmarshal(req.Body, &raw)
		endpoints = raw.Endpoints
	}
	if len(endpoints) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "endpoints array is required"})
	}

	account, errAccount := selectQueryAccount(req, payload)
	if errAccount != nil {
		return jsonResponse(statusForError(errAccount), map[string]any{"error": errAccount.Error()})
	}

	credential, errCredential := fetchCodexCredential(req, payload.ManagementOrigin, account)
	if errCredential != nil {
		return jsonResponse(statusForError(errCredential), map[string]any{"error": errCredential.Error()})
	}
	if credential.AccountID == "" {
		credential.AccountID = account.ChatGPTAccountID
	}

	cfg := normalizeConfig(mergeConfig(currentConfig(), pluginConfig{
		BaseURL:    payload.BaseURL,
		Language:   payload.Language,
		Originator: payload.Originator,
		UserAgent:  payload.UserAgent,
	}))

	type probeResult struct {
		Endpoint  string `json:"endpoint"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id,omitempty"`
		Preview   string `json:"preview,omitempty"`
		OK        bool   `json:"ok"`
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	results := make([]probeResult, 0, len(endpoints))
	for _, ep := range endpoints {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		// Only allow backend-api paths under chatgpt.com to avoid an open redirect.
		if !strings.HasPrefix(ep, "/backend-api/") {
			results = append(results, probeResult{Endpoint: ep, Status: 0, Preview: "skipped: only /backend-api/* paths allowed"})
			continue
		}
		// Per-request timeout so one slow endpoint does not exhaust the budget for others.
		probeCtx, probeCancel := context.WithTimeout(ctx, 12*time.Second)
		var status int
		var requestID string
		var raw []byte
		var errGet error
		if idx := strings.IndexByte(ep, '?'); idx >= 0 {
			// Path carries a query string: resolve full URL so query params survive.
			fullURL, errURL := codexEndpoint(cfg.BaseURL, ep[:idx])
			if errURL != nil {
				results = append(results, probeResult{Endpoint: ep, Status: 0, Preview: "error: " + errURL.Error()})
				probeCancel()
				continue
			}
			fullURL = fullURL + ep[idx:]
			// Eligibility probes get the preview header automatically (matches desktop app behaviour).
			var previewHdrs http.Header
			if strings.Contains(ep, "invite/eligibility") {
				previewHdrs = http.Header{"OpenAI-Internal-Referral-Eligibility-Preview": []string{"true"}}
			}
			status, requestID, raw, errGet = codexGetURLWithHeaders(probeCtx, cfg, credential, fullURL, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL), previewHdrs)
		} else {
			status, requestID, raw, errGet = codexGet(probeCtx, cfg, credential, ep, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL))
		}
		probeCancel()
		if errGet != nil {
			results = append(results, probeResult{Endpoint: ep, Status: status, Preview: "error: " + errGet.Error()})
			continue
		}
		preview := string(raw)
		if len(preview) > 2000 {
			preview = preview[:2000] + "...(truncated)"
		}
		results = append(results, probeResult{
			Endpoint:  ep,
			Status:    status,
			RequestID: requestID,
			Preview:   preview,
			OK:        status >= 200 && status < 300,
		})
	}

	return jsonResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"account": account,
		"results": results,
	})
}

// handleRedeem lists banked rate-limit reset credits and, if any are available, redeems one
// via POST /backend-api/wham/rate-limit-reset-redits/consume. This is the action that turns a
// stored referral reward into actual rate-limit window resets.
func handleRedeem(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)

	credential, account, errAccount := resolveQueryCredential(req, payload)
	if errAccount != nil {
		return jsonResponse(statusForError(errAccount), map[string]any{"error": errAccount.Error()})
	}

	cfg := normalizeConfig(mergeConfig(currentConfig(), pluginConfig{
		BaseURL:    payload.BaseURL,
		Language:   payload.Language,
		Originator: payload.Originator,
		UserAgent:  payload.UserAgent,
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. List banked credits to find an available one.
	listStatus, _, listRaw, errList := codexGet(ctx, cfg, credential, resetCreditsEndpointPath, strings.TrimSpace(payload.Cookie), strings.TrimSpace(payload.ProxyURL))
	if errList != nil {
		return jsonResponse(statusForError(errList), map[string]any{"error": errList.Error()})
	}
	if listStatus != http.StatusOK {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("failed to list reset credits: status %d", listStatus)})
	}

	var creditsPayload struct {
		Credits        []map[string]any `json:"credits"`
		AvailableCount int              `json:"available_count"`
	}
	if errParse := json.Unmarshal(listRaw, &creditsPayload); errParse != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "failed to parse reset credits response"})
	}

	// Find the first available credit.
	var creditID string
	for _, c := range creditsPayload.Credits {
		status, _ := c["status"].(string)
		if strings.EqualFold(status, "available") {
			if id, ok := c["id"].(string); ok && id != "" {
				creditID = id
				break
			}
		}
	}
	if creditID == "" {
		return jsonResponse(http.StatusOK, map[string]any{
			"ok":              false,
			"redeemed":        false,
			"account":         account,
			"available_count": creditsPayload.AvailableCount,
			"message":         "没有可用的重置额度（available credits = 0）。需要先通过邀请获得奖励后才能兑换。",
		})
	}

	// 2. Consume the credit.
	redeemReqID := newUUIDv4()
	consumeEndpoint, errEndpoint := codexEndpoint(cfg.BaseURL, consumeCreditsEndpointPath)
	if errEndpoint != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": errEndpoint.Error()})
	}
	consumeBody, _ := json.Marshal(map[string]any{
		"credit_id":         creditID,
		"redeem_request_id": redeemReqID,
	})

	req2, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, consumeEndpoint, bytes.NewReader(consumeBody))
	if errRequest != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": errRequest.Error()})
	}
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Oai-Language", cfg.Language)
	req2.Header.Set("Originator", cfg.Originator)
	req2.Header.Set("User-Agent", cfg.UserAgent)
	if credential.AccountID != "" {
		req2.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	if cookie := strings.TrimSpace(payload.Cookie); cookie != "" {
		req2.Header.Set("Cookie", cookie)
	} else if cfg.Cookie != "" {
		req2.Header.Set("Cookie", cfg.Cookie)
	}

	client, errClient := inviteHTTPClient(strings.TrimSpace(payload.ProxyURL))
	if errClient != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": errClient.Error()})
	}
	resp, errDo := client.Do(req2)
	if errDo != nil {
		return jsonResponse(statusForError(errDo), map[string]any{"error": errDo.Error()})
	}
	defer func() { _ = resp.Body.Close() }()
	consumeRaw, errRead := readLimited(resp.Body, upstreamBodyLimit)
	if errRead != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": errRead.Error()})
	}

	result := map[string]any{
		"ok":               resp.StatusCode >= 200 && resp.StatusCode < 300,
		"redeemed":         resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status_code":      resp.StatusCode,
		"request_id":       resp.Header.Get("x-oai-request-id"),
		"account":          account,
		"credit_id":        creditID,
		"redeem_request_id": redeemReqID,
	}
	var upstream any
	if len(consumeRaw) > 0 && json.Unmarshal(consumeRaw, &upstream) == nil {
		result["upstream"] = upstream
	} else if len(consumeRaw) > 0 {
		result["upstream_raw"] = string(consumeRaw)
	}
	return jsonResponse(http.StatusOK, result)
}

// newUUIDv4 generates a random RFC 4122 version 4 UUID without external dependencies.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type httpStatusError struct {
	status int
	msg    string
}

func (e httpStatusError) Error() string { return e.msg }

func statusForError(err error) int {
	var statusErr httpStatusError
	if err != nil && errors.As(err, &statusErr) && statusErr.status > 0 {
		return statusErr.status
	}
	return http.StatusBadGateway
}

func collectEmails(req inviteRequest, maxEmails int) ([]string, error) {
	if maxEmails <= 0 {
		maxEmails = defaultMaxEmails
	}
	if maxEmails > upperMaxEmails {
		maxEmails = upperMaxEmails
	}

	seen := map[string]struct{}{}
	out := make([]string, 0)
	add := func(raw string) {
		for _, item := range splitEmailList(raw) {
			email := strings.TrimSpace(item)
			if email == "" {
				continue
			}
			key := strings.ToLower(email)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, email)
		}
	}
	for _, item := range req.Emails {
		add(item)
	}
	add(req.EmailsText)

	if len(out) == 0 {
		return nil, fmt.Errorf("at least one email is required")
	}
	if len(out) > maxEmails {
		return nil, fmt.Errorf("too many emails: got %d, max %d", len(out), maxEmails)
	}
	for _, email := range out {
		if !emailPattern.MatchString(email) {
			return nil, fmt.Errorf("invalid email address %q", email)
		}
	}
	return out, nil
}

// handleActivate 调用 _full_v5.py 完成全链路激活（登录→OTP→邀请→OAuth→手机验证→token→codex消息→CPA导出）
// 接收 email + card 参数，返回 JSON 结果。长期运行（OTP等待+手机验证可能数分钟）。
func handleActivate(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)
	email := strings.TrimSpace(payload.Email)
	card := strings.TrimSpace(payload.Card)

	if email == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "email required"})
	}
	// card 可为空（DRY RUN）或非空（激活）

	// 定位 _full_v5.py
	scriptPath := ""
	for _, candidate := range []string{
		`G:\Github\playground\codex-auto-invite\_full_v5.py`,
		`/opt/codex-auto-invite/_full_v5.py`,
		`./codex-auto-invite/_full_v5.py`,
	} {
		if _, err := os.Stat(candidate); err == nil {
			scriptPath = candidate
			break
		}
	}
	if scriptPath == "" {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error": "_full_v5.py not found in any known location",
		})
	}

	// 构建命令行参数
	args := []string{scriptPath, "--email", email, "--json"}
	if card != "" {
		args = append(args, "--card", card)
	} else {
		args = append(args, "--dry-run")
	}
	// mail-auth 从环境变量或固定值
	mailAuth := os.Getenv("MAIL_AUTH")
	if mailAuth == "" {
		mailAuth = "lhdmm1230" // 默认值（Mox bridge）
	}
	args = append(args, "--mail-auth", mailAuth)

	// 调用 Python 脚本（最长 10 分钟超时）
	cmd := exec.Command("python", args...)
	// Windows 下 python 可能需要完整路径
	if _, err := exec.LookPath("python"); err != nil {
		// 尝试常见路径
		for _, py := range []string{
			`C:\Python313\python.exe`,
			`C:\Users\10126\AppData\Local\Programs\Python\Python311\python.exe`,
		} {
			if _, err := os.Stat(py); err == nil {
				cmd = exec.Command(py, args...)
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// 脚本出错——检查是否有 JSON 输出
		output := stdout.String()
		var result map[string]any
		if err := json.Unmarshal([]byte(output), &result); err == nil {
			result["exit_error"] = err.Error()
			return jsonResponse(http.StatusOK, result)
		}
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error":   err.Error(),
			"stdout":  stdout.String()[:2000],
			"stderr":  stderr.String()[:500],
			"timeout": ctx.Err() != nil,
		})
	}

	// 解析 JSON 输出
	output := stdout.String()
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error": "failed to parse script output: " + err.Error(),
			"stdout": output[:2000],
		})
	}
	return jsonResponse(http.StatusOK, result)
}

// handleCDPActivate 通过 CDP 在 ChatGPT Desktop 里注入 cookie 并发消息（触发 codex_turn）
// 调用 Python 脚本完成：CloakBrowser登录导出cookie → CDP注入 → 发消息
func handleCDPActivate(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)
	email := strings.TrimSpace(payload.Email)
	if email == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "email required"})
	}

	// 定位 CDP 脚本
	scriptPath := ""
	for _, candidate := range []string{
		`G:\Github\playground\codex-auto-invite\_cdp_auto.py`,
		`/opt/codex-auto-invite/_cdp_auto.py`,
	} {
		if _, err := os.Stat(candidate); err == nil {
			scriptPath = candidate
			break
		}
	}
	if scriptPath == "" {
		return jsonResponse(http.StatusInternalServerError, map[string]any{
			"error": "_cdp_auto.py not found",
		})
	}

	args := []string{scriptPath, "--host", "127.0.0.1", "--email", email, "--msg", "Hello"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pyExe := "python"
	for _, py := range []string{`C:\Python313\python.exe`, `C:\Users\10126\AppData\Local\Programs\Python\Python311\python.exe`} {
		if _, err := os.Stat(py); err == nil {
			pyExe = py
			break
		}
	}

	cmd := exec.CommandContext(ctx, pyExe, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"success":  false,
			"error":    err.Error(),
			"stdout":   stdout.String()[:min(2000, stdout.Len())],
			"timeout":  ctx.Err() != nil,
		})
	}

	output := stdout.String()
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return jsonResponse(http.StatusOK, map[string]any{
			"success": true,
			"stdout":  output[:min(2000, len(output))],
		})
	}
	return jsonResponse(http.StatusOK, result)
}

// handleDesktopActivate 通过 accept-referral 先行 → OTP → Codex OAuth → auth.json → Codex app 发消息。
// 正确顺序（2026-08-12 确认）：先点击邀请链接，再登录，然后在 Codex 发消息。
// 流程：_full_v6.py（accept-referral→OTP→OAuth→auth.json）→ _codex_send_msg.py（Codex app 发消息）
// 前提：hylouis2 上 Codex app（ChatGPT.exe --remote-debugging-port=9222）已启动
func handleDesktopActivate(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	payload := parseQueryRequest(req.Body)
	email := strings.TrimSpace(payload.Email)
	card := strings.TrimSpace(payload.Card)
	if email == "" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "email required"})
	}

	// 定位脚本（本机路径）
	fullScript := "G:\\Github\\playground\\codex-auto-invite\\_full_v6.py"
	sendScript := "G:\\Github\\playground\\codex-auto-invite\\_codex_send_msg.py"
	for _, s := range []string{fullScript, sendScript} {
		if _, err := os.Stat(s); err != nil {
			return jsonResponse(http.StatusInternalServerError, map[string]any{
				"error": "script not found: " + s,
			})
		}
	}

	pyExe := "python"
	for _, py := range []string{`C:\Python313\python.exe`, `C:\Users\10126\AppData\Local\Programs\Python\Python311\python.exe`} {
		if _, err := os.Stat(py); err == nil {
			pyExe = py
			break
		}
	}

	// Step 1: _full_v6.py（accept-referral → OTP → OAuth → auth.json）
	isDryRun := card == ""
	step1Args := []string{fullScript, "--email", email, "--json"}
	if isDryRun {
		step1Args = append(step1Args, "--dry-run")
	} else {
		step1Args = append(step1Args, "--card", card)
	}

	result := map[string]any{"email": email, "dry_run": isDryRun, "steps": map[string]any{}}

	// Run _full_v6.py (up to 10 min for OTP wait)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Minute)
	cmd1 := exec.CommandContext(ctx1, pyExe, step1Args...)
	var stdout1, stderr1 bytes.Buffer
	cmd1.Stdout = &stdout1
	cmd1.Stderr = &stderr1
	err1 := cmd1.Run()
	cancel1()
	out1 := stdout1.String()
	result["steps"].(map[string]any)["full_v6"] = map[string]any{
		"stdout":  out1[:min(3000, len(out1))],
		"success": err1 == nil,
	}
	if err1 != nil {
		se := stderr1.String()
		if len(se) > 0 {
			result["steps"].(map[string]any)["full_v6"].(map[string]any)["stderr"] = se[:min(500, len(se))]
		}
		result["success"] = false
		result["error"] = "failed at step: full_v6 (accept-referral + OAuth)"
		return jsonResponse(http.StatusOK, result)
	}

	// Parse full_v6 JSON output (JSON is on the last line of stdout)
	var v6Result map[string]any
	// 提取最后一行 JSON（_full_v6.py 先输出日志，最后一行是 JSON）
	out1Trimmed := strings.TrimSpace(out1)
	out1Lines := strings.Split(out1Trimmed, "\n")
	jsonLine := out1Lines[len(out1Lines)-1]
	if json.Unmarshal([]byte(jsonLine), &v6Result) != nil {
		// fallback: 从 stdout 全文搜索包含 "success" 的 JSON 行
		for _, line := range out1Lines {
			if strings.Contains(line, `"success"`) && strings.HasPrefix(strings.TrimSpace(line), "{") {
				if json.Unmarshal([]byte(strings.TrimSpace(line)), &v6Result) == nil {
					break
				}
			}
		}
	}
	if v6Result != nil {
		if errMsg, ok := v6Result["error"]; ok {
			result["success"] = false
			result["error"] = errMsg
			return jsonResponse(http.StatusOK, result)
		}
		if aid, ok := v6Result["account_id"]; ok {
			result["account_id"] = aid
		}
		if ta, ok := v6Result["tracking_after_accept"]; ok {
			result["tracking_after_accept"] = ta
		}
		if oauth, ok := v6Result["oauth_status"]; ok {
			result["oauth_status"] = oauth
		}
		if success, ok := v6Result["success"]; ok {
			result["v6_success"] = success
		}
	}

	// DRY RUN stops here (no message sending)
	if isDryRun {
		result["success"] = true
		result["message"] = "DRY RUN complete. Restart Codex app and run _codex_send_msg.py to send message."
		return jsonResponse(http.StatusOK, result)
	}

	// Step 2: _codex_send_msg.py (send message in Codex app)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
	cmd2 := exec.CommandContext(ctx2, pyExe, sendScript, "--msg", "Say hello")
	var stdout2, stderr2 bytes.Buffer
	cmd2.Stdout = &stdout2
	cmd2.Stderr = &stderr2
	err2 := cmd2.Run()
	cancel2()
	out2 := stdout2.String()
	result["steps"].(map[string]any)["send_msg"] = map[string]any{
		"stdout":  out2[:min(2000, len(out2))],
		"success": err2 == nil,
	}
	if err2 != nil {
		result["send_msg_error"] = err2.Error()
	}

	result["success"] = true
	return jsonResponse(http.StatusOK, result)
}

func splitEmailList(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
}

func selectAccount(accounts []accountInfo, req inviteRequest) (accountInfo, error) {
	authIndex := strings.TrimSpace(req.AuthIndex)
	authName := strings.TrimSpace(req.AuthName)
	if authIndex == "" && authName == "" {
		return accountInfo{}, fmt.Errorf("auth_index or auth_name is required")
	}
	for _, account := range accounts {
		if authIndex != "" && strings.EqualFold(account.AuthIndex, authIndex) {
			return account, nil
		}
		if authName != "" && account.Name == authName {
			return account, nil
		}
	}
	return accountInfo{}, fmt.Errorf("selected Codex credential was not found")
}

func fetchCodexAccounts(req pluginapi.ManagementRequest, explicitOrigin string) ([]accountInfo, error) {
	origin, errOrigin := resolveManagementOrigin(req, explicitOrigin)
	if errOrigin != nil {
		return nil, errOrigin
	}
	authHeader := strings.TrimSpace(req.Headers.Get("Authorization"))
	if authHeader == "" {
		return nil, httpStatusError{status: http.StatusUnauthorized, msg: "CPA management key is required"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, raw, errFetch := callLocalManagement(ctx, origin, http.MethodGet, authFilesPath, authHeader, nil)
	if errFetch != nil {
		return nil, errFetch
	}
	if status != http.StatusOK {
		return nil, httpStatusError{status: http.StatusBadGateway, msg: fmt.Sprintf("failed to list CPA auth files: status %d", status)}
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		return nil, fmt.Errorf("decode auth files response: %w", errDecode)
	}

	accounts := make([]accountInfo, 0)
	for _, file := range payload.Files {
		provider := firstString(file, "provider", "type")
		if !strings.EqualFold(provider, "codex") {
			continue
		}
		if boolValue(file["disabled"]) || boolValue(file["unavailable"]) {
			continue
		}
		name := firstString(file, "name")
		if name == "" || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		accounts = append(accounts, accountInfo{
			AuthIndex:        firstString(file, "auth_index", "auth-index"),
			Name:             name,
			Label:            firstString(file, "label"),
			Email:            firstString(file, "email"),
			Account:          firstString(file, "account"),
			ChatGPTAccountID: nestedString(file, "id_token", "chatgpt_account_id"),
			Status:           firstString(file, "status"),
			Source:           firstString(file, "source"),
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		left := strings.ToLower(accounts[i].Email + accounts[i].Name)
		right := strings.ToLower(accounts[j].Email + accounts[j].Name)
		return left < right
	})
	return accounts, nil
}

func fetchCodexCredential(req pluginapi.ManagementRequest, explicitOrigin string, account accountInfo) (codexCredential, error) {
	origin, errOrigin := resolveManagementOrigin(req, explicitOrigin)
	if errOrigin != nil {
		return codexCredential{}, errOrigin
	}
	authHeader := strings.TrimSpace(req.Headers.Get("Authorization"))
	if authHeader == "" {
		return codexCredential{}, httpStatusError{status: http.StatusUnauthorized, msg: "CPA management key is required"}
	}

	path := authFileDownloadPath + "?name=" + url.QueryEscape(account.Name)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, raw, errFetch := callLocalManagement(ctx, origin, http.MethodGet, path, authHeader, nil)
	if errFetch != nil {
		return codexCredential{}, errFetch
	}
	if status != http.StatusOK {
		return codexCredential{}, httpStatusError{status: http.StatusBadGateway, msg: fmt.Sprintf("failed to download selected Codex credential: status %d", status)}
	}

	credential, errCredential := parseCodexCredential(raw)
	if errCredential != nil {
		return codexCredential{}, errCredential
	}
	if credential.AccessToken == "" {
		return codexCredential{}, httpStatusError{status: http.StatusBadRequest, msg: "selected Codex credential does not contain access_token"}
	}
	return credential, nil
}

func resolveManagementOrigin(req pluginapi.ManagementRequest, explicit string) (string, error) {
	for _, candidate := range []string{
		explicit,
		req.Headers.Get(requestManagementOrigin),
		req.Headers.Get("Origin"),
	} {
		origin, errOrigin := normalizeOrigin(candidate)
		if errOrigin == nil && origin != "" {
			return origin, nil
		}
	}
	return "", httpStatusError{status: http.StatusBadRequest, msg: "management origin is required"}
}

func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", errParse
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported origin scheme")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("origin host is required")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func callLocalManagement(ctx context.Context, origin, method, path, authorization string, body []byte) (int, []byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if errRequest != nil {
		return 0, nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", authorization)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := http.DefaultClient.Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := readLimited(resp.Body, upstreamBodyLimit)
	if errRead != nil {
		return resp.StatusCode, nil, errRead
	}
	return resp.StatusCode, raw, nil
}

func parseCodexCredential(raw []byte) (codexCredential, error) {
	var data map[string]any
	if errUnmarshal := json.Unmarshal(raw, &data); errUnmarshal != nil {
		return codexCredential{}, fmt.Errorf("decode Codex credential: %w", errUnmarshal)
	}
	return codexCredential{
		AccessToken: firstNestedString(data,
			[]string{"access_token"},
			[]string{"token_data", "access_token"},
		),
		AccountID: firstNestedString(data,
			[]string{"account_id"},
			[]string{"chatgpt_account_id"},
			[]string{"token_data", "account_id"},
			[]string{"token_data", "chatgpt_account_id"},
		),
		Email: firstNestedString(data,
			[]string{"email"},
			[]string{"token_data", "email"},
		),
	}, nil
}

func sendInvite(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, emails []string, referralKey string, requestCookie string, proxyURL string) (inviteResponse, error) {
	// Try the new referral invite endpoint first (program_id/entrypoint body shape used by
	// the Codex desktop app). Fall back to the legacy wham endpoint if the new one is not
	// available for this account.
	result, errV2 := sendInviteV2(ctx, cfg, credential, account, emails, requestCookie, proxyURL)
	if errV2 == nil && result.OK {
		return result, nil
	}
	// Legacy fallback.
	return sendInviteLegacy(ctx, cfg, credential, account, emails, referralKey, requestCookie, proxyURL)
}

// sendInviteV2 posts to /backend-api/referrals/invite with the new body shape
// ({program_id, entrypoint, emails}) reverse-engineered from the Codex desktop app.
func sendInviteV2(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, emails []string, requestCookie string, proxyURL string) (inviteResponse, error) {
	endpoint, errEndpoint := codexEndpoint(cfg.BaseURL, inviteEndpointPathV2)
	if errEndpoint != nil {
		return inviteResponse{}, errEndpoint
	}
	body, errMarshal := json.Marshal(map[string]any{
		"program_id": programIDConsumer,
		"entrypoint": entrypointPersistent,
		"emails":     emails,
	})
	if errMarshal != nil {
		return inviteResponse{}, errMarshal
	}

	return postInvite(ctx, cfg, credential, account, endpoint, body, emails, "", requestCookie, proxyURL)
}

// sendInviteLegacy posts to the original /backend-api/wham/referrals/invite endpoint.
func sendInviteLegacy(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, emails []string, referralKey string, requestCookie string, proxyURL string) (inviteResponse, error) {
	endpoint, errEndpoint := inviteEndpoint(cfg.BaseURL)
	if errEndpoint != nil {
		return inviteResponse{}, errEndpoint
	}
	body, errMarshal := json.Marshal(map[string]any{
		"referral_key": referralKey,
		"emails":       emails,
	})
	if errMarshal != nil {
		return inviteResponse{}, errMarshal
	}

	return postInvite(ctx, cfg, credential, account, endpoint, body, emails, referralKey, requestCookie, proxyURL)
}

// postInvite is the shared POST helper for both invite endpoint versions.
func postInvite(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, endpoint string, body []byte, emails []string, referralKey string, requestCookie string, proxyURL string) (inviteResponse, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return inviteResponse{}, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Oai-Language", cfg.Language)
	req.Header.Set("Originator", cfg.Originator)
	req.Header.Set("User-Agent", cfg.UserAgent)
	if credential.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	if cookie := strings.TrimSpace(requestCookie); cookie != "" {
		req.Header.Set("Cookie", cookie)
	} else if cfg.Cookie != "" {
		req.Header.Set("Cookie", cfg.Cookie)
	}

	client, errClient := inviteHTTPClient(proxyURL)
	if errClient != nil {
		return inviteResponse{}, errClient
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return inviteResponse{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := readLimited(resp.Body, upstreamBodyLimit)
	if errRead != nil {
		return inviteResponse{}, errRead
	}

	result := inviteResponse{
		OK:          resp.StatusCode >= 200 && resp.StatusCode < 300,
		StatusCode:  resp.StatusCode,
		RequestID:   resp.Header.Get("x-oai-request-id"),
		Account:     account,
		Emails:      emails,
		ReferralKey: referralKey,
		Invites:     extractInviteLinks(raw),
	}
	var upstream any
	if len(raw) > 0 && json.Unmarshal(raw, &upstream) == nil {
		result.Upstream = upstream
	} else {
		result.UpstreamRaw = string(raw)
	}
	return result, nil
}

// chatGPTUpstreamHost is the host whose Cloudflare WAF requires a Chrome TLS
// fingerprint; requests to it are routed through the uTLS round tripper below.
const chatGPTUpstreamHost = "chatgpt.com"

// utlsRoundTripper implements http.RoundTripper using a Chrome uTLS fingerprint
// over HTTP/2, mirroring the host CPA's NewUtlsHTTPClient so the plugin's own
// requests to chatgpt.com are not blocked by Cloudflare's TLS-fingerprint WAF.
//
// It reuses a single HTTP/2 client connection per host (recreated on failure),
// the same strategy the host uses in internal/runtime/executor/helps/utls_client.go.
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
}

func newUtlsRoundTripper(dialer proxy.Dialer) *utlsRoundTripper {
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()

	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}

	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
	}

	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, errCreate := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.pending, host)
	cond.Broadcast()

	if errCreate != nil {
		return nil, errCreate
	}

	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	conn, errDial := t.dialer.Dial("tcp", addr)
	if errDial != nil {
		return nil, errDial
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		_ = conn.Close()
		return nil, errHandshake
	}

	tr := &http2.Transport{}
	h2Conn, errNew := tr.NewClientConn(tlsConn)
	if errNew != nil {
		_ = tlsConn.Close()
		return nil, errNew
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, errGet := t.getOrCreateConnection(hostname, addr)
	if errGet != nil {
		return nil, errGet
	}

	resp, errTrip := h2Conn.RoundTrip(req)
	if errTrip != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		return nil, errTrip
	}

	return resp, nil
}

// utlsRoundTripperCache reuses one Chrome round tripper per proxy URL so repeated
// queries share the HTTP/2 connection instead of re-handshaking on every call.
var utlsRoundTripperCache sync.Map

func cachedUtlsRoundTripper(dialer proxy.Dialer, cacheKey string) *utlsRoundTripper {
	if existing, ok := utlsRoundTripperCache.Load(cacheKey); ok {
		return existing.(*utlsRoundTripper)
	}
	rt := newUtlsRoundTripper(dialer)
	actual, _ := utlsRoundTripperCache.LoadOrStore(cacheKey, rt)
	return actual.(*utlsRoundTripper)
}

// chatGPTFingerprintTransport routes chatgpt.com requests through the Chrome
// uTLS round tripper and falls back to a standard proxy-aware transport for any
// other host, matching the host CPA's fallbackRoundTripper behaviour.
type chatGPTFingerprintTransport struct {
	chrome   http.RoundTripper
	fallback http.RoundTripper
}

func (f *chatGPTFingerprintTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" && strings.EqualFold(req.URL.Hostname(), chatGPTUpstreamHost) {
		return f.chrome.RoundTrip(req)
	}
	return f.fallback.RoundTrip(req)
}

// buildProxyDialer validates proxyURL and returns a proxy.Dialer for the uTLS
// path. An empty proxyURL yields proxy.Direct (direct connection).
func buildProxyDialer(proxyURL string) (proxy.Dialer, string, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return proxy.Direct, "", nil
	}
	parsed, errParse := url.Parse(proxyURL)
	if errParse != nil {
		return nil, "", fmt.Errorf("invalid proxy URL: %w", errParse)
	}
	if parsed.Scheme == "" {
		return nil, "", fmt.Errorf("proxy URL scheme is required")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, "", fmt.Errorf("unsupported proxy URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, "", fmt.Errorf("proxy URL host is required")
	}

	// For SOCKS proxies, golang.org/x/net/proxy dials the upstream directly so
	// the uTLS handshake happens over the tunnel end-to-end.
	if strings.HasPrefix(strings.ToLower(parsed.Scheme), "socks5") {
		dialer, errFrom := proxy.FromURL(parsed, proxy.Direct)
		if errFrom != nil {
			return nil, "", fmt.Errorf("build socks proxy dialer: %w", errFrom)
		}
		return dialer, proxyURL, nil
	}

	// For HTTP/HTTPS proxies, CONNECT-tunnel to 443 then run uTLS over it.
	return &httpConnectDialer{proxyURL: parsed}, proxyURL, nil
}

// httpConnectDialer reaches an HTTP(S) proxy via CONNECT, returning the raw
// tunneled TCP connection so the caller can layer uTLS on top of it.
type httpConnectDialer struct {
	proxyURL *url.URL
}

func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	return d.dialConnect(context.Background(), network, addr)
}

func (d *httpConnectDialer) dialConnect(ctx context.Context, _, addr string) (net.Conn, error) {
	proxyAddr := d.proxyURL.Host
	if !strings.Contains(proxyAddr, ":") {
		if strings.EqualFold(d.proxyURL.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}

	var dial func(network, address string) (net.Conn, error)
	dial = func(network, address string) (net.Conn, error) {
		return net.DialTimeout(network, address, 15*time.Second)
	}

	var rawConn net.Conn
	var errDial error
	rawConn, errDial = dial("tcp", proxyAddr)
	if errDial != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, errDial)
	}

	// Wrap in TLS only for https:// proxies.
	if strings.EqualFold(d.proxyURL.Scheme, "https") {
		rawConn = tls.UClient(rawConn, &tls.Config{ServerName: d.proxyURL.Hostname()}, tls.HelloChrome_Auto)
		if errHandshake := rawConn.(*tls.UConn).Handshake(); errHandshake != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("tls handshake to proxy %s: %w", proxyAddr, errHandshake)
		}
	}

	connectReq, errReq := http.NewRequest(http.MethodConnect, "https://"+addr, nil)
	if errReq != nil {
		_ = rawConn.Close()
		return nil, errReq
	}
	connectReq.Host = addr
	if d.proxyURL.User != nil {
		if username := d.proxyURL.User.Username(); username != "" {
			password, _ := d.proxyURL.User.Password()
			connectReq.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
		}
	}
	if errWrite := connectReq.Write(rawConn); errWrite != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("write CONNECT to proxy %s: %w", proxyAddr, errWrite)
	}

	br := bufio.NewReader(rawConn)
	resp, errRead := http.ReadResponse(br, connectReq)
	if errRead != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("read CONNECT response from proxy %s: %w", proxyAddr, errRead)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = rawConn.Close()
		return nil, fmt.Errorf("proxy %s rejected CONNECT to %s: %s", proxyAddr, addr, resp.Status)
	}

	// If the CONNECT response buffered any extra bytes, hand them back via a
	// combined reader so the TLS handshake does not lose them.
	if br.Buffered() > 0 {
		return &bufferedConn{r: br, Conn: rawConn}, nil
	}
	return rawConn, nil
}

// bufferedConn prepends bytes already read from a bufio.Reader in front of a
// net.Conn so a subsequent TLS handshake sees the full stream.
type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func inviteHTTPClient(proxyURL string) (*http.Client, error) {
	dialer, cacheKey, errDialer := buildProxyDialer(proxyURL)
	if errDialer != nil {
		return nil, errDialer
	}

	chromeRT := cachedUtlsRoundTripper(dialer, cacheKey)

	var fallback http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		parsed, _ := url.Parse(proxyURL)
		if parsed != nil {
			standard := http.DefaultTransport.(*http.Transport).Clone()
			standard.Proxy = http.ProxyURL(parsed)
			fallback = standard
		}
	}

	return &http.Client{Transport: &chatGPTFingerprintTransport{chrome: chromeRT, fallback: fallback}}, nil
}

func inviteEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil {
		return "", errParse
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported ChatGPT base URL scheme")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("ChatGPT base URL host is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + inviteEndpointPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// codexEndpoint resolves an arbitrary ChatGPT backend-api path against the configured base URL.
func codexEndpoint(baseURL, endpointPath string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil {
		return "", errParse
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported ChatGPT base URL scheme")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("ChatGPT base URL host is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + endpointPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// codexGet performs an authenticated GET against a ChatGPT backend-api endpoint using the same
// header recipe proven by sendInvite (Bearer access token + Chatgpt-Account-Id + Cookie + UA).
// Returns the HTTP status, the x-oai-request-id header, and the (size-limited) raw body.
func codexGet(ctx context.Context, cfg pluginConfig, credential codexCredential, endpointPath, requestCookie, proxyURL string) (status int, requestID string, raw []byte, err error) {
	endpoint, errEndpoint := codexEndpoint(cfg.BaseURL, endpointPath)
	if errEndpoint != nil {
		return 0, "", nil, errEndpoint
	}
	return codexGetURL(ctx, cfg, credential, endpoint, requestCookie, proxyURL)
}

// codexGetURL is the full-URL variant of codexGet, used when the caller has already assembled
// a complete URL (including query string, e.g. the eligibility probe's ?referral_key=...).
func codexGetURL(ctx context.Context, cfg pluginConfig, credential codexCredential, fullURL, requestCookie, proxyURL string) (status int, requestID string, raw []byte, err error) {
	return codexGetURLWithHeaders(ctx, cfg, credential, fullURL, requestCookie, proxyURL, nil)
}

// codexGetURLWithHeaders is the full-URL variant that also sets extra upstream headers
// (e.g. the OpenAI-Internal-Referral-Eligibility-Preview flag the Codex desktop app sends
// to receive the upgraded offer amounts).
func codexGetURLWithHeaders(ctx context.Context, cfg pluginConfig, credential codexCredential, fullURL, requestCookie, proxyURL string, extraHeaders http.Header) (status int, requestID string, raw []byte, err error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if errRequest != nil {
		return 0, "", nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Oai-Language", cfg.Language)
	req.Header.Set("Originator", cfg.Originator)
	req.Header.Set("User-Agent", cfg.UserAgent)
	if credential.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	}
	if cookie := strings.TrimSpace(requestCookie); cookie != "" {
		req.Header.Set("Cookie", cookie)
	} else if cfg.Cookie != "" {
		req.Header.Set("Cookie", cfg.Cookie)
	}
	for key, values := range extraHeaders {
		for _, v := range values {
			req.Header.Set(key, v)
		}
	}

	client, errClient := inviteHTTPClient(proxyURL)
	if errClient != nil {
		return 0, "", nil, errClient
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return 0, "", nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := readLimited(resp.Body, upstreamBodyLimit)
	if errRead != nil {
		return resp.StatusCode, resp.Header.Get("x-oai-request-id"), nil, errRead
	}
	return resp.StatusCode, resp.Header.Get("x-oai-request-id"), raw, nil
}

// fetchCodexUsage calls GET /backend-api/codex/usage and projects the known fields
// (credits.balance, rate_limit windows, rate_limit_reset_credits) into a stable view,
// while still echoing the full upstream payload for debugging.
func fetchCodexUsage(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, requestCookie, proxyURL string) (usageResponse, error) {
	status, requestID, raw, errGet := codexGet(ctx, cfg, credential, usageEndpointPath, requestCookie, proxyURL)
	if errGet != nil {
		return usageResponse{}, errGet
	}
	result := usageResponse{
		OK:          status >= 200 && status < 300,
		StatusCode:  status,
		RequestID:   requestID,
		Account:     account,
	}
	if len(raw) == 0 {
		return result, nil
	}

	var data map[string]any
	if errJSON := json.Unmarshal(raw, &data); errJSON != nil {
		result.UpstreamRaw = string(raw)
		return result, nil
	}
	result.Upstream = data

	if creditsRaw, ok := data["credits"].(map[string]any); ok {
		credits := usageCredits{
			Balance:         toFloat64(creditsRaw["balance"]),
			HasSubscription: boolValue(creditsRaw["has_subscription"]),
		}
		result.Credits = &credits
	}
	if rateRaw, ok := data["rate_limit"].(map[string]any); ok {
		rl := usageRateLimit{}
		if pw := windowFromAny(rateRaw["primary_window"]); pw != nil {
			rl.PrimaryWindow = pw
		}
		if sw := windowFromAny(rateRaw["secondary_window"]); sw != nil {
			rl.SecondaryWindow = sw
		}
		if rl.PrimaryWindow != nil || rl.SecondaryWindow != nil {
			result.RateLimit = &rl
		}
	}
	if resetRaw, ok := data["rate_limit_reset_credits"].(map[string]any); ok {
		result.ResetCredits = &usageResetCredits{
			AvailableCount: toInt(resetRaw["available_count"]),
			UsedCount:      toInt(resetRaw["used_count"]),
		}
	}
	return result, nil
}

func windowFromAny(value any) *usageRateWindow {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &usageRateWindow{
		UsedPercent:       toFloat64(m["used_percent"]),
		ResetAfterSeconds: toFloat64(m["reset_after_seconds"]),
	}
}

// fetchReferralCapacity attempts to surface the remaining-invite capacity for an account.
//
// It probes endpoints reverse-engineered from the openai.chatgpt VS Code extension, in priority
// order, stopping at the first one that returns actionable invite data:
//  1. GET /backend-api/referrals/invite/eligibility?referral_key=... — canonical invite
//     eligibility (grant_amount, remaining_referrals, should_show). Requires browser Cookie
//     alongside Bearer auth; returns 403 with bearer-only auth.
//  2. GET /backend-api/wham/rate-limit-reset-credits — banked reset credits (referral-granted).
//  3. GET /backend-api/wham/referrals/status — legacy status probe.
//  4. GET /backend-api/codex/usage — fallback: parse rate_limit_reset_credits / referral fields.
//
// Every probed response is echoed under the response's dedicated fields plus upstream/upstream_raw
// for transparency.
func fetchReferralCapacity(ctx context.Context, cfg pluginConfig, credential codexCredential, account accountInfo, requestCookie, proxyURL string) (referralsResponse, error) {
	result := referralsResponse{OK: true, Account: account}

	type probeOutcome struct {
		statusCode int
		raw        []byte
		hit        bool
	}
	probe := func(endpointPath string) probeOutcome {
		status, _, raw, errGet := codexGet(ctx, cfg, credential, endpointPath, requestCookie, proxyURL)
		if errGet != nil || len(raw) == 0 {
			return probeOutcome{statusCode: status}
		}
		return probeOutcome{statusCode: status, raw: raw, hit: status >= 200 && status < 300}
	}
	probeURL := func(fullURL string) probeOutcome {
		status, _, raw, errGet := codexGetURL(ctx, cfg, credential, fullURL, requestCookie, proxyURL)
		if errGet != nil || len(raw) == 0 {
			return probeOutcome{statusCode: status}
		}
		return probeOutcome{statusCode: status, raw: raw, hit: status >= 200 && status < 300}
	}

	// 1. invite/eligibility — the canonical invite-capacity endpoint from the Codex desktop
	//    app. Parameters: program_id (consumer vs workspace) + entrypoint (persistent vs rate_limit).
	//    The desktop app sends OpenAI-Internal-Referral-Eligibility-Preview: true to receive the
	//    upgraded offer amounts (e.g. credits_500 instead of credits_250), so we send it too.
	eligBaseURL, errEligURL := codexEndpoint(cfg.BaseURL, inviteEligibilityEndpointPath)
	hasCookie := strings.TrimSpace(requestCookie) != "" || strings.TrimSpace(cfg.Cookie) != ""
	if errEligURL == nil {
		progID := programIDConsumer
		eligURL := eligBaseURL + "?program_id=" + progID + "&entrypoint=" + entrypointPersistent
		previewHeaders := http.Header{"OpenAI-Internal-Referral-Eligibility-Preview": []string{"true"}}
		eligStatus, _, eligRaw, eligErr := codexGetURLWithHeaders(ctx, cfg, credential, eligURL, requestCookie, proxyURL, previewHeaders)
		result.EligibilityStatus = eligStatus
		if eligErr == nil && eligStatus >= 200 && eligStatus < 300 && len(eligRaw) > 0 {
			result.EligibilityHit = true
			result.Eligibility = jsonRawMessage(eligRaw)
			liftEligibilityFields(&result, eligRaw)
		} else if !hasCookie && eligStatus == http.StatusForbidden {
			result.Note = "邀请资格端点（/referrals/invite/eligibility）仅用 Bearer token 时返回 403，需要浏览器 Cookie 才能访问。如需查看「每邀请奖励额度」和「剩余邀请次数」，请在 Cookie 输入框填入该账号的浏览器 Cookie 后重试。"
		}
	}

	// 1b. invite/tracking — lists sent invites (counts how many people you've already invited).
	trackBaseURL, errTrackURL := codexEndpoint(cfg.BaseURL, inviteTrackingEndpointPath)
	if errTrackURL == nil {
		trackURL := trackBaseURL + "?limit=100&period=past_90_days&program_id=" + programIDConsumer
		track := probeURL(trackURL)
		result.TrackingHit = track.hit
		if track.hit {
			result.Tracking = jsonRawMessage(track.raw)
			liftTrackingCount(&result, track.raw)
		}
	}

	// 2. rate-limit-reset-credits — banked reset credits (the reward referrals grant).
	resetCredits := probe(resetCreditsEndpointPath)
	result.ResetCreditsHit = resetCredits.hit
	if resetCredits.hit {
		result.ResetCredits = jsonRawMessage(resetCredits.raw)
	}

	// 3. referrals/status — legacy status probe.
	status := probe(referralsStatusEndpointPath)
	result.StatusStatusCode = status.statusCode
	if status.hit {
		result.StatusEndpointHit = true
		liftReferralFields(&result, status.raw, "status")
	}

	// 4. usage fallback — least specific, but always available on Pro accounts. Only run if no
	// dedicated endpoint returned invite data yet.
	if !result.EligibilityHit && !status.hit {
		usage, errUsage := fetchCodexUsage(ctx, cfg, credential, account, requestCookie, proxyURL)
		if errUsage == nil && usage.StatusCode >= 200 && usage.StatusCode < 300 {
			result.UsageEndpointUsed = true
			if result.Note == "" {
				result.Note = "referrals/status 与 referrals/credits 均未返回数据，已回退显示 Codex 用量载荷。ChatGPT 未通过专门端点暴露剩余邀请次数；可将 rate_limit_reset_credits（重置额度）视作邀请赠送的额度。"
			}
			if usage.Upstream != nil {
				if raw, errMarshal := json.Marshal(usage.Upstream); errMarshal == nil {
					liftReferralFields(&result, raw, "usage")
				}
			} else if usage.UpstreamRaw != "" {
				result.UpstreamRaw = usage.UpstreamRaw
			}
		} else if errUsage != nil {
			if result.Note == "" {
				result.Note = fmt.Sprintf("所有邀请相关端点均未响应，且用量探测失败：%s；ChatGPT 可能未对该账号暴露剩余邀请计数。", errUsage.Error())
			}
		} else if result.Note == "" {
			result.Note = fmt.Sprintf("所有邀请相关端点均未响应（eligibility=%d，status=%d）；ChatGPT 可能未对该账号暴露剩余邀请计数。", result.EligibilityStatus, status.statusCode)
		}
	}

	return result, nil
}

// jsonRawMessage safely wraps a raw JSON byte slice as a json.RawMessage, returning nil on parse failure.
func jsonRawMessage(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return raw
}

// liftEligibilityFields parses an invite/eligibility response body (from the Codex desktop
// app's /referrals/invite/eligibility endpoint) and lifts the invite-capacity fields into the
// response. Field names are reverse-engineered from app.asar.
func liftEligibilityFields(result *referralsResponse, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var data map[string]any
	if errJSON := json.Unmarshal(raw, &data); errJSON != nil {
		return
	}
	// remaining_send_capacity = how many more invites you can send (canonical "invites left").
	if v, ok := data["remaining_send_capacity"]; ok && v != nil {
		result.RemainingInvites = v
	}
	// remaining_reward_capacity = how many more rewards you can still earn.
	if v, ok := data["remaining_reward_capacity"]; ok && v != nil {
		result.MaxInvites = v
	}
}

// liftTrackingCount counts valid invite records from the /referrals/invite/tracking response
// (items array, each with an email field) to report how many invites have been sent.
func liftTrackingCount(result *referralsResponse, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var data struct {
		Items []map[string]any `json:"items"`
	}
	if errJSON := json.Unmarshal(raw, &data); errJSON != nil {
		return
	}
	count := 0
	for _, item := range data.Items {
		if _, ok := item["email"]; ok {
			count++
		}
	}
	result.TrackingCount = count
}

// liftReferralFields parses an upstream JSON body and copies any field that plausibly represents
// invite capacity into the response's remaining/max slots and the status mirror, while preserving
// the full body under upstream/upstream_raw for transparency.
func liftReferralFields(result *referralsResponse, raw []byte, source string) {
	if len(raw) == 0 {
		return
	}
	var data map[string]any
	if errJSON := json.Unmarshal(raw, &data); errJSON != nil {
		result.UpstreamRaw = string(raw)
		return
	}
	result.Upstream = data

	// Mirror the whole payload under "status" so the UI can render whatever shape the endpoint used.
	result.Status = data

	// Pick the most plausible "remaining" counter. Candidates span observed field names across
	// the referrals status, referrals credits, and usage payloads.
	for _, key := range []string{
		"remaining_invites", "remaining", "invites_remaining",
		"available_invites", "available_count", "invites_available",
		"left", "remaining_count",
	} {
		if value, present := lookupNested(data, key); present {
			result.RemainingInvites = value
			break
		}
	}
	// Same for the ceiling/max counter.
	for _, key := range []string{
		"max_invites", "max", "total_invites", "total", "cap", "limit",
		"monthly_invites", "invites_per_cycle",
	} {
		if value, present := lookupNested(data, key); present {
			result.MaxInvites = value
			break
		}
	}
}

// lookupNested resolves a key at the top level OR one level deep under common parent objects
// (referrals, status, credits, summary), so liftReferralFields stays resilient to shape drift.
func lookupNested(data map[string]any, key string) (any, bool) {
	if data == nil {
		return nil, false
	}
	if value, ok := data[key]; ok {
		return value, true
	}
	for _, parent := range []string{"referrals", "status", "credits", "summary", "data"} {
		if child, ok := data[parent].(map[string]any); ok {
			if value, ok := child[key]; ok {
				return value, true
			}
		}
	}
	return nil, false
}

// toFloat64 coerces JSON numbers (float64 by default in Go) and numeric strings to float64.
func toFloat64(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		var f float64
		_, _ = fmt.Sscanf(strings.TrimSpace(v), "%f", &f)
		return f
	default:
		return 0
	}
}

// toInt coerces JSON numbers and numeric strings to int.
func toInt(value any) int {
	return int(toFloat64(value))
}

func extractInviteLinks(raw []byte) []inviteLink {
	var parsed struct {
		Invites []inviteLink `json:"invites"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return nil
	}
	return parsed.Invites
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	data, errRead := io.ReadAll(limited)
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body is too large")
	}
	return data, nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(data[key]); value != "" {
			return value
		}
	}
	return ""
}

func nestedString(data map[string]any, path ...string) string {
	var current any = data
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = next[key]
	}
	return stringValue(current)
}

func firstNestedString(data map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value := nestedString(data, path...); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func boolValue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func jsonResponse(status int, body any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"failed to encode response"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{contentTypeJSON}},
		Body:       raw,
	}
}

func htmlResponse(status int, body string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{contentTypeHTML}},
		Body:       []byte(body),
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func renderInvitePage(cfg pluginConfig) string {
	defaults := map[string]any{
		"referralKey": cfg.ReferralKey,
		"baseURL":     cfg.BaseURL,
		"language":    cfg.Language,
		"originator":  cfg.Originator,
		"userAgent":   cfg.UserAgent,
		"maxEmails":   cfg.MaxEmailsPerRequest,
	}
	rawDefaults, errMarshal := json.Marshal(defaults)
	if errMarshal != nil {
		rawDefaults = []byte(`{"referralKey":"codex_referral_persistent_invite","baseURL":"https://chatgpt.com","language":"zh-CN","originator":"Codex Desktop","userAgent":"","maxEmails":10}`)
	}
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Codex Invite</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: Canvas;
      color: CanvasText;
      letter-spacing: 0;
    }
    * { box-sizing: border-box; }
    body { margin: 0; background: Canvas; color: CanvasText; }
    main { max-width: 1120px; margin: 0 auto; padding: 24px; }
    header { display: flex; align-items: end; justify-content: space-between; gap: 16px; margin-bottom: 18px; }
    h1 { margin: 0; font-size: 24px; font-weight: 760; letter-spacing: 0; }
    h2 { margin: 0 0 14px; font-size: 15px; font-weight: 720; letter-spacing: 0; }
    label { display: grid; gap: 7px; font-size: 13px; font-weight: 650; min-width: 0; }
    input, select, textarea, button { font: inherit; }
    input, select, textarea {
      width: 100%;
      border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas 82%);
      border-radius: 6px;
      padding: 9px 10px;
      background: Canvas;
      color: CanvasText;
    }
    textarea { min-height: 116px; resize: vertical; line-height: 1.45; }
    button {
      border: 0;
      border-radius: 6px;
      padding: 9px 12px;
      background: #0f766e;
      color: #fff;
      font-weight: 720;
      cursor: pointer;
      white-space: nowrap;
    }
    button.secondary { background: color-mix(in srgb, CanvasText 10%, Canvas 90%); color: CanvasText; }
    button.warning { background: #b45309; }
    button:disabled { opacity: .54; cursor: not-allowed; }
    .header-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: end; gap: 10px; }
    .locale-control { display: flex; align-items: center; gap: 8px; min-width: auto; font-size: 12px; font-weight: 650; }
    .locale-control select { width: auto; min-width: 120px; padding: 7px 9px; }
    .layout { display: grid; grid-template-columns: 340px minmax(0, 1fr); gap: 16px; align-items: start; }
    .stack { display: grid; gap: 16px; }
    .panel {
      border: 1px solid color-mix(in srgb, CanvasText 14%, Canvas 86%);
      border-radius: 8px;
      padding: 16px;
      background: color-mix(in srgb, Canvas 96%, CanvasText 4%);
    }
    .collapsible { padding: 0; overflow: hidden; }
    .collapsible > summary {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 16px;
      cursor: pointer;
      list-style: none;
    }
    .collapsible > summary::-webkit-details-marker { display: none; }
    .collapsible > summary::after {
      content: "+";
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 22px;
      height: 22px;
      border-radius: 6px;
      background: color-mix(in srgb, CanvasText 10%, Canvas 90%);
      font-size: 15px;
      font-weight: 760;
      flex: 0 0 auto;
    }
    .collapsible[open] > summary {
      border-bottom: 1px solid color-mix(in srgb, CanvasText 12%, Canvas 88%);
    }
    .collapsible[open] > summary::after { content: "-"; }
    .collapsible-body { padding: 16px; }
    .summary-text { display: grid; gap: 3px; min-width: 0; }
    .summary-title { font-size: 15px; font-weight: 720; letter-spacing: 0; }
    .summary-subtitle {
      color: color-mix(in srgb, CanvasText 62%, Canvas 38%);
      font-size: 12px;
      font-weight: 520;
      line-height: 1.35;
    }
    .fields { display: grid; gap: 13px; }
    .grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 13px; }
    .actions { display: flex; flex-wrap: wrap; gap: 9px; align-items: center; }
    .actions button { width: auto; }
    .inline { display: flex; gap: 9px; align-items: center; }
    .inline input[type="checkbox"] { width: auto; margin: 0; }
    .metric {
      min-height: 34px;
      display: inline-flex;
      align-items: center;
      border-radius: 6px;
      padding: 6px 9px;
      font-size: 12px;
      font-weight: 700;
      background: color-mix(in srgb, #2563eb 12%, Canvas 88%);
      color: color-mix(in srgb, #2563eb 72%, CanvasText 28%);
    }
    .muted { color: color-mix(in srgb, CanvasText 62%, Canvas 38%); font-size: 12px; font-weight: 520; }
    .status {
      margin-top: 16px;
      white-space: pre-wrap;
      word-break: break-word;
      border-radius: 8px;
      padding: 13px;
      background: color-mix(in srgb, #2563eb 10%, Canvas 90%);
      border: 1px solid color-mix(in srgb, #2563eb 18%, Canvas 82%);
      font-size: 13px;
      line-height: 1.45;
    }
    .status.error {
      background: color-mix(in srgb, #dc2626 12%, Canvas 88%);
      border-color: color-mix(in srgb, #dc2626 24%, Canvas 76%);
    }
    .links { display: grid; gap: 8px; margin-top: 12px; }
    .links a {
      color: #0f766e;
      overflow-wrap: anywhere;
      border: 1px solid color-mix(in srgb, CanvasText 12%, Canvas 88%);
      border-radius: 6px;
      padding: 9px 10px;
      background: Canvas;
      text-decoration: none;
    }
    @media (max-width: 860px) {
      main { padding: 16px; }
      header { display: grid; align-items: start; }
      .header-actions { justify-content: start; }
      .layout, .grid { grid-template-columns: 1fr; }
      .actions, .inline { display: grid; }
      .actions button { width: 100%; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1 data-i18n="app.title">Codex Invite</h1>
      <div class="header-actions">
        <label class="locale-control">
          <span data-i18n="app.language">Language</span>
          <select id="localeSelect" autocomplete="off">
            <option value="en">English</option>
            <option value="zh-CN">中文</option>
          </select>
        </label>
        <span class="metric" id="emailCount">0 emails</span>
      </div>
    </header>
    <div class="layout">
      <div class="stack">
        <section class="panel">
          <h2 data-i18n="connection.title">Connection</h2>
          <div class="fields">
            <label><span data-i18n="connection.managementKey">CPA management key</span>
              <input id="managementKey" type="password" autocomplete="off" spellcheck="false">
            </label>
            <div class="actions">
              <button id="loadAccounts" type="button" data-i18n="connection.loadAccounts">Load accounts</button>
            </div>
            <label><span data-i18n="connection.account">Codex account</span>
              <select id="account"></select>
            </label>
            <span id="accountCount" class="muted"></span>
            <label><span data-i18n="connection.credentialSource">Credential source</span>
              <select id="credMode" autocomplete="off">
                <option value="cpa" data-i18n="connection.sourceCpa">CPA-managed</option>
                <option value="manual" data-i18n="connection.sourceManual">Manual entry</option>
              </select>
            </label>
            <div id="manualCredFields" hidden>
              <label><span data-i18n="connection.accessToken">access_token</span>
                <input id="manualToken" type="password" autocomplete="off" spellcheck="false">
              </label>
              <label><span data-i18n="connection.accountId">account_id</span>
                <input id="manualAccountId" spellcheck="false">
              </label>
              <label><span data-i18n="connection.manualEmail">Email (optional)</span>
                <input id="manualEmail" spellcheck="false">
              </label>
            </div>
          </div>
        </section>
        <details class="panel collapsible" id="settingsPanel">
          <summary>
            <span class="summary-text">
              <span class="summary-title" data-i18n="settings.title">Settings</span>
              <span class="summary-subtitle" data-i18n="settings.summary">Defaults work for most cases</span>
            </span>
          </summary>
          <div class="fields collapsible-body">
            <label><span data-i18n="settings.referralKey">Referral key</span>
              <input id="referralKey" spellcheck="false">
            </label>
            <label><span data-i18n="settings.baseUrl">ChatGPT base URL</span>
              <input id="baseUrl" spellcheck="false">
            </label>
            <div class="grid">
              <label><span data-i18n="settings.upstreamLanguage">Language</span>
                <input id="language" spellcheck="false">
              </label>
              <label><span data-i18n="settings.originator">Originator</span>
                <input id="originator" spellcheck="false">
              </label>
            </div>
            <label><span data-i18n="settings.userAgent">User-Agent</span>
              <input id="userAgent" spellcheck="false">
            </label>
            <label><span data-i18n="settings.maxEmails">Max emails per request</span>
              <input id="maxEmails" type="number" min="1" max="50" step="1">
            </label>
            <label><span data-i18n="settings.cookie">Cookie</span>
              <textarea id="cookie" autocomplete="off" spellcheck="false"></textarea>
            </label>
            <div class="actions">
              <button id="saveLocal" type="button" class="secondary" data-i18n="settings.saveLocal">Save local</button>
              <button id="resetLocal" type="button" class="secondary" data-i18n="settings.resetLocal">Reset local</button>
            </div>
          </div>
        </details>
      </div>
      <section class="panel">
        <h2 data-i18n="invite.title">Invite</h2>
        <div class="fields">
          <label><span data-i18n="invite.proxyUrl">Proxy URL</span>
            <input id="proxyUrl" spellcheck="false" placeholder="http://127.0.0.1:7890">
          </label>
          <label><span data-i18n="invite.emails">Email addresses</span>
            <textarea id="emails" spellcheck="false" data-i18n-placeholder="invite.emailsPlaceholder" placeholder="name@example.com&#10;teammate@example.com"></textarea>
          </label>
          <div class="actions">
            <button id="send" type="button" data-i18n="invite.send">Send invites</button>
            <button id="clearResult" type="button" class="secondary" data-i18n="invite.clearResult">Clear result</button>
          </div>
        </div>
      </section>
    </div>
    <section id="status" class="status" hidden></section>
    <section id="links" class="links"></section>
  </main>
  <script>
    const DEFAULTS = ` + string(rawDefaults) + `;
    const STORAGE_KEY = 'codex-invite-settings-v2';
    const LOCALE_STORAGE_KEY = 'codex-invite-locale-v1';
    const TRANSLATIONS = {
      en: {
        'app.title': 'Codex Invite',
        'app.language': 'Language',
        'connection.title': 'Connection',
        'connection.managementKey': 'CPA management key',
        'connection.loadAccounts': 'Load accounts',
        'connection.account': 'Codex account',
        'connection.credentialSource': 'Credential source',
        'connection.sourceCpa': 'CPA-managed',
        'connection.sourceManual': 'Manual entry',
        'connection.accessToken': 'access_token',
        'connection.accountId': 'account_id',
        'connection.manualEmail': 'Email (optional)',
        'settings.title': 'Settings',
        'settings.summary': 'Defaults work for most cases',
        'settings.referralKey': 'Referral key',
        'settings.baseUrl': 'ChatGPT base URL',
        'settings.upstreamLanguage': 'Language',
        'settings.originator': 'Originator',
        'settings.userAgent': 'User-Agent',
        'settings.maxEmails': 'Max emails per request',
        'settings.cookie': 'Cookie',
        'settings.saveLocal': 'Save local',
        'settings.resetLocal': 'Reset local',
        'invite.title': 'Invite',
        'invite.proxyUrl': 'Proxy URL',
        'invite.emails': 'Email addresses',
        'invite.emailsPlaceholder': 'name@example.com\nteammate@example.com',
        'invite.send': 'Send invites',
        'invite.clearResult': 'Clear result',
        'email.countOne': '{count} email',
        'email.countOther': '{count} emails',
        'account.none': 'No Codex accounts loaded',
        'account.count': '{count} accounts loaded',
        'status.localLoadFailed': 'Failed to load local settings: {error}',
        'status.localSaved': 'Local settings saved.',
        'status.localReset': 'Local settings reset.',
        'status.accountsLoaded': 'Accounts loaded.',
        'error.managementKeyRequired': 'CPA management key is required',
        'error.loadAccountsFailed': 'Failed to load accounts',
        'error.selectAccount': 'Select a Codex account',
        'error.inviteFailed': 'Invite request failed',
        'error.tokenRequired': 'access_token is required in manual credential mode'
      },
      'zh-CN': {
        'app.title': 'Codex 邀请',
        'app.language': '界面语言',
        'connection.title': '连接',
        'connection.managementKey': 'CPA 管理密钥',
        'connection.loadAccounts': '加载账号',
        'connection.account': 'Codex 账号',
        'connection.credentialSource': '凭据来源',
        'connection.sourceCpa': 'CPA 管理',
        'connection.sourceManual': '手动输入',
        'connection.accessToken': 'access_token',
        'connection.accountId': 'account_id',
        'connection.manualEmail': '邮箱（可选）',
        'settings.title': '设置',
        'settings.summary': '默认值通常可以直接使用',
        'settings.referralKey': '邀请 referral key',
        'settings.baseUrl': 'ChatGPT 基础地址',
        'settings.upstreamLanguage': '上游语言',
        'settings.originator': 'Originator',
        'settings.userAgent': 'User-Agent',
        'settings.maxEmails': '单次最多邮箱数',
        'settings.cookie': 'Cookie',
        'settings.saveLocal': '保存到本地',
        'settings.resetLocal': '恢复默认',
        'invite.title': '邀请',
        'invite.proxyUrl': '代理地址',
        'invite.emails': '邮箱地址',
        'invite.emailsPlaceholder': 'name@example.com\nteammate@example.com',
        'invite.send': '发送邀请',
        'invite.clearResult': '清空结果',
        'email.countOne': '{count} 个邮箱',
        'email.countOther': '{count} 个邮箱',
        'account.none': '未加载 Codex 账号',
        'account.count': '已加载 {count} 个账号',
        'status.localLoadFailed': '加载本地设置失败：{error}',
        'status.localSaved': '本地设置已保存。',
        'status.localReset': '本地设置已恢复默认。',
        'status.accountsLoaded': '账号已加载。',
        'error.managementKeyRequired': '需要填写 CPA 管理密钥',
        'error.loadAccountsFailed': '加载账号失败',
        'error.selectAccount': '请选择 Codex 账号',
        'error.inviteFailed': '邀请请求失败',
        'error.tokenRequired': '手动凭据模式下必须填写 access_token'
      }
    };
    const origin = window.location.origin;

    function normalizeLocale(raw) {
      return String(raw || '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
    }

    function detectLocale() {
      try {
        const saved = window.localStorage.getItem(LOCALE_STORAGE_KEY);
        if (saved) return normalizeLocale(saved);
      } catch (error) {
        // Ignore storage access failures and fall back to the browser locale.
      }
      const candidates = navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language];
      for (const item of candidates) {
        if (String(item || '').toLowerCase().startsWith('zh')) return 'zh-CN';
      }
      return 'en';
    }

    const state = { accounts: [], locale: detectLocale() };

    function field(id) {
      return document.getElementById(id);
    }

    const accountSelect = field('account');
    const statusBox = field('status');
    const linksBox = field('links');
    const keyInput = field('managementKey');
    const localeSelect = field('localeSelect');
    const loadButton = field('loadAccounts');
    const saveLocalButton = field('saveLocal');
    const resetLocalButton = field('resetLocal');
    const sendButton = field('send');
    const clearResultButton = field('clearResult');
    const accountCount = field('accountCount');
    const emailCount = field('emailCount');
    const credMode = field('credMode');
    const manualCredFields = field('manualCredFields');

    function syncCredMode() {
      const manual = credMode.value === 'manual';
      manualCredFields.hidden = !manual;
      accountSelect.disabled = manual;
      loadButton.disabled = manual;
      updateEmailCount();
    }
    credMode.addEventListener('change', syncCredMode);

    function t(key, params) {
      const dictionary = TRANSLATIONS[state.locale] || TRANSLATIONS.en;
      let message = dictionary[key] || TRANSLATIONS.en[key] || key;
      for (const name of Object.keys(params || {})) {
        message = message.split('{' + name + '}').join(String(params[name]));
      }
      return message;
    }

    function emailCountText(count) {
      return t(count === 1 ? 'email.countOne' : 'email.countOther', { count });
    }

    function updateAccountCount() {
      accountCount.textContent = state.accounts.length ? t('account.count', { count: state.accounts.length }) : t('account.none');
    }

    function applyLocale() {
      document.documentElement.lang = state.locale;
      document.title = t('app.title');
      localeSelect.value = state.locale;
      for (const item of document.querySelectorAll('[data-i18n]')) {
        item.textContent = t(item.dataset.i18n);
      }
      for (const item of document.querySelectorAll('[data-i18n-placeholder]')) {
        item.placeholder = t(item.dataset.i18nPlaceholder);
      }
      updateAccountCount();
      updateEmailCount();
    }

    function changeLocale(locale) {
      state.locale = normalizeLocale(locale);
      try {
        window.localStorage.setItem(LOCALE_STORAGE_KEY, state.locale);
      } catch (error) {
        // The page remains usable if localStorage is unavailable.
      }
      applyLocale();
    }

    function setStatus(message, error) {
      statusBox.hidden = false;
      statusBox.textContent = message;
      statusBox.className = 'status' + (error ? ' error' : '');
    }

    function clearResult() {
      statusBox.hidden = true;
      statusBox.textContent = '';
      linksBox.innerHTML = '';
    }

    function formatError(data, fallback) {
      if (!data) return fallback;
      if (typeof data === 'string') return data;
      return data.message || data.error || fallback;
    }

    async function readJSON(response) {
      const text = await response.text();
      if (!text) return {};
      try {
        return JSON.parse(text);
      } catch (error) {
        return { error: text };
      }
    }

    function authHeaders() {
      const key = keyInput.value.trim();
      if (!key) throw new Error(t('error.managementKeyRequired'));
      const authorization = key.toLowerCase().startsWith('bearer ') ? key : 'Bearer ' + key;
      return {
        'Authorization': authorization,
        'X-Codex-Invite-Origin': origin
      };
    }

    function numericMaxEmails() {
      const value = Number.parseInt(field('maxEmails').value, 10);
      if (!Number.isFinite(value) || value < 1) return DEFAULTS.maxEmails || 10;
      return Math.min(value, 50);
    }

    function getSettings() {
      return {
        referral_key: field('referralKey').value.trim(),
        base_url: field('baseUrl').value.trim(),
        proxy_url: field('proxyUrl').value.trim(),
        language: field('language').value.trim(),
        originator: field('originator').value.trim(),
        user_agent: field('userAgent').value.trim(),
        max_emails_per_request: numericMaxEmails()
      };
    }

    function settingsForStorage() {
      const settings = getSettings();
      return {
        referralKey: settings.referral_key,
        baseURL: settings.base_url,
        language: settings.language,
        originator: settings.originator,
        userAgent: settings.user_agent,
        maxEmails: settings.max_emails_per_request
      };
    }

    function applySettings(raw) {
      const data = raw || {};
      field('referralKey').value = data.referral_key || data.referralKey || DEFAULTS.referralKey || '';
      field('baseUrl').value = data.base_url || data.baseURL || DEFAULTS.baseURL || 'https://chatgpt.com';
      field('proxyUrl').value = '';
      field('language').value = data.language || DEFAULTS.language || 'zh-CN';
      field('originator').value = data.originator || DEFAULTS.originator || 'Codex Desktop';
      field('userAgent').value = data.user_agent || data.userAgent || DEFAULTS.userAgent || '';
      field('maxEmails').value = data.max_emails_per_request || data.maxEmails || DEFAULTS.maxEmails || 10;
    }

    function loadLocalSettings() {
      try {
        const raw = window.localStorage.getItem(STORAGE_KEY);
        if (raw) {
          applySettings({ ...DEFAULTS, ...JSON.parse(raw) });
          return;
        }
      } catch (error) {
        setStatus(t('status.localLoadFailed', { error: error.message || String(error) }), true);
      }
      applySettings(DEFAULTS);
    }

    function saveLocalSettings() {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify(settingsForStorage()));
      setStatus(t('status.localSaved'));
    }

    function resetLocalSettings() {
      window.localStorage.removeItem(STORAGE_KEY);
      applySettings(DEFAULTS);
      setStatus(t('status.localReset'));
      updateEmailCount();
    }

    function splitEmails(text) {
      return text.split(/[,\s;]+/).map((item) => item.trim()).filter(Boolean);
    }

    function updateEmailCount() {
      const count = splitEmails(field('emails').value).length;
      emailCount.textContent = emailCountText(count);
      const manual = credMode && credMode.value === 'manual';
      const hasTarget = manual
        ? !!field('manualToken').value.trim()
        : !!accountSelect.selectedOptions.length;
      sendButton.disabled = count === 0 || !hasTarget;
    }

    function renderAccounts(accounts) {
      accountSelect.innerHTML = '';
      state.accounts = Array.isArray(accounts) ? accounts : [];
      for (const account of state.accounts) {
        const option = document.createElement('option');
        option.value = account.auth_index || account.name;
        option.dataset.name = account.name;
        option.textContent = [account.email, account.account, account.name].filter(Boolean).join(' - ') || account.name;
        accountSelect.appendChild(option);
      }
      updateAccountCount();
      updateEmailCount();
    }

    async function loadAccounts() {
      clearResult();
      loadButton.disabled = true;
      try {
        const response = await fetch('/v0/management/codex-invite/accounts', { headers: authHeaders() });
        const data = await readJSON(response);
        if (!response.ok) throw new Error(formatError(data, t('error.loadAccountsFailed')));
        renderAccounts(data.accounts || []);
        setStatus(t('status.accountsLoaded'));
      } catch (error) {
        setStatus(error.message || String(error), true);
      } finally {
        loadButton.disabled = false;
      }
    }

    async function sendInvites() {
      clearResult();
      sendButton.disabled = true;
      try {
        const settings = getSettings();
        const payload = {
          emails_text: field('emails').value,
          referral_key: settings.referral_key,
          base_url: settings.base_url,
          proxy_url: settings.proxy_url,
          language: settings.language,
          originator: settings.originator,
          user_agent: settings.user_agent,
          max_emails_per_request: settings.max_emails_per_request,
          cookie: field('cookie').value,
          management_origin: origin
        };
        const manual = field('credMode').value === 'manual';
        if (manual) {
          const token = field('manualToken').value.trim();
          if (!token) throw new Error(t('error.tokenRequired'));
          payload.access_token = token;
          payload.account_id = field('manualAccountId').value.trim();
          payload.manual_email = field('manualEmail').value.trim();
        } else {
          const selected = accountSelect.selectedOptions[0];
          if (!selected) throw new Error(t('error.selectAccount'));
          payload.auth_index = selected.value;
          payload.auth_name = selected.dataset.name || '';
        }
        const response = await fetch('/v0/management/codex-invite/invite', {
          method: 'POST',
          headers: { ...authHeaders(), 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        const data = await readJSON(response);
        if (!response.ok) throw new Error(formatError(data, t('error.inviteFailed')));
        const ok = data.ok === true;
        setStatus(JSON.stringify(data, null, 2), !ok);
        for (const invite of data.invites || []) {
          if (!invite.invite_url) continue;
          const link = document.createElement('a');
          link.href = invite.invite_url;
          link.target = '_blank';
          link.rel = 'noreferrer';
          link.textContent = (invite.email || 'invite') + ': ' + invite.invite_url;
          linksBox.appendChild(link);
        }
      } catch (error) {
        setStatus(error.message || String(error), true);
      } finally {
        updateEmailCount();
      }
    }

    localeSelect.addEventListener('change', () => changeLocale(localeSelect.value));
    loadButton.addEventListener('click', loadAccounts);
    saveLocalButton.addEventListener('click', saveLocalSettings);
    resetLocalButton.addEventListener('click', resetLocalSettings);
    sendButton.addEventListener('click', sendInvites);
    clearResultButton.addEventListener('click', clearResult);
    field('emails').addEventListener('input', updateEmailCount);
    accountSelect.addEventListener('change', updateEmailCount);
    renderAccounts([]);
    applyLocale();
    loadLocalSettings();
    updateEmailCount();
  </script>
</body>
</html>`
}

// renderUsagePage returns the management-center page that queries Codex account usage
// (credit balance, rate-limit usage, referral reset credits) and surfaces the remaining
// invite capacity for the selected credential.
func renderUsagePage(cfg pluginConfig) string {
	defaults := map[string]any{
		"baseURL":    cfg.BaseURL,
		"language":   cfg.Language,
		"originator": cfg.Originator,
		"userAgent":  cfg.UserAgent,
	}
	rawDefaults, errMarshal := json.Marshal(defaults)
	if errMarshal != nil {
		rawDefaults = []byte(`{"baseURL":"https://chatgpt.com","language":"zh-CN","originator":"Codex Desktop","userAgent":""}`)
	}
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Codex Usage</title>
  <style>
    :root {
      color-scheme: light dark;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: Canvas;
      color: CanvasText;
    }
    * { box-sizing: border-box; }
    body { margin: 0; background: Canvas; color: CanvasText; }
    main { max-width: 920px; margin: 0 auto; padding: 24px; }
    header { display: flex; align-items: end; justify-content: space-between; gap: 16px; margin-bottom: 18px; }
    h1 { margin: 0; font-size: 24px; font-weight: 760; }
    h2 { margin: 0 0 14px; font-size: 15px; font-weight: 720; }
    label { display: grid; gap: 7px; font-size: 13px; font-weight: 650; min-width: 0; }
    input, select, button { font: inherit; width: 100%; }
    input, select {
      border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas 82%);
      border-radius: 6px; padding: 9px 10px; background: Canvas; color: CanvasText;
    }
    button {
      border: 0; border-radius: 6px; padding: 9px 12px;
      background: #0f766e; color: #fff; font-weight: 720; cursor: pointer; white-space: nowrap;
    }
    button.secondary { background: color-mix(in srgb, CanvasText 10%, Canvas 90%); color: CanvasText; }
    button:disabled { opacity: .54; cursor: not-allowed; }
    .row { display: grid; grid-template-columns: 1fr; gap: 12px; margin-bottom: 16px; }
    .actions { display: flex; gap: 10px; flex-wrap: wrap; }
    .panel {
      border: 1px solid color-mix(in srgb, CanvasText 14%, Canvas 86%);
      border-radius: 8px; padding: 16px; margin-bottom: 16px;
      background: color-mix(in srgb, Canvas 96%, CanvasText 4%);
    }
    .metric { display: grid; grid-template-columns: minmax(140px, 220px) 1fr; gap: 8px 16px; align-items: baseline; }
    .metric dt { font-size: 12px; font-weight: 650; opacity: .82; }
    .metric dd { margin: 0; font-size: 14px; font-weight: 600; word-break: break-word; }
    .badge { display: inline-block; padding: 2px 8px; border-radius: 999px; font-size: 11px; font-weight: 720; }
    .badge.ok { background: color-mix(in srgb, #10b981 22%, Canvas 78%); color: CanvasText; }
    .badge.warn { background: color-mix(in srgb, #b45309 22%, Canvas 78%); color: CanvasText; }
    .badge.err { background: color-mix(in srgb, #dc2626 22%, Canvas 78%); color: CanvasText; }
    pre {
      margin: 8px 0 0; padding: 12px; border-radius: 6px; overflow: auto;
      background: color-mix(in srgb, CanvasText 6%, Canvas 94%);
      font-size: 12px; line-height: 1.5; max-height: 420px;
    }
    details > summary { cursor: pointer; font-size: 12px; font-weight: 650; margin-top: 10px; }
    .hint { font-size: 12px; opacity: .78; margin-top: 8px; line-height: 1.5; }
    .header-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: end; gap: 10px; }
    .locale-control { display: flex; align-items: center; gap: 8px; min-width: auto; font-size: 12px; font-weight: 650; }
    .locale-control select { width: auto; min-width: 120px; padding: 7px 9px; }
  </style>
</head>
<body>
  <main>
    <header>
      <h1 data-i18n="app.title">Codex Usage</h1>
      <div class="header-actions">
        <label class="locale-control">
          <span data-i18n="app.language">Language</span>
          <select id="localeSelect" autocomplete="off">
            <option value="en">English</option>
            <option value="zh-CN">中文</option>
          </select>
        </label>
      </div>
    </header>

    <section class="panel">
      <h2 data-i18n="account.title">Account</h2>
      <div class="row">
        <label>
          <span data-i18n="account.managementKey">CPA management key</span>
          <input id="managementKey" type="password" autocomplete="off" spellcheck="false">
        </label>
        <label>
          <span data-i18n="account.credential">Codex credential</span>
          <select id="account"></select>
        </label>
      </div>
      <div class="actions">
        <button id="reload" class="secondary" type="button" data-i18n="account.reload">Reload accounts</button>
        <button id="queryUsage" type="button" data-i18n="account.queryUsage">Query usage</button>
        <button id="queryReferrals" type="button" data-i18n="account.queryReferrals">Query remaining invites</button>
        <button id="redeem" type="button" class="warning" data-i18n="account.redeem">Redeem reward</button>
        <button id="clearResult" class="secondary" type="button" data-i18n="account.clear">Clear</button>
      </div>
      <div class="actions" style="margin-top:8px">
        <input id="activateEmail" type="text" placeholder="email@example.com" style="flex:1;min-width:200px">
        <input id="activateCard" type="text" placeholder="LeadBee card (empty=dry run)" style="flex:1;min-width:200px">
        <button id="activate" type="button" class="warning" data-i18n="account.activate">Activate</button>
        <button id="desktopActivate" type="button" class="warning" data-i18n="account.desktopActivate" title="CDP OAuth + auth.json injection + Codex app message (triggers codex_turn)">Desktop Activate</button>
      </div>
      <p class="hint" data-i18n="account.hint">
        Query usage: calls GET /backend-api/codex/usage to read credit balance, rate-limit usage,
        and referral-granted reset credits. Query remaining invites: probes /backend-api/wham/referrals/status then
        /credits, falling back to the usage payload. ChatGPT does not expose a single dedicated
        remaining-invite counter, so the best available number plus the raw upstream body are both shown.
      </p>
    </section>

    <section class="panel" id="resultPanel" hidden>
      <h2 id="resultTitle">Result</h2>
      <dl class="metric" id="metrics"></dl>
      <details><summary data-i18n="result.rawUpstream">Raw upstream response</summary><pre id="raw"></pre></details>
    </section>
  </main>

  <script>
    const DEFAULTS = ` + "`" + string(rawDefaults) + "`" + `;
    const settings = Object.assign({ baseURL: 'https://chatgpt.com', language: 'zh-CN', originator: 'Codex Desktop', userAgent: '' }, DEFAULTS);
    const origin = (window.location && window.location.origin) || 'http://127.0.0.1:8317';
    const MGMT_KEY_STORE = 'codex-usage-mgmt-key-v1';
    const LOCALE_STORE = 'codex-usage-locale-v1';
    const TRANSLATIONS = {
      en: {
        'app.title': 'Codex Usage',
        'app.language': 'Language',
        'account.title': 'Account',
        'account.managementKey': 'CPA management key',
        'account.credential': 'Codex credential',
        'account.reload': 'Reload accounts',
        'account.queryUsage': 'Query usage',
        'account.queryReferrals': 'Query remaining invites',
        'account.clear': 'Clear',
        'account.redeem': 'Redeem reward',
        'account.activate': 'Activate (one-click)',
        'redeem.confirm': 'Redeem one banked rate-limit reset credit for this account? This will consume the credit immediately.',
        'redeem.none': 'No available credits to redeem (available_count = 0). Earn rewards via invites first.',
        'redeem.success': 'Reward redeemed successfully! windows_reset={count}',
        'redeem.failed': 'Redeem failed',
        'account.hint': 'Query usage: calls GET /backend-api/codex/usage to read credit balance, rate-limit usage, and referral-granted reset credits. Query remaining invites: probes /backend-api/wham/referrals/status then /credits, falling back to the usage payload. ChatGPT does not expose a single dedicated remaining-invite counter, so the best available number plus the raw upstream body are both shown.',
        'result.rawUpstream': 'Raw upstream response',
        'result.default': 'Result',
        'account.placeholderKey': '(enter CPA management key above first)',
        'account.placeholderEmpty': '(no Codex credential)',
        'account.loadFailed': '(failed to load: {error})',
        'error.title': 'Error',
        'error.keyRequired': 'Management key required',
        'error.keyRequiredMsg': 'Enter the CPA management key in the box above, then click Reload accounts.',
        'metric.account': 'Account',
        'metric.httpStatus': 'HTTP status',
        'metric.creditBalance': 'Credit balance',
        'metric.subscription': 'Subscription',
        'metric.subscriptionYes': 'yes',
        'metric.subscriptionNo': 'no',
        'metric.primaryUsage': 'Primary usage',
        'metric.primaryReset': 'Primary reset',
        'metric.weeklyUsage': 'Weekly usage',
        'metric.weeklyReset': 'Weekly reset',
        'metric.resetAvail': 'Reset credits available',
        'metric.resetUsed': 'Reset credits used',
        'metric.remaining': 'Remaining invites',
        'metric.max': 'Max invites',
        'metric.source': 'Source',
        'metric.note': 'Note',
        'metric.planType': 'Plan type',
        'metric.rateLimitStatus': 'Rate-limit status',
        'metric.rateLimitExhausted': 'exhausted',
        'metric.rateLimitActive': 'active',
        'metric.rateLimitReset': 'Rate-limit resets at',
        'metric.resetCreditsAvail': 'Reset credits available',
        'metric.totalEarned': 'Total referral rewards earned',
        'metric.hasCredits': 'Has credits',
        'metric.invitesSent': 'Invites sent (90 days)',
        'metric.remainingReward': 'Remaining reward capacity',
        'metric.offerTitle': 'Offer',
        'metric.offerDesc': 'Offer details',
        'metric.referrerReward': 'Referrer reward',
        'metric.recipientReward': 'Recipient reward',
        'metric.rules': 'Rules',
        'metric.quotaUsage': 'Quota usage',
        'metric.pendingInvites': 'Pending invites',
        'metric.expiresAt': 'Expires at',
        'metric.grantDetail': 'Reward',
        'metric.resendStatus': 'Resend',
        'metric.canResend': 'Resendable now',
        'metric.resendAt': 'Resend available at',
        'metric.notResendable': 'No',
        'metric.eligible': 'Eligible',
        'metric.noReward': 'No reward',
        'metric.eligibleCount': 'Eligible accounts',
        'status.pending': 'pending',
        'status.accepted': 'accepted',
        'status.completed': 'completed',
        'status.expired': 'expired',
        'quota.send': 'Send quota',
        'quota.reward': 'Reward quota',
        'secondsSuffix': ' s',
        'usage.titleSuffix': 'Usage — {name}',
        'referrals.titleSuffix': 'Remaining invites — {name}',
        'source.usageFallback': 'usage endpoint (fallback)',
        'source.status': 'referrals/status',
        'source.credits': 'referrals/credits'
      },
      'zh-CN': {
        'app.title': 'Codex 用量',
        'app.language': '界面语言',
        'account.title': '账号',
        'account.managementKey': 'CPA 管理密钥',
        'account.credential': 'Codex 凭据',
        'account.reload': '重新加载账号',
        'account.queryUsage': '查询用量',
        'account.queryReferrals': '查询剩余邀请次数',
        'account.clear': '清空',
        'account.redeem': '兑换奖励',
        'account.activate': '激活（一键）',
        'redeem.confirm': '确定为该账号兑换一个已存储的速率限制重置额度？此操作会立即消耗该额度。',
        'redeem.none': '没有可兑换的额度（available_count = 0）。请先通过邀请获得奖励。',
        'redeem.success': '奖励兑换成功！已重置窗口数：{count}',
        'redeem.failed': '兑换失败',
        'account.hint': '查询用量：调用 GET /backend-api/codex/usage 读取额度余额、速率限制用量，以及邀请赠送的重置额度。查询剩余邀请次数：依次探测 /backend-api/wham/referrals/status 和 /credits，并回退到用量载荷。ChatGPT 没有暴露专门的剩余邀请计数器，因此会同时显示最接近的数值和原始上游响应。',
        'result.rawUpstream': '原始上游响应',
        'result.default': '结果',
        'account.placeholderKey': '（请先在上方填写 CPA 管理密钥）',
        'account.placeholderEmpty': '（无 Codex 凭据）',
        'account.loadFailed': '（加载失败：{error}）',
        'error.title': '错误',
        'error.keyRequired': '需要管理密钥',
        'error.keyRequiredMsg': '请在上方输入框填写 CPA 管理密钥，然后点击“重新加载账号”。',
        'metric.account': '账号',
        'metric.httpStatus': 'HTTP 状态',
        'metric.creditBalance': '额度余额',
        'metric.subscription': '订阅',
        'metric.subscriptionYes': '是',
        'metric.subscriptionNo': '否',
        'metric.primaryUsage': '主窗口用量',
        'metric.primaryReset': '主窗口重置',
        'metric.weeklyUsage': '周窗口用量',
        'metric.weeklyReset': '周窗口重置',
        'metric.resetAvail': '可用重置额度',
        'metric.resetUsed': '已用重置额度',
        'metric.remaining': '剩余邀请次数',
        'metric.max': '最大邀请次数',
        'metric.source': '来源',
        'metric.note': '说明',
        'metric.planType': '套餐类型',
        'metric.rateLimitStatus': '额度状态',
        'metric.rateLimitExhausted': '已耗尽',
        'metric.rateLimitActive': '可用',
        'metric.rateLimitReset': '额度重置时间',
        'metric.resetCreditsAvail': '可用重置额度',
        'metric.totalEarned': '累计邀请奖励次数',
        'metric.hasCredits': '是否有额度',
        'metric.invitesSent': '已邀请人数（90天）',
        'metric.remainingReward': '剩余可获奖励次数',
        'metric.offerTitle': '奖励活动',
        'metric.offerDesc': '活动说明',
        'metric.referrerReward': '邀请者奖励',
        'metric.recipientReward': '被邀请者奖励',
        'metric.rules': '规则',
        'metric.quotaUsage': '配额用量',
        'metric.pendingInvites': '待处理邀请',
        'metric.expiresAt': '过期时间',
        'metric.grantDetail': '奖励',
        'metric.resendStatus': '重发',
        'metric.canResend': '现在可重发',
        'metric.resendAt': '可重发时间',
        'metric.notResendable': '否',
        'metric.eligible': '有资格',
        'metric.noReward': '无奖励',
        'metric.eligibleCount': '有资格账号',
        'status.pending': '待处理',
        'status.accepted': '已接受',
        'status.completed': '已完成',
        'status.expired': '已过期',
        'quota.send': '发送配额',
        'quota.reward': '奖励配额',
        'secondsSuffix': ' 秒',
        'usage.titleSuffix': '用量 — {name}',
        'referrals.titleSuffix': '剩余邀请次数 — {name}',
        'source.usageFallback': '用量端点（回退）',
        'source.status': 'referrals/status',
        'source.credits': 'referrals/credits'
      }
    };

    function normalizeLocale(raw) {
      return String(raw || '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
    }
    function detectLocale() {
      try {
        const saved = window.localStorage.getItem(LOCALE_STORE);
        if (saved) return normalizeLocale(saved);
      } catch (error) { /* fall back to browser locale */ }
      const candidates = navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language];
      for (const item of candidates) {
        if (String(item || '').toLowerCase().startsWith('zh')) return 'zh-CN';
      }
      return 'en';
    }
    const state = { locale: detectLocale() };
    const localeSelect = document.getElementById('localeSelect');
    function t(key, params) {
      const dict = TRANSLATIONS[state.locale] || TRANSLATIONS.en;
      let msg = dict[key] || TRANSLATIONS.en[key] || key;
      for (const name of Object.keys(params || {})) {
        msg = msg.split('{' + name + '}').join(String(params[name]));
      }
      return msg;
    }
    function applyLocale() {
      document.documentElement.lang = state.locale;
      document.title = t('app.title');
      localeSelect.value = state.locale;
      for (const item of document.querySelectorAll('[data-i18n]')) {
        item.textContent = t(item.dataset.i18n);
      }
    }
    function changeLocale(locale) {
      state.locale = normalizeLocale(locale);
      try { window.localStorage.setItem(LOCALE_STORE, state.locale); } catch (error) { /* ignore */ }
      applyLocale();
    }

    const keyInput = document.getElementById('managementKey');
    function storedManagementKey() {
      try { return localStorage.getItem(MGMT_KEY_STORE) || ''; } catch { return ''; }
    }
    function persistManagementKey(value) {
      try {
        if (value) { localStorage.setItem(MGMT_KEY_STORE, value); } else { localStorage.removeItem(MGMT_KEY_STORE); }
      } catch (error) { /* ignore storage failures */ }
    }
    function authHeaders() {
      const raw = (keyInput && keyInput.value.trim()) || '';
      if (!raw) return {};
      const authorization = raw.toLowerCase().startsWith('bearer ') ? raw : 'Bearer ' + raw;
      return { Authorization: authorization, 'X-Codex-Invite-Origin': origin };
    }
    const accountSelect = document.getElementById('account');
    const reloadBtn = document.getElementById('reload');
    const usageBtn = document.getElementById('queryUsage');
    const refsBtn = document.getElementById('queryReferrals');
    const redeemBtn = document.getElementById('redeem');
    const clearBtn = document.getElementById('clearResult');
    const resultPanel = document.getElementById('resultPanel');
    const resultTitle = document.getElementById('resultTitle');
    const metrics = document.getElementById('metrics');
    const rawPre = document.getElementById('raw');

    async function readJSON(response) {
      const text = await response.text();
      try { return JSON.parse(text); } catch { return { raw: text }; }
    }
    function fmtNumber(v) {
      if (v === null || v === undefined || v === '') return '—';
      if (typeof v === 'number') return Number.isFinite(v) ? v.toLocaleString() : String(v);
      return String(v);
    }
    function fmtEpoch(epochSec) {
      const n = Number(epochSec);
      if (!Number.isFinite(n) || n <= 0) return '—';
      try { return new Date(n * 1000).toLocaleString(); } catch { return String(epochSec); }
    }
    function fmtTime(v) {
      // 兼容 epoch 秒 / epoch 毫秒 / ISO 字符串；无法识别则原样返回。
      if (v === null || v === undefined || v === '') return '—';
      if (typeof v === 'number') return fmtEpoch(v > 1e12 ? v / 1000 : v);
      const n = Number(v);
      if (Number.isFinite(n) && n > 0) return fmtEpoch(n > 1e12 ? n / 1000 : n);
      try { const d = new Date(v); return isNaN(d.getTime()) ? String(v) : d.toLocaleString(); } catch { return String(v); }
    }
    function pctBadge(pct) {
      const n = Number(pct);
      if (!Number.isFinite(n)) return fmtNumber(pct);
      const cls = n >= 90 ? 'err' : n >= 70 ? 'warn' : 'ok';
      return '<span class="badge ' + cls + '">' + n.toFixed(1) + '%</span>';
    }
    function setMetric(rows) {
      metrics.innerHTML = '';
      for (const [k, v] of rows) {
        const dt = document.createElement('dt'); dt.textContent = k;
        const dd = document.createElement('dd'); dd.innerHTML = v;
        metrics.appendChild(dt); metrics.appendChild(dd);
      }
    }
    function showResult(title, rows, rawObj) {
      resultTitle.textContent = title;
      setMetric(rows);
      try { rawPre.textContent = JSON.stringify(rawObj, null, 2); }
      catch { rawPre.textContent = String(rawObj); }
      resultPanel.hidden = false;
    }
    async function loadAccounts() {
      reloadBtn.disabled = true;
      try {
        const response = await fetch('/v0/management/codex-invite/accounts', { headers: authHeaders() });
        const data = await readJSON(response);
        if (!response.ok) throw new Error(data.error || ('HTTP ' + response.status));
        accountSelect.innerHTML = '';
        for (const acc of data.accounts || []) {
          const opt = document.createElement('option');
          opt.value = acc.auth_index || acc.name;
          opt.dataset.name = acc.name;
          opt.textContent = acc.email || acc.label || acc.name;
          accountSelect.appendChild(opt);
        }
        if (!accountSelect.options.length) accountSelect.innerHTML = '<option>' + t('account.placeholderEmpty') + '</option>';
      } catch (e) {
        accountSelect.innerHTML = '<option>' + t('account.loadFailed', { error: (e.message || e) }) + '</option>';
      } finally {
        reloadBtn.disabled = false;
      }
    }
    async function queryEndpoint(path, title) {
      const selected = accountSelect.selectedOptions[0];
      if (!selected) return;
      usageBtn.disabled = refsBtn.disabled = true;
      try {
        const payload = {
          auth_index: selected.value,
          auth_name: selected.dataset.name || '',
          base_url: settings.baseURL,
          language: settings.language,
          originator: settings.originator,
          user_agent: settings.userAgent,
          management_origin: origin
        };
        const response = await fetch(path, {
          method: 'POST',
          headers: Object.assign({}, authHeaders(), { 'Content-Type': 'application/json' }),
          body: JSON.stringify(payload)
        });
        const data = await readJSON(response);
        if (!response.ok) throw new Error(data.error || ('HTTP ' + response.status));
        if (path.indexOf('usage') !== -1 && path.indexOf('referrals') === -1) {
          renderUsage(data);
        } else {
          renderReferrals(data);
        }
      } catch (e) {
        showResult(t('error.title'), [['message', String(e.message || e)]], { error: String(e.message || e) });
      } finally {
        usageBtn.disabled = refsBtn.disabled = false;
      }
    }
    function accountName(d) {
      return (d.account && (d.account.email || d.account.name)) || 'Codex';
    }
    function renderUsage(d) {
      const rows = [];
      const c = d.credits || {};
      const rl = d.rate_limit || {};
      const rc = d.rate_limit_reset_credits || {};
      rows.push([t('metric.account'), (d.account && (d.account.email || d.account.name)) || '—']);
      rows.push([t('metric.httpStatus'), '<span class="badge ' + (d.ok ? 'ok' : 'err') + '">' + d.status_code + '</span>']);
      rows.push([t('metric.creditBalance'), fmtNumber(c.balance)]);
      rows.push([t('metric.subscription'), c.has_subscription ? t('metric.subscriptionYes') : t('metric.subscriptionNo')]);
      if (rl.primary_window) {
        rows.push([t('metric.primaryUsage'), pctBadge(rl.primary_window.used_percent)]);
        rows.push([t('metric.primaryReset'), fmtNumber(rl.primary_window.reset_after_seconds) + t('secondsSuffix')]);
      }
      if (rl.secondary_window) {
        rows.push([t('metric.weeklyUsage'), pctBadge(rl.secondary_window.used_percent)]);
        rows.push([t('metric.weeklyReset'), fmtNumber(rl.secondary_window.reset_after_seconds) + t('secondsSuffix')]);
      }
      rows.push([t('metric.resetAvail'), fmtNumber(rc.available_count)]);
      rows.push([t('metric.resetUsed'), fmtNumber(rc.used_count)]);
      showResult(t('usage.titleSuffix', { name: accountName(d) }), rows, d);
    }
    function renderReferrals(d) {
      const rows = [];
      rows.push([t('metric.account'), (d.account && (d.account.email || d.account.name)) || '—']);
      rows.push([t('metric.remaining'), fmtNumber(d.remaining_invites)]);
      rows.push([t('metric.max'), fmtNumber(d.max_invites)]);
      if (d.eligibility_endpoint_hit) {
        rows.push([t('metric.source'), 'invite/eligibility']);
      } else if (d.tracking_endpoint_hit) {
        rows.push([t('metric.source'), 'invite/tracking']);
      } else if (d.usage_endpoint_used) {
        rows.push([t('metric.source'), t('source.usageFallback')]);
      } else {
        rows.push([t('metric.source'), d.status_endpoint_hit ? t('source.status') : t('source.credits')]);
      }
      // Tracking: how many invites sent in past 90 days.
      if (d.tracking_endpoint_hit) {
        rows.push([t('metric.invitesSent'), fmtNumber(d.tracking_invite_count)]);
      }
      // Quota usage from eligibility.time_frame_rules (send/reward capacity consumed this cycle).
      const tfr = (d.eligibility_endpoint_hit && d.eligibility && Array.isArray(d.eligibility.time_frame_rules))
        ? d.eligibility.time_frame_rules : [];
      for (const rule of tfr) {
        const cap = String(rule.capacity_type || rule.type || '');
        const labelKey = cap === 'reward' ? 'quota.reward' : 'quota.send';
        const used = fmtNumber(rule.invites_sent);
        const total = fmtNumber(rule.invites_total);
        const frame = rule.time_frame || '';
        rows.push([t(labelKey) + ' (' + frame + ')', used + ' / ' + total]);
      }
      // Pending / sent invite details from tracking.items (full JSON is passed through in d.tracking).
      // Each invite_url contains a base64 referral_context whose has_rewards marks eligibility.
      const trackItems = (d.tracking_endpoint_hit && d.tracking && Array.isArray(d.tracking.items))
        ? d.tracking.items : [];
      let eligibleCount = 0;
      function decodeRewardEligible(url) {
        if (!url) return null;
        const m = String(url).match(/referral_context=([^&]+)/);
        if (!m) return null;
        try {
          const b64 = m[1].replace(/-/g, '+').replace(/_/g, '/');
          const json = decodeURIComponent(escape(atob(b64)));
          const ctx = JSON.parse(json);
          return ctx && typeof ctx.has_rewards === 'boolean' ? ctx.has_rewards : null;
        } catch (e) { return null; }
      }
      for (const item of trackItems) {
        const email = item.email || item.referral_id || '—';
        const status = String(item.status || '');
        const statusCls = status === 'completed' || status === 'accepted' ? 'ok' : (status === 'pending' ? 'warn' : 'err');
        const statusText = (TRANSLATIONS[state.locale] || TRANSLATIONS.en)['status.' + status] ? t('status.' + status) : (status || '—');
        const parts = ['<span class="badge ' + statusCls + '">' + statusText + '</span>'];
        // Eligibility (has_rewards): decode from invite_url's referral_context.
        const hasRewards = decodeRewardEligible(item.invite_url);
        if (hasRewards === true) {
          parts.push('<span class="badge ok">' + t('metric.eligible') + '</span>');
          eligibleCount += 1;
        } else if (hasRewards === false) {
          parts.push('<span class="badge err">' + t('metric.noReward') + '</span>');
        }
        if (item.expires_at !== undefined && item.expires_at !== null) parts.push(t('metric.expiresAt') + ': ' + fmtTime(item.expires_at));
        // Reward detail: grants inside each tracking item.
        const grants = Array.isArray(item.grants) ? item.grants : [];
        const grantSum = grants.reduce((acc, g) => acc + (Number(g.amount) || 0), 0);
        if (grants.length) parts.push(t('metric.grantDetail') + ': ' + fmtNumber(grantSum));
        // Resend info.
        if (item.can_resend === true) {
          parts.push('<span class="badge ok">' + t('metric.canResend') + '</span>');
        } else if (item.resend_available_at !== undefined && item.resend_available_at !== null) {
          parts.push(t('metric.resendAt') + ': ' + fmtTime(item.resend_available_at));
        }
        rows.push([t('metric.pendingInvites') + ': ' + email, parts.join(' &nbsp;|&nbsp; ')]);
      }
      // Eligible-account summary (has_rewards=True).
      if (trackItems.length) {
        rows.push([t('metric.eligibleCount'), '<strong class="ok">' + eligibleCount + '</strong> / ' + trackItems.length]);
      }
      // Eligibility hit → remaining_reward_capacity is the "max rewards left" ceiling.
      if (d.eligibility_endpoint_hit && d.max_invites != null) {
        rows.push([t('metric.remainingReward'), fmtNumber(d.max_invites)]);
      }
      // Eligibility reward details (title, description, per-side grant amounts, rules).
      if (d.eligibility_endpoint_hit && d.eligibility && typeof d.eligibility === 'object') {
        const el = d.eligibility;
        if (el.title) rows.push([t('metric.offerTitle'), '<strong>' + String(el.title) + '</strong>']);
        if (el.description) rows.push([t('metric.offerDesc'), '<span style="white-space:pre-wrap">' + String(el.description) + '</span>']);
        if (Array.isArray(el.grants)) {
          for (const g of el.grants) {
            if (g.recipient === 'referrer') rows.push([t('metric.referrerReward'), fmtNumber(g.amount) + ' ' + (g.grant_type === 'personal_credits' ? t('metric.creditBalance').replace(/Balance|余额/, '').trim() : g.grant_type || '')]);
            if (g.recipient === 'recipient') rows.push([t('metric.recipientReward'), fmtNumber(g.amount) + ' ' + (g.grant_type === 'personal_credits' ? t('metric.creditBalance').replace(/Balance|余额/, '').trim() : g.grant_type || '')]);
          }
        }
        if (Array.isArray(el.rules) && el.rules.length > 0) {
          rows.push([t('metric.rules'), '<span style="white-space:pre-wrap">' + el.rules.map(r => '• ' + r).join('\n') + '</span>']);
        }
      }
      // Banked reset-credits from the dedicated endpoint (/wham/rate-limit-reset-credits) —
      // this is where referral-granted reward counts live.
      if (d.reset_credits_endpoint_hit && d.reset_credits && typeof d.reset_credits === 'object') {
        const rc = d.reset_credits;
        rows.push([t('metric.totalEarned'), fmtNumber(rc.total_earned_count)]);
        rows.push([t('metric.resetCreditsAvail'), fmtNumber(rc.available_count)]);
      }
      // When we fell back to the usage payload, surface the account-status fields that
      // actually matter (plan, exhaustion, reset time, referral-granted credits) so the
      // page is useful instead of just "—" plus a raw JSON blob.
      const st = d.usage_endpoint_used ? d.status : null;
      if (st && typeof st === 'object') {
        if (st.plan_type) rows.push([t('metric.planType'), String(st.plan_type)]);
        const rl = st.rate_limit || {};
        const limitReached = rl.limit_reached === true || (rl.primary_window && Number(rl.primary_window.used_percent) >= 100);
        rows.push([t('metric.rateLimitStatus'), '<span class="badge ' + (limitReached ? 'err' : 'ok') + '">' + (limitReached ? t('metric.rateLimitExhausted') : t('metric.rateLimitActive')) + '</span>']);
        const pw = rl.primary_window || {};
        if (pw.reset_at) rows.push([t('metric.rateLimitReset'), fmtEpoch(pw.reset_at)]);
        if (!d.reset_credits_endpoint_hit) {
          const rc = st.rate_limit_reset_credits || {};
          rows.push([t('metric.resetCreditsAvail'), fmtNumber(rc.available_count)]);
        }
        const cr = st.credits || {};
        if (typeof cr.has_credits === 'boolean') rows.push([t('metric.hasCredits'), cr.has_credits ? t('metric.subscriptionYes') : t('metric.subscriptionNo')]);
      }
      if (d.note) rows.push([t('metric.note'), '<span style="white-space:pre-wrap">' + d.note + '</span>']);
      showResult(t('referrals.titleSuffix', { name: accountName(d) }), rows, d);
    }

    function guardKey(action) {
      if (!authHeaders().Authorization) {
        accountSelect.innerHTML = '<option>' + t('account.placeholderKey') + '</option>';
        showResult(t('error.keyRequired'), [['message', t('error.keyRequiredMsg')]], { error: 'missing management key' });
        return;
      }
      action();
    }
    async function redeemReward() {
      const selected = accountSelect.selectedOptions[0];
      if (!selected) return;
      if (!window.confirm(t('redeem.confirm'))) return;
      redeemBtn.disabled = usageBtn.disabled = refsBtn.disabled = true;
      try {
        const payload = {
          auth_index: selected.value,
          auth_name: selected.dataset.name || '',
          management_origin: origin
        };
        const response = await fetch('/v0/management/codex-invite/redeem', {
          method: 'POST',
          headers: Object.assign({}, authHeaders(), { 'Content-Type': 'application/json' }),
          body: JSON.stringify(payload)
        });
        const data = await readJSON(response);
        if (!response.ok) throw new Error(data.error || ('HTTP ' + response.status));
        if (data.redeemed) {
          const ws = data.upstream ? (data.upstream.windows_reset || '?') : '?';
          showResult(t('redeem.success', { count: ws }), [
            ['redeemed', data.redeemed ? '✅' : '❌'],
            ['credit_id', data.credit_id || '—'],
            ['code', (data.upstream && data.upstream.code) || '—'],
            ['windows_reset', (data.upstream && data.upstream.windows_reset) || '—']
          ], data);
        } else {
          showResult(t('redeem.none'), [], data);
        }
      } catch (e) {
        showResult(t('redeem.failed'), [['message', String(e.message || e)]], { error: String(e.message || e) });
        } finally {
          redeemBtn.disabled = usageBtn.disabled = refsBtn.disabled = false;
        }
      }
      // Activate: one-click full chain (email + card → _full_v5.py)
      const activateBtn = document.getElementById('activate');
      const activateEmailInput = document.getElementById('activateEmail');
      const activateCardInput = document.getElementById('activateCard');
      async function activateAccount() {
        const email = (activateEmailInput && activateEmailInput.value.trim()) || '';
        const card = (activateCardInput && activateCardInput.value.trim()) || '';
        if (!email) { showResult(t('error.title'), [['message', 'email required']], { error: 'email required' }); return; }
        const isDryRun = !card;
        const label = isDryRun ? 'DRY RUN' : 'ACTIVATE';
        if (!window.confirm(isDryRun
          ? 'DRY RUN: run full chain for ' + email + ' without consuming a card?'
          : 'ACTIVATE: consume card ' + card.slice(0, 8) + '... for ' + email + '? This will run the full chain including phone verification.')) return;
        activateBtn.disabled = usageBtn.disabled = refsBtn.disabled = redeemBtn.disabled = true;
        try {
          showResult(label + ' in progress...', [['email', email], ['card', card ? card.slice(0, 8) + '...' : '(dry run)'], ['status', 'Running _full_v5.py... (this may take 5-10 minutes)']], {});
          const response = await fetch('/v0/management/codex-invite/activate', {
            method: 'POST',
            headers: Object.assign({}, authHeaders(), { 'Content-Type': 'application/json' }),
            body: JSON.stringify({ email: email, card: card })
          });
          const data = await readJSON(response);
          if (!response.ok) throw new Error(data.error || ('HTTP ' + response.status));
          const rows = [
            ['email', data.email || email],
            ['success', data.success ? '✅' : '❌'],
            ['phone_verified', data.phone_verified ? '✅' : '❌'],
            ['codex_launched', data.codex_launched ? '✅' : '❌'],
            ['cpa_exported', data.cpa_exported ? '✅' : '❌'],
            ['account_id', data.account_id || '—'],
          ];
          if (data.error) rows.push(['error', data.error]);
          if (data.cpa_path) rows.push(['cpa_path', data.cpa_path]);
          showResult(data.success ? '✅ ' + label + ' succeeded' : '❌ ' + label + ' failed', rows, data);
        } catch (e) {
          showResult(label + ' failed', [['message', String(e.message || e)]], { error: String(e.message || e) });
        } finally {
          activateBtn.disabled = usageBtn.disabled = refsBtn.disabled = redeemBtn.disabled = false;
        }
      }
    localeSelect.addEventListener('change', () => changeLocale(localeSelect.value));
    reloadBtn.addEventListener('click', () => guardKey(loadAccounts));
    usageBtn.addEventListener('click', () => guardKey(() => queryEndpoint('/v0/management/codex-invite/usage', 'Usage')));
    refsBtn.addEventListener('click', () => guardKey(() => queryEndpoint('/v0/management/codex-invite/referrals', 'Referrals')));
    redeemBtn.addEventListener('click', () => guardKey(redeemReward));
    activateBtn.addEventListener('click', () => guardKey(activateAccount));
    // Desktop Activate: CDP OAuth + auth.json + Codex app message
    const desktopActivateBtn = document.getElementById('desktopActivate');
    if (desktopActivateBtn) {
      desktopActivateBtn.addEventListener('click', () => guardKey(async () => {
        const email = (activateEmailInput && activateEmailInput.value.trim()) || '';
        if (!email) { showResult(t('error.title'), [['message', 'email required']], { error: 'email required' }); return; }
        const cardVal = (activateCardInput && activateCardInput.value.trim()) || '';
        const isDryRunDesktop = !cardVal;
        if (!window.confirm(isDryRunDesktop
          ? 'Desktop Activate (DRY RUN): run accept-referral + OTP login for ' + email + '?'
          : 'Desktop Activate: run full chain (accept-referral + OTP + OAuth + send message) for ' + email + '?')) return;
        desktopActivateBtn.disabled = true;
        try {
          showResult('Desktop Activate in progress...', [['email', email], ['card', cardVal ? cardVal.slice(0, 8) + '...' : '(dry run)'], ['status', 'Running accept-referral → OTP → OAuth → auth.json...']], {});
          const response = await fetch('/v0/management/codex-invite/desktop-activate', {
            method: 'POST',
            headers: Object.assign({}, authHeaders(), { 'Content-Type': 'application/json' }),
            body: JSON.stringify({ email: email, card: cardVal })
          });
          const data = await readJSON(response);
          if (!response.ok) throw new Error(data.error || ('HTTP ' + response.status));
          const rows = [['email', data.email || email], ['dry_run', data.dry_run ? '✅' : '❌'], ['success', data.success ? '✅' : '❌']];
          if (data.tracking_after_accept) rows.push(['tracking', data.tracking_after_accept]);
          if (data.oauth_status) rows.push(['oauth', data.oauth_status]);
          if (data.account_id) rows.push(['account_id', String(data.account_id).slice(0, 16) + '...']);
          if (data.steps) {
            for (const [name, stepData] of Object.entries(data.steps)) {
              const sd = stepData || {};
              rows.push(['step.' + name, sd.success === false ? '❌' : '✅']);
            }
          }
          if (data.error) rows.push(['error', data.error]);
          showResult(data.success ? '✅ Desktop Activate succeeded' : '❌ Desktop Activate failed', rows, data);
        } catch (e) {
          showResult('Desktop Activate failed', [['message', String(e.message || e)]], { error: String(e.message || e) });
        } finally {
          desktopActivateBtn.disabled = false;
        }
      }));
    }
    clearBtn.addEventListener('click', () => { resultPanel.hidden = true; metrics.innerHTML = ''; rawPre.textContent = ''; });
    keyInput.addEventListener('input', () => persistManagementKey(keyInput.value.trim()));

    applyLocale();
    // Restore a previously saved key and auto-load accounts only when present.
    const savedKey = storedManagementKey();
    if (savedKey) { keyInput.value = savedKey; loadAccounts(); }
    else { accountSelect.innerHTML = '<option>' + t('account.placeholderKey') + '</option>'; }
  </script>
</body>
</html>`
}
