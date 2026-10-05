// Package app assembles dependencies and runs the binary modes.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/config"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/admin"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentcfg"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agentrun"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/attachments"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/catalog"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/ciresults"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/domains"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/feedback"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/flags"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/imports"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/metricsources"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/nabuconn"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/overview"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/profile"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rollback"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rules"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/runner"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/services"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/validation"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/voice"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/webhooks"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/jobs/cleaner"
	agentapi "github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/operator"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/cicd"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/executor"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/kafka"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/storage"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/whisper"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// core holds dependencies shared by all modes.
type core struct {
	cfg      *config.Config
	pool     *pgxpool.Pool
	provider git.Provider
	authRepo *auth.Repository
	authSvc  *auth.Service
	store    *specdata.PG
	s3       storage.Storage
	events   *events.PGPublisher
	secrets  *signing.Secrets
	box      *crypto.Box
}

// NewProvider builds the git provider with the bot identity (GitHub App or GitLab bot token).
func NewProvider(cfg *config.Config) (git.Provider, error) {
	switch cfg.GitProvider {
	case "github":
		gh := git.NewGitHub(cfg.GitBaseURL, cfg.GitOAuthURL, cfg.GitRepo, cfg.GitHubClientID, cfg.GitHubSecret)
		return gh.WithApp(cfg.GitHubAppID, cfg.GitHubPrivateKey)
	case "gitlab":
		return git.NewGitLab(cfg.GitBaseURL, cfg.GitOAuthURL, cfg.GitRepo, cfg.GitLabClientID, cfg.GitLabSecret).WithBot(cfg.GitLabBotToken), nil
	}
	return nil, fmt.Errorf("unknown git provider %q", cfg.GitProvider)
}

func newCore(ctx context.Context, cfg *config.Config) (*core, error) {
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	provider, err := NewProvider(cfg)
	if err != nil {
		return nil, err
	}
	box, err := crypto.NewBox(cfg.TokenEncryptionKey)
	if err != nil {
		return nil, err
	}
	s3, err := storage.NewS3(ctx, cfg.S3Endpoint, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3Bucket, cfg.S3UseSSL)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	authRepo := auth.NewRepository(pool)
	authSvc := auth.NewService(authRepo, provider, box, cfg.PublicAPIURL, cfg.BootstrapAdmins, cfg.DefaultLanguage)
	authSvc.SetLogin(loginProvider(cfg, provider), gitAPIURL(cfg))
	domain.SetAgentDisabled(!cfg.AgentEnabled())
	return &core{
		cfg: cfg, pool: pool, provider: provider, authRepo: authRepo, authSvc: authSvc,
		store: specdata.NewPG(pool), s3: s3, events: events.NewPGPublisher(pool), secrets: signing.NewSecrets(box), box: box,
	}, nil
}

func (c *core) close() { c.pool.Close() }

// gitAPIURL is the REST API of the git provider (emails, organizations).
func gitAPIURL(cfg *config.Config) string {
	if cfg.GitProvider == "gitlab" {
		return cfg.GitBaseURL + "/api/v4"
	}
	if cfg.GitBaseURL == "" || cfg.GitBaseURL == "https://github.com" {
		return "https://api.github.com"
	}
	return cfg.GitBaseURL + "/api/v3"
}

// loginProvider builds the sign-in provider (FTR.HMR.CMN-0006 R1): empty
// AUTH_PROVIDER keeps the sign-in through the git provider.
func loginProvider(cfg *config.Config, provider git.Provider) auth.LoginProvider {
	switch cfg.AuthProvider {
	case "github":
		scopes := []string{"read:user", "user:email", "read:org"}
		if cfg.GitProvider == "github" && cfg.GitHubLoginClientID == "" {
			// The OAuth app of the git provider: the sign-in links the git account.
			p := git.NewGitHub(githubBase(cfg), githubOAuth(cfg), cfg.GitRepo, cfg.GitHubClientID, cfg.GitHubSecret).WithScopes(scopes...)
			return &auth.GitLogin{Provider: p, KindName: "github", APIURL: gitAPIURL(cfg), AllowedOrg: cfg.GitHubAllowedOrg, LinksGit: true}
		}
		// A separate OAuth App for sign-in: only the identity, the git account is linked apart.
		base, oauth, api := "https://github.com", "https://github.com", "https://api.github.com"
		if cfg.GitProvider == "github" {
			base, oauth, api = githubBase(cfg), githubOAuth(cfg), gitAPIURL(cfg)
		}
		p := git.NewGitHub(base, oauth, cfg.GitRepo, cfg.GitHubLoginClientID, cfg.GitHubLoginClientSecret).WithScopes(scopes...)
		return &auth.GitLogin{Provider: p, KindName: "github", APIURL: api, AllowedOrg: cfg.GitHubAllowedOrg}
	case "oidc":
		return &auth.OIDCLogin{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, Secret: cfg.OIDCClientSecret,
			Scopes: cfg.OIDCScopes, Name: cfg.OIDCProviderName}
	}
	return &auth.GitLogin{Provider: provider, KindName: "git", APIURL: gitAPIURL(cfg), LinksGit: true}
}

