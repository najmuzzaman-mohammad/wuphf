package provider

import "fmt"

// Kind values for ProviderBinding.Kind. The empty string means "fall back to
// the install-wide default" (config.ResolveLLMProvider at dispatch time), which
// keeps manifests written before per-agent providers existed loading unchanged.
const (
	KindClaudeCode   = "claude-code"
	KindCodex        = "codex"
	KindOpencode     = "opencode"
	KindOpenclaw     = "openclaw"
	KindOpenclawHTTP = "openclaw-http"
	KindHermesAgent  = "hermes-agent"
	// KindSlack tags a foreign Slack agent bridged through the Slack transport's
	// membrane: the broker never dispatches LLM turns for it — the agent is a
	// foreign bot that acts in Slack and whose registered bot user id is the
	// inbound routing key (see internal/team/broker_slack_agents.go).
	KindSlack = "slack"
	// OpenAI-compatible HTTP runtimes. These expose the same
	// /v1/chat/completions API; only their default base URL and model differ.
	// Configure per-kind overrides via Config.ProviderEndpoints or
	// WUPHF_<KIND>_BASE_URL / WUPHF_<KIND>_MODEL.
	KindAtlasCloud = "atlascloud"
	KindMLXLM      = "mlx-lm"
	KindOllama     = "ollama"
	KindExo        = "exo"
)

// ProviderBinding is the per-agent runtime selection persisted on an office
// member and on company.MemberSpec. It captures which LLM runtime executes the
// agent's turns (Kind) and which model the runtime should use (Model, free
// form — validated by the runtime itself, not here).
//
// Openclaw is a pointer so agents on other runtimes don't emit an empty object
// into their JSON; it's populated only when Kind == KindOpenclaw.
type ProviderBinding struct {
	Kind     string                   `json:"kind,omitempty"`
	Model    string                   `json:"model,omitempty"`
	Openclaw *OpenclawProviderBinding `json:"openclaw,omitempty"`
	Slack    *SlackProviderBinding    `json:"slack,omitempty"`
}

// OpenclawProviderBinding holds OpenClaw-specific parameters. SessionKey is
// the gateway's identifier for the agent's persistent conversation. AgentID
// is the OpenClaw agent config name (defaults to "main" at creation time).
type OpenclawProviderBinding struct {
	SessionKey string `json:"session_key,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
}

// SlackProviderBinding holds the Slack identity attached to an office member.
// UserID is the bot's Slack user id (U…/W…) — the key the Slack transport
// matches inbound bot-authored messages against. It accompanies two member
// shapes:
//
//   - FOREIGN agents (Kind == KindSlack): bots running outside WUPHF, bridged
//     through the membrane; inbound posts from UserID are ALLOWED.
//   - SPAWNED agents (Kind != KindSlack, BotTokenEnv != ""): real office
//     agents on WUPHF's own runtime that carry their own Slack app identity;
//     inbound posts from UserID are DROPPED (echo guard) and outbound posts
//     are sent with the agent's own token so they appear as that agent.
type SlackProviderBinding struct {
	UserID string `json:"user_id,omitempty"`
	// BotTokenEnv names the ENVIRONMENT VARIABLE holding the bot token
	// (xoxb-…) a spawned office agent posts to Slack with. Only the env var
	// name is ever persisted or sent over the wire — the raw token lives
	// exclusively in process env. Empty for foreign agents.
	BotTokenEnv string `json:"bot_token_env,omitempty"`
}

// ValidateKind reports whether s is an acceptable ProviderBinding.Kind value.
// The empty string is valid and means "use install-wide default."
func ValidateKind(s string) error {
	switch s {
	case "",
		KindClaudeCode, KindCodex, KindOpencode, KindOpenclaw, KindOpenclawHTTP, KindHermesAgent,
		KindSlack, KindAtlasCloud, KindMLXLM, KindOllama, KindExo:
		return nil
	default:
		return fmt.Errorf("unknown provider kind %q (valid: %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, or empty)",
			s, KindClaudeCode, KindCodex, KindOpencode, KindOpenclaw, KindOpenclawHTTP, KindHermesAgent, KindSlack, KindAtlasCloud, KindMLXLM, KindOllama, KindExo)
	}
}

// ResolveKind returns the effective runtime kind for a binding. If the
// binding's Kind is empty, it falls back to global() — the caller provides
// this function so this package stays decoupled from config loading.
func ResolveKind(b ProviderBinding, global func() string) string {
	if b.Kind != "" {
		return b.Kind
	}
	if global == nil {
		return ""
	}
	return global()
}

// IsGatewayKind reports whether kind names a gateway-controlled binding rather
// than a directly-dispatched LLM runtime. Gateway kinds (openclaw, openclaw-http,
// hermes-agent, slack) tag agents that were imported through an external gateway
// or transport membrane; they are managed via the Integrations app, not via the
// Default Runtime picker. Per-agent provider pickers should hide gateway kinds
// and surface a "Managed by <Gateway>" badge instead.
func IsGatewayKind(kind string) bool {
	switch kind {
	case KindOpenclaw, KindOpenclawHTTP, KindHermesAgent, KindSlack:
		return true
	default:
		return false
	}
}
