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
	CodeTaskStates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_code_task_states_total", Help: "Code tasks entering a status, by type.",
	}, []string{"type", "status"})
	CodeTaskDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_code_task_duration_seconds", Help: "Code task duration.", Buckets: []float64{10, 30, 60, 300, 900, 1800, 3600, 7200},
	}, []string{"type"})
	CodeTokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_code_tokens_total", Help: "Agent tokens of code tasks by type.",
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
	SpecRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_spec_runs_total", Help: "Runs of the writer of specifications by mode and result (done, failed).",
	}, []string{"mode", "result"})
	SpecRunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "hammurapi_spec_run_duration_seconds", Help: "Duration of the agent session of a run of the writer by mode.",
		Buckets: []float64{60, 180, 300, 600, 1200, 1800, 3600},
	}, []string{"mode"})
	// SpecChangeNotes counts notes of changed requirements (FTR.HMR.DEV-0002 arch §7):
	// applied, rejected rows, re-applied after a conflict of the merge.
	SpecChangeNotes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_spec_change_notes_total", Help: "Notes in changed features by result (applied, rejected, conflict_reapplied).",
	}, []string{"result"})
	// RulesContractRejections counts templates refused by the document contract (FTR.HMR.DEV-0003 arch §7).
	RulesContractRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_rules_contract_rejections_total", Help: "Rules templates refused by the document contract by stage (propose, approve).",
	}, []string{"stage"})
	// FTR.HMR.SPEC-0002 arch §7: the check of speclint in CI of repositories.
	CITestCaseRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_ci_test_cases_requests_total", Help: "Requests of CI for the test cases of a service by result (ok, unauthorized, revoked, forbidden, not_found, rate_limited, error) and by the way in (oidc_github, oidc_gitlab, oidc — the keys of the issuer not reachable, installation, none).",
	}, []string{"result", "auth"})
	// FTR.HMR.SPEC-0004 arch §7: submissions refused by the services, the C4 diagram and the contract files.
	SpecContractRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_spec_contract_rejections_total", Help: "Violations of the document contract that refused a submission of arch or tech, by area and check.",
	}, []string{"area", "check"})
	// FTR.HMR.SPEC-0004 tech §3.2: the catalog for the CI of the repository of specifications.
	CIServicesRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_ci_services_requests_total", Help: "Requests of CI for the keys of the catalog by result and by the way in, as hammurapi_ci_test_cases_requests_total.",
	}, []string{"result", "auth"})
	CIChecks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hammurapi_ci_checks", Help: "Repositories by the status of their check of speclint (target: spec, service).",
	}, []string{"target", "status"})
	CICheckPRs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_ci_check_prs_total", Help: "Pull requests of the check of speclint by kind (propose, bump) and result.",
	}, []string{"kind", "result"})
	SpecRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_spec_requests_total", Help: "Instructions to the writer by the state they reached (queued, running, done, failed).",
	}, []string{"state"})
	FeatureTopicsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hammurapi_feature_topics_created_total", Help: "Topics of features created in Nabu by a first message.",
	})
	IssueTopicsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hammurapi_issue_topics_created_total", Help: "Topics of issues created in Nabu by a first message.",
	})
	IssueTopicsArchived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_issue_topics_archived_total", Help: "Topics of issues archived in Nabu when the research ended, by result (archived, error).",
	}, []string{"result"})
	DiscoveryQuestionsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hammurapi_discovery_questions_dropped_total", Help: "Questions of the researcher dropped: over the limit of a round or without a title or a recommendation.",
	})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, GitAPIErrors, WebhookEvents, KafkaLag,
		AgentSessions, AgentProcessStarts, LLMRequests, LLMErrors, LLMTokens, LLMCost, GitProviderUp, GateTransitions,
		SpecScanRuns, SpecScanDuration, SpecIndexIssuesOpen, SpecDocuments, SpecIndexedFeatures,
		WorkflowRuns, WorkflowTransitionDuration, WorkflowBlocked, EffectsFailed, CodeTaskStates, CodeTaskDuration,
		CodeTokens, DeployRuns, ReleaseRollbacks,
		SpecRuns, SpecRunDuration, SpecRequests, SpecChangeNotes, RulesContractRejections, CITestCaseRequests, CIServicesRequests, SpecContractRejections, CIChecks, CICheckPRs, FeatureTopicsCreated, DiscoveryRequests, DiscoveryRequestDuration, NabuNotices, IssueTopicsCreated, IssueTopicsArchived, DiscoveryQuestionsDropped,
	)
}

// FTR.HMR.CMN-0007 arch §8: code tasks in the sandbox of Nabu.
var (
	CodeTasks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hammurapi_code_tasks_total", Help: "Finished code tasks by type, result and error class.",
	}, []string{"type", "result", "error_class"})
	CodeStartSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "hammurapi_code_start_seconds", Help: "Preparing the working copy and starting the run of Nabu.",
		Buckets: []float64{1, 3, 10, 30, 60, 120, 300, 600},
	})
	CodeApplySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "hammurapi_code_apply_seconds", Help: "Taking the changes of a run and committing them.",
		Buckets: []float64{1, 3, 10, 30, 60, 120, 300},
	})
)

func init() { Registry.MustRegister(CodeTasks, CodeStartSeconds, CodeApplySeconds) }

// FTR.HMR.SPEC-0006 arch §8: answers of the spec tools by source — the index or the branch of a feature in work.
var SpecToolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hammurapi_spec_tool_calls_total", Help: "Answers of spec_read, spec_requirements and spec_links by source.",
}, []string{"tool", "source"})

func init() { Registry.MustRegister(SpecToolCalls) }

// FTR.HMR.SPEC-0005 arch §8: test cases of another service left out of the answer to the CI of a service.
var CITestCasesFiltered = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hammurapi_ci_test_cases_filtered_total", Help: "Test cases marked with other services, left out of the test cases of a service.",
}, []string{"service"})

func init() { Registry.MustRegister(CITestCasesFiltered) }
