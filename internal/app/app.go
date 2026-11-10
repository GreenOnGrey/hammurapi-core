// Package app assembles dependencies and runs the binary modes.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/config"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/admin"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
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
	"github.com/GreenOnGrey/hammurapi-core/internal/features/featuretopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/feedback"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/flags"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/imports"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issuetopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/metricsources"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/nabuconn"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/overview"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/profile"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rollback"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rules"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/services"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/usage"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/validation"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/voice"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/webhooks"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/jobs/cleaner"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/cicd"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
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
	changes   *specchanges.Service
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
	changes := &specchanges.Service{Q: c.pool, Git: c.provider, Lang: contract.Lang(cfg.SpecLanguage)} // FTR.HMR.DEV-0002, FTR.HMR.DEV-0003 R22
	gs := gates.NewService(c.store, c.provider, c.authSvc, c.events, gategen.Generator{Q: c.pool}, branch)
	gs.Changes = changes
	return &slices{
		spec:      spec,
		changes:   changes,
		issues:    issues.NewService(c.store, fs, mt, c.events),
		gates:     gs,
		features:  fs,
		metrics:   mt,
		discovery: discovery.NewService(c.pool, c.events),
		codegen:   codegen.NewService(c.store, c.events),
		catalog:   &catalog.Syncer{Pool: c.pool, Git: c.provider, CatalogChanged: spec.RequestCatalog},
	}
}

