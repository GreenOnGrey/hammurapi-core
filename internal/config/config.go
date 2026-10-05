// Package config loads instance configuration from environment variables.
package config

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every deployment-level parameter of a Hammurapi instance.
type Config struct {
	// HTTP
	HTTPAddr    string // user API, admin API, hooks
	ServiceAddr string // healthz, readyz, metrics
	MCPAddr     string // deprecated alias of InternalAddr
	// InternalAddr serves the chat MCP endpoint and the runner API (/internal/v1) — cluster-internal only.
	InternalAddr string
	// InternalURL is how agent processes and runner tasks reach InternalAddr.
	InternalURL string
	// WorkerMCPAddr is the worker's own MCP endpoint for its agent sessions
	// (Analysis, generation, checks); WorkerMCPURL is how the agent operator reaches it.
	WorkerMCPAddr string
	WorkerMCPURL  string
	PublicURL     string // external URL of the web app, used for OAuth redirects and cookies
	// PublicWebURL and PublicAPIURL split the SPA and the API across domains
	// (web.<domain>, api.<domain>); both default to PublicURL (one origin).
	PublicWebURL string
	PublicAPIURL string
	// CORSAllowedOrigins may call the API with credentials (the SPA on another domain).
	CORSAllowedOrigins []string
	// CookieDomain is the Domain attribute of the session and CSRF cookies, so
	// the SPA on web.<domain> can read the CSRF token set by api.<domain>.
	CookieDomain string
	// HooksURL is the base URL of /hooks/v1/* as reachable by CI/CD (deploy result callbacks); defaults to PublicAPIURL.
	HooksURL string

	// Git provider
	GitProvider      string // github | gitlab
	GitBaseURL       string
	GitOAuthURL      string // browser-facing base URL for OAuth authorize (defaults to GitBaseURL)
	GitRepo          string // owner/name
	GitDefaultBranch string
	GitHubAppID      string
	GitHubPrivateKey string
	GitHubClientID   string
	GitHubSecret     string
	GitLabClientID   string
	GitLabSecret     string
	WebhookSecret    string
	GitLabBotToken   string
	BotLogin         string // provider login of the bot (agent PRs, own review replies)
	CIResultsSecret  []string

	// Agent operator (FTR.HMR.CMN-0004 tech §12)
	AgentAddr         string        // the operator as api and worker reach it
	AgentRunnerURL    string        // the operator as runner pods reach it
	AgentServiceToken string        // service token of the operator's internal API
	AgentIdleTimeout  time.Duration // a chat session idle this long is saved and closed
	// BootstrapDeepSeekKey creates the first LLM connection once (tech spec §11).
	BootstrapDeepSeekKey string
	// BootstrapDeepSeekURL replaces the preset API address of that connection
	// (development and demos: the scripted fakellm).
	BootstrapDeepSeekURL string

	BootstrapAdmins []string

	// Sign-in (FTR.HMR.CMN-0006 R1, tech §7): AUTH_PROVIDER empty — the git
	// provider as before; github — GitHub with the email and the organization
	// (GITHUB_LOGIN_* or, when empty, the git provider's OAuth app); oidc.
	AuthProvider            string
	OIDCIssuer              string
	OIDCClientID            string
	OIDCClientSecret        string
	OIDCScopes              string
	OIDCProviderName        string
	GitHubLoginClientID     string
	GitHubLoginClientSecret string
	GitHubAllowedOrg        string

	// Nabu (R4, tech §7): empty NABU_URL — the mode without the agent, unless
	// the agent operator of FTR.HMR.CMN-0004 is still deployed (AgentEnabled).
	NabuURL          string
	NabuClientID     string
	NabuClientSecret string
	NabuJWKSCache    time.Duration
	// How Nabu reaches the MCP of Hammurapi for service agents (callerMcp):
	// the worker MCP for its scenarios and the internal /mcp for runner tasks.
	NabuWorkerMCPURL string
	NabuTaskMCPURL   string

	UploadMaxBytes     int64
	UploadAllowedTypes []string

	DatabaseURL  string
	KafkaBrokers []string
	S3Endpoint   string
	S3Bucket     string
	S3AccessKey  string
	S3SecretKey  string
	S3UseSSL     bool
	WhisperURL   string

	TokenEncryptionKey []byte

	OTLPEndpoint    string
	LogLevel        string
	DefaultLanguage string

	ImportMaxBytes             int64
	ImportMaxUncompressedBytes int64
	ImportMaxFiles             int
	ImportAllowedAssetTypes    []string

	// Runner (FTR.HMR.CMN-0002 arch §15)
	RunnerExecutor        string // k8s | local
	RunnerNamespace       string
	RunnerImage           string
	RunnerTimeout         time.Duration
	RunnerTokenLimit      int64
	RunnerMaxParallel     int
	RunnerMaxParallelRepo int
	RunnerWorkdir         string
	RunnerCPU             string
	RunnerMemory          string
	RunnerWorkspacePort   int // the runner's workspace server (FTR.HMR.CMN-0004 arch §4.2)
	WorkflowMaxAttempts   int
	WorkflowLease         time.Duration
	DiscoveryTimeout      time.Duration

	// FTR.HMR.CMN-0005: the check of the specification repository and the navigator.
	SpecScanPushDebounce    time.Duration
	SpecScanPushDebounceMax time.Duration
	SpecScanMaxFileBytes    int64
	SpecScanTimeout         time.Duration
	SpecFilePreviewMaxBytes int64
	SpecSearchMaxLimit      int
	SpecAgentReadMaxChars   int
	SpecAgentPageSize       int
}