func githubBase(cfg *config.Config) string {
	if cfg.GitBaseURL == "" {
		return "https://github.com"
	}
	return cfg.GitBaseURL
}

func githubOAuth(cfg *config.Config) string {
	if cfg.GitOAuthURL == "" {
		return githubBase(cfg)
	}
	return cfg.GitOAuthURL
}

// nabuService is the connection to Nabu; its Client is nil without NABU_URL.
func (c *core) nabuService() *nabuconn.Service {
	return &nabuconn.Service{Pool: c.pool, Client: nabu.New(c.cfg.NabuURL, c.cfg.NabuClientID, c.cfg.NabuClientSecret), Box: c.box,
		SkillsRepo: c.cfg.GitRepo, DefaultBranch: c.cfg.GitDefaultBranch}
}

// serviceServer exposes /healthz, /readyz and /metrics on the service port.
func serviceServer(addr string, ready func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func (c *core) ready(ctx context.Context) error {
	if err := c.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := kafka.Ping(ctx, c.cfg.KafkaBrokers); err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	return nil
}

// serve runs servers until ctx is done, then shuts them down gracefully.
func serve(ctx context.Context, g *errgroup.Group, servers ...*http.Server) {
	for _, s := range servers {
		s := s
		g.Go(func() error {
			slog.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
		g.Go(func() error {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return s.Shutdown(sctx)
		})
	}
}

func principalLoader(repo *auth.Repository) func(context.Context, uuid.UUID) (*domain.Principal, error) {
	return func(ctx context.Context, id uuid.UUID) (*domain.Principal, error) {
		p, err := repo.Principal(ctx, id)
		if err == nil && p == nil {
			return nil, apperr.NotFound("user_not_found", "user not found")
		}
		return p, err
	}
}

// slices are the services shared by the api and the worker.
type slices struct {
	gates     *gates.Service
	features  *features.Service
	metrics   *metricsources.Service
	discovery *discovery.Service
	codegen   *codegen.Service
	issues    *issues.Service
	catalog   *catalog.Syncer
	spec      *specindex.Service
}

func (c *core) slices() *slices {
	branch := c.cfg.GitDefaultBranch
	cfg := c.cfg
	spec := specindex.NewService(c.pool, c.provider, c.events, specindex.Config{DefaultBranch: branch,
		PushDebounce: cfg.SpecScanPushDebounce, PushDebounceMax: cfg.SpecScanPushDebounceMax, MaxFileBytes: cfg.SpecScanMaxFileBytes,
		Timeout: cfg.SpecScanTimeout, PreviewMaxBytes: cfg.SpecFilePreviewMaxBytes, SearchMaxLimit: cfg.SpecSearchMaxLimit,
		AgentReadMaxChars: cfg.SpecAgentReadMaxChars, AgentPageSize: cfg.SpecAgentPageSize})
	fs := features.NewService(c.store, c.provider, c.authSvc, c.events, branch)
	mt := metricsources.NewService(c.pool, c.secrets)
	return &slices{
		spec:      spec,
		issues:    issues.NewService(c.store, fs, mt, c.events),
		gates:     gates.NewService(c.store, c.provider, c.authSvc, c.events, gategen.Generator{Q: c.pool}, branch),
		features:  fs,
		metrics:   mt,
		discovery: discovery.NewService(c.pool, c.events),
		codegen:   codegen.NewService(c.store, c.events),
		catalog:   &catalog.Syncer{Pool: c.pool, Git: c.provider, CatalogChanged: spec.RequestCatalog},
	}
}

func (c *core) toolDeps(s *slices, loadPrincipal agent.PrincipalLoader) agent.ToolDeps {
	return agent.ToolDeps{Store: c.store, Git: c.provider, Tokens: c.authSvc, Gates: s.gates, Principal: loadPrincipal,
		DefaultBranch: c.cfg.GitDefaultBranch, EditDiscovery: s.discovery.AgentEdit, TestMetric: s.metrics.Test,
		CreateIssue: func(ctx context.Context, p *domain.Principal, in agent.IssueInput) (string, error) {
			is, err := s.issues.Create(ctx, p, issues.CreateInput{Type: domain.IssueType(in.Type), Domain: in.Domain, Title: in.Title,
				Description: in.Description, ViaNabu: true})
			if err != nil {
				return "", err
			}
			return is.Key, nil
		}}
}

// RunAPI runs the HTTP API, SSE, webhooks, the chat agent pool and the internal
// server (:8081: MCP for the chat, the runner API and the runner MCP).
func RunAPI(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	if err := kafka.EnsureTopics(ctx, cfg.KafkaBrokers, kafka.TopicGitPush, kafka.TopicImports); err != nil {
		slog.Warn("could not ensure kafka topics (auto-creation will be used)", "err", err)
	}
	if cfg.RunnerExecutor == "local" {
		slog.Warn("RUNNER_EXECUTOR=local runs agent tasks as subprocesses of the worker without network isolation; it is meant for development and demos, not for production")
	}
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	hub := events.NewHub()
	go hub.Listen(ctx, c.pool)

	tokens := c.authSvc
	branch := cfg.GitDefaultBranch
	loadPrincipal := principalLoader(c.authRepo)
	sl := c.slices()

	approvalSvc := approvals.NewService(c.store, approvals.NewRepository(c.pool), c.provider, tokens, c.events, gategen.Generator{Q: c.pool})
	domainSvc := domains.NewService(c.pool, c.events)
	domainSvc.CatalogChanged = sl.spec.RequestCatalog // R6: a new domain or system triggers a check
	profileSvc := profile.NewService(c.pool)
	adminSvc := admin.NewService(c.pool).WithIdentities(c.authRepo)
	nabuSvc := c.nabuService()
	adminSvc.RunnerExecutor = cfg.RunnerExecutor
	rulesSvc := rules.NewService(c.pool, c.provider, tokens, branch)
	attSvc := attachments.NewService(c.pool, c.s3, cfg.UploadMaxBytes, cfg.UploadAllowedTypes)
	importSvc := imports.NewService(c.pool, c.store, c.s3, producer, c.provider, tokens, c.events, rulesSvc, loadPrincipal, imports.Config{
		MaxBytes: cfg.ImportMaxBytes, DefaultBranch: branch, AllowedAssets: cfg.ImportAllowedAssetTypes,
		Limits: imports.Limits{MaxUncompressed: cfg.ImportMaxUncompressedBytes, MaxFiles: cfg.ImportMaxFiles},
	})
	issueSvc := sl.issues
	validationSvc := validation.NewService(c.pool, c.store, c.provider, tokens, c.events)
	releaseSvc := releases.NewService(c.pool, c.store, c.events)
	serviceSvc := services.NewService(c.pool, sl.catalog)
	overviewSvc := overview.NewService(c.pool)
	trigger := cicd.New(c.provider)
	deployEffects := &deploy.Effects{Q: c.pool, Trigger: trigger, Secrets: c.secrets, PublicURL: cfg.HooksURL}
	deployAdmin := &deploy.Admin{Pool: c.pool, Secrets: c.secrets, Effects: deployEffects}
	flagHook := &flags.Hook{Pool: c.pool, Secrets: c.secrets, Events: c.events}

	// The agent operator (FTR.HMR.CMN-0004): the chat and the runner API open Pi
	// sessions there; the Agent section configures them.
	operatorClient := &agentapi.Client{BaseURL: cfg.AgentAddr, Token: cfg.AgentServiceToken}
	agentCfg := agentcfg.NewService(c.pool, c.box, operatorClient, c.events, c.s3, c.provider, tokens, branch)
	if err := agentCfg.Bootstrap(ctx, cfg.BootstrapDeepSeekKey, cfg.BootstrapDeepSeekURL); err != nil {
		slog.Error("agent bootstrap failed", "err", err)
	}
	overviewSvc.AgentFocus = func(ctx context.Context) ([]overview.Item, error) {
		items, err := agentCfg.Focus(ctx)
		out := make([]overview.Item, 0, len(items))
		for _, it := range items {
			out = append(out, overview.Item{Kind: it.Kind, Key: it.Key, Title: it.Title, Action: it.Action, WaitingSince: it.WaitingSince, Hint: it.Hint})
		}
		return out, err
	}
	overviewSvc.SpecFocus = func(ctx context.Context) ([]overview.Item, error) {
		items, err := sl.spec.Focus(ctx)
		out := make([]overview.Item, 0, len(items))
		for _, it := range items {
			out = append(out, overview.Item{Kind: it.Kind, Key: it.Key, Title: it.Title, Action: it.Action, WaitingSince: it.WaitingSince, Hint: it.Hint})
		}
		return out, err
	}
	mcpServer := mcp.NewServer()
	mcpServer.Register(sl.spec.Tools()...) // spec_* in every scenario (FTR.HMR.CMN-0005 R18)
	chatSvc := agent.NewService(agent.NewRepository(c.pool), c.store, operatorClient, agentCfg, mcpServer, cfg.InternalURL+"/mcp", hub, attSvc, c.s3, loadPrincipal)
	chatSvc.IdleTimeout = cfg.AgentIdleTimeout
	mcpServer.Register(agent.Tools(c.toolDeps(sl, loadPrincipal))...)
	profileSvc.OnAgentChanged = chatSvc.ResetPersona

	internal := &runner.Internal{Pool: c.pool, Store: c.store, Git: c.provider, Events: c.events, MCP: mcpServer, GitBaseURL: cfg.GitBaseURL,
		PublicURL: cfg.InternalURL, Timeout: cfg.RunnerTimeout, TokenLimit: cfg.RunnerTokenLimit,
		Config: agentCfg, AgentURL: cfg.AgentRunnerURL,
		Nabu: nabuSvc.Client, NabuBindings: nabuSvc, NabuMCPURL: cfg.NabuTaskMCPURL, UserEmail: c.authRepo.UserEmail}
	if cfg.AgentServiceToken != "" {
		internal.Operator = operatorClient // the built-in agent (FTR.HMR.CMN-0004) until the transfer
	}
	mcpServer.Resolve = internal.ResolveMCP

	var nabuChat *nabuconn.Chat
	nabuMCP := &nabuconn.MCPHandler{Server: mcpServer, EnsureUser: c.authSvc.EnsureDelegated}
	if nabuSvc.Enabled() {
		nabuChat = &nabuconn.Chat{Client: nabuSvc.Client, Hub: hub, Email: c.authRepo.UserEmail}
		nabuMCP.Verifier = &nabu.Verifier{Issuer: cfg.NabuURL, Audience: "hammurapi", TTL: cfg.NabuJWKSCache}
		if _, err := nabuSvc.Get(ctx); err != nil {
			slog.Warn("nabu settings", "err", err)
		}
	}
	go c.authSvc.BackfillEmails(context.WithoutCancel(ctx))

	authH := auth.NewHandlers(c.authSvc, auth.PublicConfig{
		Provider: cfg.GitProvider, UploadMaxBytes: cfg.UploadMaxBytes, UploadTypes: cfg.UploadAllowedTypes,
		ImportMaxBytes: cfg.ImportMaxBytes, Languages: domain.Languages, DefaultLanguage: cfg.DefaultLanguage, DefaultBranch: branch,
		BootstrapAdminsConfigured: len(cfg.BootstrapAdmins) > 0,
		Login:                     auth.LoginInfo{Kind: c.authSvc.LoginProvider().Kind(), Label: c.authSvc.LoginProvider().Label(), Org: cfg.GitHubAllowedOrg},
		Agent:                     agentInfo(cfg),
	}, strings.HasPrefix(cfg.PublicAPIURL, "https://")).WithWeb(webRedirectBase(cfg), cfg.CookieDomain)

	r := chi.NewRouter()
	r.Use(httpx.Observe)
	r.Use(httpx.CORS(cfg.CORSAllowedOrigins))
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(authH.Authenticate)
		authH.Public(r)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireSession)
			authH.Private(r)
			profileSvc.Routes(r)
			feedback.Routes(r, c.provider, tokens)
			domainSvc.PublicRoutes(r)
			overviewSvc.Routes(r)
			issueSvc.Routes(r)
			sl.discovery.Routes(r)
			sl.spec.Routes(r)
			features.NewHandlers(sl.features).Routes(r)
			gates.NewHandlers(sl.gates).Routes(r)
			approvals.NewHandlers(approvalSvc).Routes(r)
			sl.codegen.Routes(r)
			validationSvc.Routes(r)
			releaseSvc.Routes(r)
			serviceSvc.Routes(r)
			if nabuChat != nil {
				nabuChat.Routes(r) // the chat is the personal agent in Nabu (R5)
			} else if cfg.AgentServiceToken != "" {
				chatSvc.Routes(r)
			}
			voice.Routes(r, whisper.New(cfg.WhisperURL))
			attSvc.Routes(r)
			importSvc.Routes(r)
			if nabuChat != nil {
				r.Method(http.MethodGet, "/events", nabuChat.Events(events.SSEHandler(hub)))
			} else {
				r.Get("/events", events.SSEHandler(hub))
			}
		})
	})
	// The MCP of Hammurapi for the personal agents of Nabu (R7, tech §3.6).
	r.Method(http.MethodPost, "/mcp/nabu", nabuMCP)
	r.Route("/admin/api/v1", func(r chi.Router) {
		r.Use(authH.Authenticate, auth.RequireSession, requireAnyAdmin)
		adminSvc.Routes(r)
		domainSvc.AdminRoutes(r)
		rulesSvc.Routes(r)
		serviceSvc.AdminRoutes(r)
		sl.catalog.Routes(r)
		sl.metrics.AdminRoutes(r)
		deployAdmin.Routes(r)
		flagHook.AdminRoutes(r)
		agentCfg.Routes(r) // global administrators only
		nabuSvc.AdminRoutes(r)
		sl.spec.AdminRoutes(r)
	})
	r.Method(http.MethodPost, "/hooks/v1/git", webhooks.NewReceiver(c.provider, cfg.WebhookSecret, producer))
	r.Method(http.MethodPost, "/hooks/v1/ci-results", &ciresults.Handler{Pool: c.pool, Secrets: cfg.CIResultsSecret, Events: c.events})
	r.Method(http.MethodPost, "/hooks/v1/deploy", &deploy.Hook{Pool: c.pool, Secrets: c.secrets, Events: c.events})
	r.Method(http.MethodPost, "/hooks/v1/feature-flags", flagHook)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, apperr.NotFound("not_found", "not found"))
	})

	internalRouter := chi.NewRouter()
	internalRouter.Handle("/mcp", mcpServer)
	internal.Routes(internalRouter)

	api := &http.Server{Addr: cfg.HTTPAddr, Handler: otelhttp.NewHandler(r, "http"), ReadHeaderTimeout: 15 * time.Second}
	internalHTTP := &http.Server{Addr: cfg.InternalAddr, Handler: otelhttp.NewHandler(internalRouter, "internal"), ReadHeaderTimeout: 15 * time.Second}
	svc := serviceServer(cfg.ServiceAddr, c.ready) // agent health is a metric, not readiness

	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, api, internalHTTP, svc)
	if cfg.AgentServiceToken != "" {
		g.Go(func() error { chatSvc.Run(gctx); return nil })
	}
	return g.Wait()
}