func (c *core) toolDeps(s *slices, loadPrincipal agent.PrincipalLoader) agent.ToolDeps {
	return agent.ToolDeps{Store: c.store, Git: c.provider, Tokens: c.authSvc, Gates: s.gates, Principal: loadPrincipal,
		// FTR.HMR.DEV-0002 R3: the writer gets the reasons of wrong rows of the section
		CheckChanges:  s.changes.Refusal,
		DefaultBranch: c.cfg.GitDefaultBranch, TestMetric: s.metrics.Test,
		// FTR.HMR.DISC-0001: instructions to the researcher, the basis and the check of the estimate
		RequestRevision: func(ctx context.Context, p *domain.Principal, g mcp.Grant, key string, brief agent.RevisionBrief) (string, error) {
			r, err := discovery.RequestRevision(ctx, c.pool, discovery.Channel(c.cfg.NabuClientName), p, g, key, brief)
			if err != nil {
				return "", err
			}
			// the id comes first: Nabu keeps 140 characters of the result in the step the web reads it from
			b, _ := json.Marshal(struct {
				RequestID uuid.UUID `json:"requestId"`
				State     string    `json:"state"`
				Position  *int      `json:"position"`
				IssueKey  string    `json:"issueKey"`
			}{r.ID, r.State, r.Position, r.IssueKey})
			return string(b), nil
		},
		// FTR.HMR.DEV-0001: instructions to the writer of specifications
		RequestSpecRevision: func(ctx context.Context, p *domain.Principal, g mcp.Grant, brief agent.SpecBrief) (string, error) {
			r, err := gategen.RequestRevision(ctx, c.pool, c.store, discovery.Channel(c.cfg.NabuClientName), p, g, brief)
			if err != nil {
				return "", err
			}
			// the id comes first: Nabu keeps 140 characters of the result in the step the web reads it from
			b, _ := json.Marshal(struct {
				RequestID  uuid.UUID `json:"requestId"`
				State      string    `json:"state"`
				Position   *int      `json:"position"`
				FeatureKey string    `json:"featureKey"`
				Blocked    bool      `json:"blocked"`
			}{r.ID, r.State, r.Position, r.FeatureKey, r.Blocked})
			return string(b), nil
		},
		ReadSpecQuestions: func(ctx context.Context, p *domain.Principal, key string) (string, error) {
			f, err := features.Load(ctx, c.store, key)
			if err != nil {
				return "", err
			}
			return gategen.QuestionsText(ctx, c.pool, p, f)
		},
		SimilarFeaturesCost: func(ctx context.Context, issueID uuid.UUID, limit int) (string, error) {
			return discovery.SimilarFeaturesCostText(ctx, c.pool, issueID, limit)
		},
		CheckEstimate: func(ctx context.Context, e *cycledata.Estimate) error { return discovery.CheckEstimate(ctx, c.pool, e) },
		CheckMeasure:  func(ctx context.Context, m *cycledata.Measure) error { return discovery.CheckMeasure(ctx, c.pool, m) },
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
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	hub := events.NewHub()
	go hub.Listen(ctx, c.pool)

	tokens := c.authSvc
	branch := cfg.GitDefaultBranch
	loadPrincipal := principalLoader(c.authRepo)
	sl := c.slices()

	approvalSvc := approvals.NewService(c.store, approvals.NewRepository(c.pool), c.provider, tokens, c.events, gategen.Generator{Q: c.pool})
	approvalSvc.Changes = sl.changes
	domainSvc := domains.NewService(c.pool, c.events)
	domainSvc.CatalogChanged = sl.spec.RequestCatalog // R6: a new domain or system triggers a check
	profileSvc := profile.NewService(c.pool)
	adminSvc := admin.NewService(c.pool).WithIdentities(c.authRepo)
	nabuSvc := c.nabuService()
	rulesSvc := rules.NewService(c.pool, c.provider, tokens, branch)
	rulesSvc.Lang = contract.Lang(cfg.SpecLanguage)
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
	mcpServer.Register(agent.Tools(c.toolDeps(sl, loadPrincipal))...)

	var nabuChat *nabuconn.Chat
	nabuMCP := &nabuconn.MCPHandler{Server: mcpServer, EnsureUser: c.authSvc.EnsureDelegated}
	// FTR.HMR.DISC-0001: the topics of issues in the chat
	topics := &issuetopics.Service{Pool: c.pool, Email: c.authRepo.UserEmail}
	// FTR.HMR.DEV-0001: the topics of features, the writer of specifications
	featureTopics := &featuretopics.Service{Pool: c.pool, Store: c.store, Email: c.authRepo.UserEmail}
	writer := gategen.Generator{Q: c.pool}
	sl.features.Writer, sl.features.OpenCounts = writer.Active, writer.OpenCounts
	sl.features.Closed = featureTopics.Archive
	sl.features.Unlocked = writer.Wake
	sl.codegen.Closed = featureTopics.Archive
	if nabuSvc.Enabled() {
		topics.Nabu = nabuSvc.Client
		featureTopics.Nabu = nabuSvc.Client
		issueSvc.KeyChanged = topics.Rename
		issueSvc.Closed = topics.Archive
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
		Login:                     loginInfo(cfg, c.authSvc),
		NabuWebURL:                cfg.NabuWebURL,
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
			topics.Routes(r)
			featureTopics.Routes(r)
			(&gategen.Service{Pool: c.pool, Store: c.store, Events: c.events, Changes: sl.changes, Tokens: tokens}).Routes(r)
			sl.discovery.Routes(r)
			sl.metrics.Routes(r) // FTR.HMR.DISC-0001 R23: the sources to choose from
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
		(&usage.Service{Pool: c.pool}).Routes(r) // the history of usage (FTR.HMR.CMN-0007 R3), global administrators only
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

	api := &http.Server{Addr: cfg.HTTPAddr, Handler: otelhttp.NewHandler(r, "http"), ReadHeaderTimeout: 15 * time.Second}
	svc := serviceServer(cfg.ServiceAddr, c.ready)

	g, gctx := errgroup.WithContext(ctx)
	serve(gctx, g, api, svc)
	return g.Wait()
}

// loginInfo is the sign-in block of /api/v1/config (FTR.HMR.CMN-0006 R1, R2).
func loginInfo(cfg *config.Config, svc *auth.Service) auth.LoginInfo {
	lp := svc.LoginProvider()
	return auth.LoginInfo{Kind: lp.Kind(), Label: lp.Label(), Org: cfg.GitHubAllowedOrg, LinksGit: svc.GitIsLogin()}
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
	rulesSvc.Lang = contract.Lang(cfg.SpecLanguage)
	importSvc := imports.NewService(c.pool, c.store, c.s3, producer, c.provider, tokens, c.events, rulesSvc, loadPrincipal, imports.Config{
		MaxBytes: cfg.ImportMaxBytes, DefaultBranch: cfg.GitDefaultBranch, AllowedAssets: cfg.ImportAllowedAssetTypes,
		Limits: imports.Limits{MaxUncompressed: cfg.ImportMaxUncompressedBytes, MaxFiles: cfg.ImportMaxFiles},
	})
	proc := webhooks.NewProcessor(c.store, c.provider, c.events, sl.catalog, sl.codegen, cfg.BotLogin)

	// Agent sessions of the worker (Analysis, generation, checks, code) are runs
	// of the service agents of Nabu; they reach the worker's own MCP endpoint
	// (result sinks, FTR.HMR.CMN-0007 R4).
	proc.Specs = sl.spec  // pushes to the default branch update the specification index
	proc.Rules = rulesSvc // FTR.HMR.DEV-0003 R20: pushes to rules/ check the templates
	// R20, R24: the templates of the default branch are checked at the start; an empty repository gets the starter rules
	go func() {
		if err := rulesSvc.Start(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "rules contract check at start", "err", err)
		}
	}()
	mcpServer := mcp.NewServer()
	mcpServer.Register(sl.spec.Tools()...)
	mcpServer.Register(agent.Tools(c.toolDeps(sl, loadPrincipal))...)
	ns := c.nabuService()
	agents := &agentrun.Runner{MCP: mcpServer, Usage: &usage.Service{Pool: c.pool},
		Nabu: &agentrun.NabuRuns{Client: ns.Client, Bindings: ns, CallerMCPURL: cfg.NabuWorkerMCPURL}}
	// FTR.HMR.CMN-0007 R5: code tasks in the sandbox of Nabu; the task token is
	// the MCP token of the run and outlives a restart of the worker.
	code := &codegen.Code{Pool: c.pool, Store: c.store, Git: c.provider, Events: c.events, Nabu: ns.Client, Bindings: ns,
		CallerMCPURL: cfg.NabuWorkerMCPURL, UserEmail: c.authRepo.UserEmail, Timeout: cfg.CodeTimeout, TokenKey: cfg.TokenEncryptionKey, CheckMaxRepairs: cfg.CodeCheckMaxRepairs}
	mcpServer.Resolve = code.ResolveMCP
	engine := workflows.New(c.pool, c.events, workflows.Config{MaxAttempts: cfg.WorkflowMaxAttempts, Lease: cfg.WorkflowLease,
		Dispatchers: cfg.WorkflowDispatchers})
	engine.Register(discovery.Machine{}, gategen.Machine{}, codegen.Machine{StartValidation: validation.Start},
		codegen.TaskMachine{Limits: codegen.Limits{MaxParallel: cfg.CodeMaxParallel, Timeout: cfg.CodeTimeout}},
		validation.Machine{}, releases.Machine{}, rollback.Machine{})
	disc := &discovery.Effects{Q: c.pool, Runner: agents, Timeout: cfg.DiscoveryTimeout}
	engine.Handle(discovery.Effect, workflows.EffectHandler{Lease: cfg.DiscoveryTimeout + time.Minute, Do: disc.Do})
	// FTR.HMR.DISC-0001: the next run for waiting instructions and the notices of the result
	engine.Handle(discovery.EffectDrain, workflows.EffectHandler{Do: (&discovery.DrainEffects{Q: c.pool}).Do})
	notices := &discovery.NoticeEffects{Q: c.pool, Email: c.authRepo.UserEmail}
	notices.Nabu = ns.Client
	engine.Handle(discovery.EffectNotice, workflows.EffectHandler{Do: notices.Do})
	specNotices := &gategen.NoticeEffects{Q: c.pool, Email: c.authRepo.UserEmail, Nabu: ns.Client}
	engine.Handle(gategen.EffectNotice, workflows.EffectHandler{Do: specNotices.Do})
	gen := &gategen.Effects{Store: c.store, Git: c.provider, Tokens: tokens, Runner: agents, DefaultBranch: cfg.GitDefaultBranch, Timeout: cfg.WriterTimeout,
		Changes: sl.changes, Lang: contract.Lang(cfg.SpecLanguage)}
	engine.Handle(gategen.Effect, workflows.EffectHandler{Lease: cfg.WriterTimeout + time.Minute, Do: gen.Do})
	engine.Handle(gategen.EffectTrace, workflows.EffectHandler{Do: gen.DoTrace})
	engine.Handle(gategen.EffectDrain, workflows.EffectHandler{Do: (&gategen.DrainEffects{Q: c.pool}).Do})
	check := &validation.Effects{Store: c.store, Git: c.provider, Runner: agents}
	engine.Handle(validation.EffectCheck, workflows.EffectHandler{Lease: 30 * time.Minute, Do: check.Check})
	engine.Handle(codegen.EffectStart, workflows.EffectHandler{Lease: 15 * time.Minute, Do: code.Start})
	engine.Handle(codegen.EffectPoll, workflows.EffectHandler{Do: code.Poll})
	engine.Handle(codegen.EffectApply, workflows.EffectHandler{Lease: 15 * time.Minute, Do: code.Apply})
	engine.Handle(codegen.EffectStop, workflows.EffectHandler{Do: code.Cancel})
	dep := &deploy.Effects{Q: c.pool, Trigger: cicd.New(c.provider), Secrets: c.secrets, PublicURL: cfg.HooksURL}
	engine.Handle(deploy.Effect, workflows.EffectHandler{Do: dep.Do})
	rel := &releases.Effects{Q: c.pool, Store: c.store, Git: c.provider, Tokens: tokens, Changes: sl.changes, DefaultBranch: cfg.GitDefaultBranch}
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
