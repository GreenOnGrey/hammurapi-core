package domain

import "sync/atomic"

// agentDisabled is the mode without the agent (FTR.HMR.CMN-0006 R9): Hammurapi
// is connected neither to Nabu nor to the built-in agent operator. It is set
// once at start-up.
var agentDisabled atomic.Bool

// SetAgentDisabled switches the mode without the agent.
func SetAgentDisabled(v bool) { agentDisabled.Store(v) }

// AgentDisabled reports the mode without the agent: no Discovery, no
// generation of tech and qa, no code generation; people write everything.
func AgentDisabled() bool { return agentDisabled.Load() }