// DefaultUploadTypes is the default allow-list of chat attachment MIME types.
var DefaultUploadTypes = []string{
	"image/jpeg", "image/png", "image/gif", "image/webp", "image/heic", "image/heif",
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-powerpoint",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/x-ole-storage", // legacy Office files are often detected as OLE containers
	"text/plain", "application/pdf",
}

// DefaultImportAssetTypes is the default allow-list of files next to spec.md in import archives.
var DefaultImportAssetTypes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml",
	"application/pdf", "text/html",
}

// Load reads configuration from the environment. Mode-specific requirements are checked by Validate.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:                env("HTTP_ADDR", ":8080"),
		ServiceAddr:             env("SERVICE_ADDR", ":9100"),
		MCPAddr:                 env("MCP_ADDR", ""),
		InternalAddr:            env("INTERNAL_ADDR", env("MCP_ADDR", ":8081")),
		WorkerMCPAddr:           env("WORKER_MCP_ADDR", ":8083"),
		PublicURL:               strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		GitProvider:             strings.ToLower(env("GIT_PROVIDER", "")),
		GitBaseURL:              strings.TrimRight(env("GIT_BASE_URL", ""), "/"),
		GitRepo:                 env("GIT_REPO", ""),
		GitDefaultBranch:        env("GIT_DEFAULT_BRANCH", "main"),
		GitHubAppID:             env("GITHUB_APP_ID", ""),
		GitHubPrivateKey:        env("GITHUB_APP_PRIVATE_KEY", ""),
		GitHubClientID:          env("GITHUB_CLIENT_ID", ""),
		GitHubSecret:            env("GITHUB_CLIENT_SECRET", ""),
		GitLabClientID:          env("GITLAB_CLIENT_ID", ""),
		GitLabSecret:            env("GITLAB_CLIENT_SECRET", ""),
		WebhookSecret:           env("WEBHOOK_SECRET", ""),
		GitLabBotToken:          env("GITLAB_BOT_TOKEN", ""),
		BotLogin:                env("HAMMURAPI_BOT_LOGIN", ""),
		CIResultsSecret:         splitList(env("CI_RESULTS_SECRET", ""), ","),
		RunnerExecutor:          strings.ToLower(env("RUNNER_EXECUTOR", "k8s")),
		RunnerNamespace:         env("RUNNER_NAMESPACE", "hammurapi-runners"),
		RunnerImage:             env("RUNNER_IMAGE", ""),
		RunnerWorkdir:           env("RUNNER_WORKDIR", "/var/lib/hammurapi/runs"),
		RunnerCPU:               env("RUNNER_CPU", "2"),
		RunnerMemory:            env("RUNNER_MEMORY", "4Gi"),
		AgentAddr:               strings.TrimRight(env("AGENT_ADDR", "http://agent:8090"), "/"),
		AgentServiceToken:       env("AGENT_SERVICE_TOKEN", ""),
		BootstrapDeepSeekKey:    env("BOOTSTRAP_DEEPSEEK_API_KEY", ""),
		BootstrapDeepSeekURL:    env("BOOTSTRAP_DEEPSEEK_BASE_URL", ""),
		BootstrapAdmins:         splitList(env("BOOTSTRAP_ADMINS", ""), ","),
		AuthProvider:            env("AUTH_PROVIDER", ""),
		OIDCIssuer:              strings.TrimRight(env("OIDC_ISSUER", ""), "/"),
		OIDCClientID:            env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:        env("OIDC_CLIENT_SECRET", ""),
		OIDCScopes:              env("OIDC_SCOPES", "openid email profile"),
		OIDCProviderName:        env("OIDC_PROVIDER_NAME", "Keycloak"),
		GitHubLoginClientID:     env("GITHUB_LOGIN_CLIENT_ID", ""),
		GitHubLoginClientSecret: env("GITHUB_LOGIN_CLIENT_SECRET", ""),
		GitHubAllowedOrg:        env("GITHUB_ALLOWED_ORG", ""),
		NabuURL:                 strings.TrimRight(env("NABU_URL", ""), "/"),
		NabuClientID:            env("NABU_CLIENT_ID", ""),
		NabuClientSecret:        env("NABU_CLIENT_SECRET", ""),
		DatabaseURL:             env("DATABASE_URL", ""),
		KafkaBrokers:            splitList(env("KAFKA_BROKERS", ""), ","),
		S3Endpoint:              env("S3_ENDPOINT", ""),
		S3Bucket:                env("S3_BUCKET", "hammurapi"),
		S3AccessKey:             env("S3_ACCESS_KEY", ""),
		S3SecretKey:             env("S3_SECRET_KEY", ""),
		WhisperURL:              strings.TrimRight(env("WHISPER_URL", ""), "/"),
		OTLPEndpoint:            env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		LogLevel:                env("LOG_LEVEL", "info"),
		DefaultLanguage:         env("DEFAULT_LANGUAGE", "en"),
	}
	var err error
	if c.RunnerWorkspacePort, err = envInt("RUNNER_WORKSPACE_PORT", 8095); err != nil {
		return nil, err
	}
	if c.RunnerTimeout, err = envDuration("RUNNER_TIMEOUT", 2*time.Hour); err != nil {
		return nil, err
	}
	if c.RunnerTokenLimit, err = envInt64("RUNNER_TOKEN_LIMIT", 3_000_000); err != nil {
		return nil, err
	}
	if c.RunnerMaxParallel, err = envInt("RUNNER_MAX_PARALLEL", 10); err != nil {
		return nil, err
	}
	if c.RunnerMaxParallelRepo, err = envInt("RUNNER_MAX_PARALLEL_PER_REPO", 1); err != nil {
		return nil, err
	}
	if c.WorkflowMaxAttempts, err = envInt("WORKFLOW_MAX_ATTEMPTS", 8); err != nil {
		return nil, err
	}
	if c.WorkflowLease, err = envDuration("WORKFLOW_LEASE", 2*time.Minute); err != nil {
		return nil, err
	}
	if c.DiscoveryTimeout, err = envDuration("DISCOVERY_TIMEOUT", 20*time.Minute); err != nil {
		return nil, err
	}
	if c.NabuJWKSCache, err = envDuration("NABU_JWKS_CACHE", 10*time.Minute); err != nil {
		return nil, err
	}
	if c.AgentIdleTimeout, err = envDuration("AGENT_IDLE_TIMEOUT", 15*time.Minute); err != nil {
		return nil, err
	}
	if c.SpecScanPushDebounce, err = envDuration("SPEC_SCAN_PUSH_DEBOUNCE", 30*time.Second); err != nil {
		return nil, err
	}
	if c.SpecScanPushDebounceMax, err = envDuration("SPEC_SCAN_PUSH_DEBOUNCE_MAX", 5*time.Minute); err != nil {
		return nil, err
	}
	if c.SpecScanTimeout, err = envDuration("SPEC_SCAN_TIMEOUT", 15*time.Minute); err != nil {
		return nil, err
	}
	if c.SpecScanMaxFileBytes, err = envBytes("SPEC_SCAN_MAX_FILE_BYTES", 2<<20); err != nil {
		return nil, err
	}
	if c.SpecFilePreviewMaxBytes, err = envBytes("SPEC_FILE_PREVIEW_MAX_BYTES", 5<<20); err != nil {
		return nil, err
	}
	if c.SpecSearchMaxLimit, err = envInt("SPEC_SEARCH_MAX_LIMIT", 50); err != nil {
		return nil, err
	}
	if c.SpecAgentReadMaxChars, err = envInt("SPEC_AGENT_READ_MAX_CHARS", 40000); err != nil {
		return nil, err
	}
	if c.SpecAgentPageSize, err = envInt("SPEC_AGENT_PAGE_SIZE", 20); err != nil {
		return nil, err
	}
	if c.UploadMaxBytes, err = envInt64("UPLOAD_MAX_BYTES", 20<<20); err != nil {
		return nil, err
	}
	if c.ImportMaxBytes, err = envInt64("IMPORT_MAX_BYTES", 50<<20); err != nil {
		return nil, err
	}
	if c.ImportMaxUncompressedBytes, err = envInt64("IMPORT_MAX_UNCOMPRESSED_BYTES", 200<<20); err != nil {
		return nil, err
	}
	if c.ImportMaxFiles, err = envInt("IMPORT_MAX_FILES", 500); err != nil {
		return nil, err
	}
	if c.S3UseSSL, err = strconv.ParseBool(env("S3_USE_SSL", "false")); err != nil {
		return nil, fmt.Errorf("S3_USE_SSL: %w", err)
	}
	c.UploadAllowedTypes = splitList(env("UPLOAD_ALLOWED_TYPES", ""), ",")
	if len(c.UploadAllowedTypes) == 0 {
		c.UploadAllowedTypes = DefaultUploadTypes
	}
	c.ImportAllowedAssetTypes = splitList(env("IMPORT_ALLOWED_ASSET_TYPES", ""), ",")
	if len(c.ImportAllowedAssetTypes) == 0 {
		c.ImportAllowedAssetTypes = DefaultImportAssetTypes
	}
	if k := env("TOKEN_ENCRYPTION_KEY", ""); k != "" {
		if c.TokenEncryptionKey, err = decodeKey(k); err != nil {
			return nil, err
		}
	}
	c.GitOAuthURL = strings.TrimRight(env("GIT_OAUTH_URL", ""), "/")
	if c.GitBaseURL == "" {
		switch c.GitProvider {
		case "github":
			c.GitBaseURL = "https://github.com"
		case "gitlab":
			c.GitBaseURL = "https://gitlab.com"
		}
	}
	if c.GitOAuthURL == "" {
		c.GitOAuthURL = c.GitBaseURL
	}
	c.InternalURL = strings.TrimRight(env("INTERNAL_URL", "http://"+localAddr(c.InternalAddr)), "/")
	c.WorkerMCPURL = strings.TrimRight(env("WORKER_MCP_URL", "http://"+localAddr(c.WorkerMCPAddr)), "/")
	c.NabuWorkerMCPURL = env("NABU_WORKER_MCP_URL", c.WorkerMCPURL+"/mcp")
	c.NabuTaskMCPURL = env("NABU_TASK_MCP_URL", c.InternalURL+"/mcp")
	c.AgentRunnerURL = strings.TrimRight(env("AGENT_RUNNER_URL", c.AgentAddr), "/")
	c.PublicWebURL = strings.TrimRight(env("PUBLIC_WEB_URL", c.PublicURL), "/")
	c.PublicAPIURL = strings.TrimRight(env("PUBLIC_API_URL", c.PublicURL), "/")
	for _, o := range splitList(env("CORS_ALLOWED_ORIGINS", ""), ",") {
		c.CORSAllowedOrigins = append(c.CORSAllowedOrigins, strings.TrimRight(o, "/"))
	}
	c.CookieDomain = strings.TrimPrefix(env("COOKIE_DOMAIN", ""), ".")
	c.HooksURL = strings.TrimRight(env("HOOKS_URL", c.PublicAPIURL), "/")
	if c.RunnerExecutor != "k8s" && c.RunnerExecutor != "local" {
		return nil, fmt.Errorf("RUNNER_EXECUTOR must be k8s or local, got %q", c.RunnerExecutor)
	}
	return c, nil
}

