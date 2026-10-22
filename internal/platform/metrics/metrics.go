// Package metrics declares the Prometheus metrics exported on :9100/metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the process-wide registry.
var Registry = prometheus.NewRegistry()

var (
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_http_requests_total", Help: "HTTP requests by route, method and status.",
	}, []string{"route", "method", "status"})
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_http_request_duration_seconds", Help: "HTTP request latency.", Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	GitAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_git_api_errors_total", Help: "Failed git provider API calls.",
	}, []string{"provider", "op"})
	WebhookEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_webhook_events_total", Help: "Webhook events by result.",
	}, []string{"result"})
	KafkaLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_kafka_consumer_lag", Help: "Kafka consumer lag by topic.",
	}, []string{"topic"})
	// Agent operator and LLM use (FTR.HMR.CMN-0004 arch §12).
	AgentSessions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_agent_sessions_active", Help: "Active Pi sessions of the agent operator by kind.",
	}, []string{"kind"})
	AgentProcessStarts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_agent_process_starts_total", Help: "Pi process starts by reason (new, restore, crash, check).",
	}, []string{"reason"})
	LLMRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_llm_requests_total", Help: "Agent runs (prompts) by connection, model and scenario.",
	}, []string{"connection", "model", "scenario"})
	LLMErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_llm_errors_total", Help: "LLM errors by class and connection.",
	}, []string{"class", "connection"})
	LLMTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_llm_tokens_total", Help: "LLM tokens by direction (in, out, cache_read, cache_write).",
	}, []string{"direction"})
	LLMCost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_llm_cost_usd_total", Help: "LLM cost in US dollars by connection and model.",
	}, []string{"connection", "model"})
	// Index of the specification repository (FTR.HMR.CMN-0005 arch §11).
	SpecScanRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_spec_scan_runs_total", Help: "Checks of the specification repository by trigger and result.",
	}, []string{"trigger", "result"})
	SpecScanDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "hammurapi_spec_scan_duration_seconds", Help: "Duration of a check of the specification repository.",
		Buckets: []float64{1, 5, 15, 30, 60, 180, 600, 900},
	})
	SpecIndexIssuesOpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_spec_index_issues_open", Help: "Open indexing problems by kind.",
	}, []string{"kind"})
	SpecDocuments = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_spec_documents_total", Help: "Documents in the specification index.",
	})
	SpecIndexedFeatures = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_spec_indexed_features_total", Help: "Features indexed from the repository.",
	})
	GitProviderUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hammurapi_git_provider_up", Help: "1 if the last git provider call succeeded.",
	})
	GateTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_gate_transitions_total", Help: "Gate status transitions.",
	}, []string{"area", "to"})
)

// FTR.HMR.CMN-0002
var (
	WorkflowRuns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_workflow_runs", Help: "Active workflow runs by kind and state.",
	}, []string{"kind", "state"})
	WorkflowTransitionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_workflow_transition_duration_seconds", Help: "Duration of workflow transitions.", Buckets: prometheus.DefBuckets,
	}, []string{"kind", "step"})
	// EffectsFailed counts effects that failed for good: out of attempts or a
	// permanent error (FTR.HMR.DISC-0001 arch §8: the alert on notices).
	EffectsFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_workflow_effects_failed_total", Help: "Effects of workflows that failed for good, by effect.",
	}, []string{"effect"})
	WorkflowBlocked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_workflow_blocked_total", Help: "Workflow runs that became blocked.",
	}, []string{"kind", "reason"})
	RunnerTasks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_runner_tasks", Help: "Agent tasks by type and final status.",
	}, []string{"type", "status"})
	RunnerTaskDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_runner_task_duration_seconds", Help: "Agent task duration.", Buckets: []float64{10, 30, 60, 300, 900, 1800, 3600, 7200},
	}, []string{"type"})
	RunnerTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_runner_tokens_total", Help: "Agent tokens used by task type.",
	}, []string{"type"})
	DeployRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_deploy_runs_total", Help: "Deploy runs by environment and status.",
	}, []string{"environment", "status"})
	ReleaseRollbacks = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hammurapi_release_rollbacks_total", Help: "Rolled back releases.",
	})
	// FTR.HMR.DISC-0001 arch §8.
	DiscoveryRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_discovery_requests_total", Help: "Instructions to the researcher by the state they reached (queued, running, done, failed).",
	}, []string{"state"})
	DiscoveryRequestDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "hammurapi_discovery_request_duration_seconds", Help: "From an instruction to the revision that answers it.",
		Buckets: []float64{60, 180, 300, 600, 1200, 1800, 3600, 7200},
	})
	NabuNotices = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_nabu_notice_total", Help: "Attempts to send notices of Hammurapi to the topics of users by result (sent, gone, error); an error is retried.",
	}, []string{"result"})
	IssueTopicsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hammurapi_issue_topics_created_total", Help: "Topics of issues created in Nabu by a first message.",
	})
	IssueTopicsArchived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_issue_topics_archived_total", Help: "Topics of issues archived in Nabu when the research ended, by result (archived, error).",
	}, []string{"result"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, GitAPIErrors, WebhookEvents, KafkaLag,
		AgentSessions, AgentProcessStarts, LLMRequests, LLMErrors, LLMTokens, LLMCost, GitProviderUp, GateTransitions,
		SpecScanRuns, SpecScanDuration, SpecIndexIssuesOpen, SpecDocuments, SpecIndexedFeatures,
		WorkflowRuns, WorkflowTransitionDuration, WorkflowBlocked, EffectsFailed, RunnerTasks, RunnerTaskDuration,
		RunnerTokens, DeployRuns, ReleaseRollbacks,
		DiscoveryRequests, DiscoveryRequestDuration, NabuNotices, IssueTopicsCreated, IssueTopicsArchived,
	)
}