// agentInfo is the agent block of /api/v1/config (tech §3.1).
func agentInfo(cfg *config.Config) auth.AgentInfo {
	switch {
	case cfg.NabuURL != "":
		return auth.AgentInfo{Enabled: true, Provider: "nabu"}
	case cfg.AgentServiceToken != "":
		return auth.AgentInfo{Enabled: true, Provider: "builtin"}
	}
	return auth.AgentInfo{Enabled: false}
}

// RunOperator serves the agent operator (FTR.HMR.CMN-0004 arch §3): the internal
// API on listenAddr and health and metrics on serviceAddr.
func RunOperator(ctx context.Context, op *operator.Operator, listenAddr, serviceAddr string, piCommand []string) error {
	ready := func(context.Context) error {
		if len(piCommand) == 0 {
			return errors.New("PI_BINARY is empty")
		}
		if _, err := exec.LookPath(piCommand[0]); err != nil {
			return fmt.Errorf("pi: %w", err)
		}
		return nil
	}
	srv := &http.Server{Addr: listenAddr, Handler: otelhttp.NewHandler(op.Handler(), "agent"), ReadHeaderTimeout: 15 * time.Second}
	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, srv, serviceServer(serviceAddr, ready))
	g.Go(func() error { op.Run(gctx); return nil })
	return g.Wait()
}

func requireAnyAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpx.PrincipalFrom(r.Context())
		if p == nil || !p.IsAnyAdmin() {
			httpx.Error(w, r, apperr.Forbidden("forbidden", "administrator role required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewExecutor builds the runner executor.
func NewExecutor(cfg *config.Config) (executor.Executor, error) {
	if cfg.RunnerExecutor == "local" {
		l := executor.NewLocal(cfg.RunnerWorkdir, cfg.RunnerTimeout+5*time.Minute, nil)
		// The operator reaches the workspace servers of local tasks at the worker's host name.
		if l.WorkspaceHost = os.Getenv("RUNNER_WORKSPACE_HOST"); l.WorkspaceHost == "" {
			l.WorkspaceHost, _ = os.Hostname()
		}
		return l, nil
	}
	return executor.NewK8s(executor.K8sConfig{Namespace: cfg.RunnerNamespace, Image: cfg.RunnerImage, Timeout: cfg.RunnerTimeout,
		CPU: cfg.RunnerCPU, Memory: cfg.RunnerMemory, WorkspacePort: cfg.RunnerWorkspacePort})
}

// RunWorker consumes webhook events and import jobs, runs the workflow engine
// (state machines and the outbox), starts runner tasks and syncs the catalog.
func RunWorker(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	if err := kafka.EnsureTopics(ctx, cfg.KafkaBrokers, kafka.TopicGitPush, kafka.TopicImports); err != nil {
		slog.Warn("could not ensure kafka topics", "err", err)
	}
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()
	tokens := c.authSvc
	loadPrincipal := principalLoader(c.authRepo)
	sl := c.slices()
	rulesSvc := rules.NewService(c.pool, c.provider, tokens, cfg.GitDefaultBranch)
	importSvc := imports.NewService(c.pool, c.store, c.s3, producer, c.provider, tokens, c.events, rulesSvc, loadPrincipal, imports.Config{
		MaxBytes: cfg.ImportMaxBytes, DefaultBranch: cfg.GitDefaultBranch, AllowedAssets: cfg.ImportAllowedAssetTypes,
		Limits: imports.Limits{MaxUncompressed: cfg.ImportMaxUncompressedBytes, MaxFiles: cfg.ImportMaxFiles},
	})
	proc := webhooks.NewProcessor(c.store, c.provider, c.events, sl.catalog, sl.codegen, cfg.BotLogin)

	// Agent sessions of the worker (Analysis, generation, checks) run in the
	// agent operator and reach the worker's own MCP endpoint (result sinks).
	operatorClient := &agentapi.Client{BaseURL: cfg.AgentAddr, Token: cfg.AgentServiceToken}
	agentCfg := agentcfg.NewService(c.pool, c.box, operatorClient, c.events, c.s3, c.provider, tokens, cfg.GitDefaultBranch)
	proc.Skills = agentCfg // pushes to /agent/ rebuild the skills snapshot
	proc.Specs = sl.spec   // pushes to the default branch update the specification index
	mcpServer := mcp.NewServer()
	mcpServer.Register(sl.spec.Tools()...)
	mcpServer.Register(agent.Tools(c.toolDeps(sl, loadPrincipal))...)
	agents := &agentrun.Runner{Config: agentCfg, MCP: mcpServer, URL: cfg.WorkerMCPURL + "/mcp"}
	if cfg.AgentServiceToken != "" {
		// Without the token there is no built-in agent: unbound scenarios fail at
		// once instead of retrying against an operator that is not there.
		agents.Operator = operatorClient
	}
	if ns := c.nabuService(); ns.Enabled() {
		agents.Nabu = &agentrun.NabuRuns{Client: ns.Client, Bindings: ns, CallerMCPURL: cfg.NabuWorkerMCPURL}
	}

	exec, err := NewExecutor(cfg)
	if err != nil {
		return err
	}
	engine := workflows.New(c.pool, c.events, workflows.Config{MaxAttempts: cfg.WorkflowMaxAttempts, Lease: cfg.WorkflowLease})
	engine.Register(discovery.Machine{}, gategen.Machine{}, codegen.Machine{StartValidation: validation.Start},
		codegen.TaskMachine{Limits: codegen.Limits{MaxParallel: cfg.RunnerMaxParallel, Timeout: cfg.RunnerTimeout}},
		validation.Machine{}, releases.Machine{}, rollback.Machine{})
	disc := &discovery.Effects{Q: c.pool, Runner: agents, Timeout: cfg.DiscoveryTimeout}
	engine.Handle(discovery.Effect, workflows.EffectHandler{Lease: cfg.DiscoveryTimeout + time.Minute, Do: disc.Do})
	gen := &gategen.Effects{Store: c.store, Git: c.provider, Tokens: tokens, Runner: agents, DefaultBranch: cfg.GitDefaultBranch, Timeout: 30 * time.Minute}
	engine.Handle(gategen.Effect, workflows.EffectHandler{Lease: 31 * time.Minute, Do: gen.Do})
	check := &validation.Effects{Store: c.store, Git: c.provider, Runner: agents}
	engine.Handle(validation.EffectCheck, workflows.EffectHandler{Lease: 30 * time.Minute, Do: check.Check})
	run := &codegen.RunnerEffects{Q: c.pool, Executor: exec, InternalURL: cfg.InternalURL}
	engine.Handle(codegen.EffectStart, workflows.EffectHandler{Lease: 5 * time.Minute, Do: run.Start})
	engine.Handle(codegen.EffectStop, workflows.EffectHandler{Do: run.Stop})
	dep := &deploy.Effects{Q: c.pool, Trigger: cicd.New(c.provider), Secrets: c.secrets, PublicURL: cfg.HooksURL}
	engine.Handle(deploy.Effect, workflows.EffectHandler{Do: dep.Do})
	rel := &releases.Effects{Q: c.pool, Store: c.store, Git: c.provider, Tokens: tokens}
	engine.Handle(releases.EffectMerge, workflows.EffectHandler{Do: rel.Merge})
	engine.Handle(releases.EffectMergeSpec, workflows.EffectHandler{Do: rel.MergeSpec})
	engine.Handle(releases.EffectCheckTag, workflows.EffectHandler{Do: rel.CheckTag})
	engine.Handle(releases.EffectClosePRs, workflows.EffectHandler{Do: rel.ClosePRs})

	mcpHTTP := &http.Server{Addr: cfg.WorkerMCPAddr, Handler: mcpServer, ReadHeaderTimeout: 15 * time.Second}
	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, serviceServer(cfg.ServiceAddr, c.ready), mcpHTTP)
	g.Go(func() error {
		return kafka.Consume(gctx, cfg.KafkaBrokers, "hammurapi-worker", kafka.TopicGitPush, proc.Handle)
	})
	g.Go(func() error {
		return kafka.Consume(gctx, cfg.KafkaBrokers, "hammurapi-worker", kafka.TopicImports, importSvc.Handle)
	})
	g.Go(func() error { return engine.Run(gctx) })
	g.Go(func() error { return sl.spec.Run(gctx) }) // checks of the specification repository (FTR.HMR.CMN-0005)
	g.Go(func() error {
		// Full catalog synchronization once a day (arch §10); pushes trigger it in between.
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-t.C:
				if _, err := sl.catalog.Sync(gctx); err != nil {
					var ae *apperr.Error
					if !errors.As(err, &ae) {
						slog.Warn("daily catalog sync failed", "err", err)
					}
				}
			}
		}
	})
	return g.Wait()
}

// RunCleaner runs one maintenance pass.
func RunCleaner(ctx context.Context, cfg *config.Config) error {
	c, err := newCore(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.close()
	cl := &cleaner.Cleaner{Pool: c.pool, S3: c.s3, Store: c.store, Git: c.provider, Tokens: c.authSvc}
	_, err = cl.Run(ctx)
	return err
}

// RunMigrate applies database migrations.
func RunMigrate(ctx context.Context, cfg *config.Config) error {
	return postgres.Migrate(ctx, cfg.DatabaseURL)
}

// webRedirectBase is where the browser returns after sign-in: the SPA's own
// origin when it is served separately from the API, otherwise a relative path.
func webRedirectBase(cfg *config.Config) string {
	if cfg.PublicWebURL != cfg.PublicAPIURL {
		return cfg.PublicWebURL
	}
	return ""
}