// localAddr turns ":8081" into "127.0.0.1:8081".
func localAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// Validate checks the parameters required by the given binary mode.
func (c *Config) Validate(mode string) error {
	var missing []string
	need := func(name, v string) {
		if v == "" {
			missing = append(missing, name)
		}
	}
	need("DATABASE_URL", c.DatabaseURL)
	if mode == "migrate" {
		return missingErr(missing)
	}
	switch c.GitProvider {
	case "github":
		need("GITHUB_CLIENT_ID", c.GitHubClientID)
		need("GITHUB_CLIENT_SECRET", c.GitHubSecret)
	case "gitlab":
		need("GITLAB_CLIENT_ID", c.GitLabClientID)
		need("GITLAB_CLIENT_SECRET", c.GitLabSecret)
	default:
		return fmt.Errorf("GIT_PROVIDER must be github or gitlab, got %q", c.GitProvider)
	}
	need("GIT_REPO", c.GitRepo)
	if len(c.TokenEncryptionKey) == 0 {
		missing = append(missing, "TOKEN_ENCRYPTION_KEY")
	}
	if mode == "api" || mode == "worker" {
		if len(c.KafkaBrokers) == 0 {
			missing = append(missing, "KAFKA_BROKERS")
		}
	}
	if mode == "api" {
		need("WEBHOOK_SECRET", c.WebhookSecret)
	}
	// AGENT_SERVICE_TOKEN is optional since FTR.HMR.CMN-0006: without it and
	// without NABU_URL Hammurapi works without the agent (R9).
	need("S3_ENDPOINT", c.S3Endpoint)
	switch c.AuthProvider {
	case "":
	case "github":
		// GitHub sign-in with another git provider needs its own OAuth App.
		if c.GitProvider != "github" || c.GitHubLoginClientID != "" {
			need("GITHUB_LOGIN_CLIENT_ID", c.GitHubLoginClientID)
			need("GITHUB_LOGIN_CLIENT_SECRET", c.GitHubLoginClientSecret)
		}
	case "oidc":
		need("OIDC_ISSUER", c.OIDCIssuer)
		need("OIDC_CLIENT_ID", c.OIDCClientID)
		need("OIDC_CLIENT_SECRET", c.OIDCClientSecret)
	default:
		return fmt.Errorf("AUTH_PROVIDER must be empty, github or oidc, got %q", c.AuthProvider)
	}
	if c.NabuURL != "" {
		need("NABU_CLIENT_ID", c.NabuClientID)
		need("NABU_CLIENT_SECRET", c.NabuClientSecret)
	}
	return missingErr(missing)
}

func missingErr(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
}

// decodeKey accepts a 32-byte key as base64 or hex.
func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY must be 32 bytes encoded as base64 or hex")
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

func envInt64(name string, def int64) (int64, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// envBytes reads a size in bytes: a number or a number with KB, MB or GB
// (binary units), e.g. 2MB.
func envBytes(name string, def int64) (int64, error) {
	v := strings.ToUpper(strings.TrimSpace(env(name, "")))
	if v == "" {
		return def, nil
	}
	mult := int64(1)
	for suffix, m := range map[string]int64{"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30} {
		if strings.HasSuffix(v, suffix) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, suffix)), m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: a size like 2MB or a number of bytes is expected", name)
	}
	return n * mult, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := env(name, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// AgentEnabled reports whether Hammurapi has an agent: Nabu or, until the
// transfer, the built-in operator (FTR.HMR.CMN-0006 R9).
func (c *Config) AgentEnabled() bool { return c.NabuURL != "" || c.AgentServiceToken != "" }
