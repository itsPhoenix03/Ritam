// Package proxy is the boundary for the future local LLM proxy.
//
// The proxy is NOT the orchestrator. Its job, once implemented, is entirely
// about provider traffic: transforming requests/responses between whatever
// wire format Claude Code / Codex speak and each provider's API, logging that
// traffic, accounting for token usage, and context optimization (caching,
// trimming, etc.) on the way out. The orchestrator must never depend on a
// provider SDK — it only ever depends on ports.LLMProxy, and in this scaffold
// it does not depend on the proxy at all.
//
// TODO(phase-10): what remains to build a real proxy:
//   - An actual HTTP listener that Start binds and Stop shuts down cleanly.
//   - Request/response transformation between the Anthropic and OpenAI wire
//     formats and whatever routing rule selects the upstream provider.
//   - Structured request logging (respecting ProxyConfig.LogRequests) with
//     the same redaction discipline as internal/logging.
//   - Token accounting per agent/task, exposed for cost reporting.
//   - Context optimization: prompt caching, trimming, dedup across turns.
//   - TLS termination / local-only binding hardening if ever exposed beyond
//     127.0.0.1.
//   - Credential handling: the proxy, not agent config, is the intended place
//     upstream API keys would eventually live (read from the environment or
//     an OS keychain at proxy start time) — never checked into config files
//     or passed to agents directly.
package proxy

import (
	"context"
	"fmt"
	"sync"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// Proxy implements ports.LLMProxy. In this phase it only establishes the
// seam; see the package doc and the TODO(phase-10) list for what is
// deliberately not built yet.
type Proxy struct {
	cfg config.ProxyConfig
	log *logging.Logger

	mu      sync.Mutex
	running bool
}

// New builds a Proxy from configuration. Constructing a Proxy never starts
// anything and never touches the network.
func New(cfg config.ProxyConfig, log *logging.Logger) *Proxy {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &Proxy{cfg: cfg, log: log}
}

// Start brings the proxy up. When disabled, this is a no-op so callers can
// unconditionally call Start/Stop without branching on configuration. When
// enabled, real request handling is not implemented in this phase.
func (p *Proxy) Start(ctx context.Context) error {
	if !p.cfg.Enabled {
		return nil
	}
	return fmt.Errorf("llm proxy: %w", ports.ErrNotImplemented)
}

// Stop shuts the proxy down. It is always safe to call, including when the
// proxy was never started.
func (p *Proxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// Endpoint returns the base URL agents should be pointed at. It is empty
// when the proxy is disabled, since there is nothing for an agent to point
// at.
func (p *Proxy) Endpoint() string {
	if !p.cfg.Enabled {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", p.cfg.Host, p.cfg.Port)
}

// EnvironmentFor returns the environment variables that would point an agent
// of the given kind at the local proxy. Values only — this never returns a
// credential; upstream auth is the proxy's own concern once implemented (see
// the package TODO).
func (p *Proxy) EnvironmentFor(kind domain.AgentKind) map[string]string {
	endpoint := p.Endpoint()
	if endpoint == "" {
		return nil
	}
	switch kind {
	case domain.AgentKindClaudeCode:
		return map[string]string{"ANTHROPIC_BASE_URL": endpoint}
	case domain.AgentKindCodex:
		return map[string]string{"OPENAI_BASE_URL": endpoint}
	default:
		return nil
	}
}

// Enabled reports whether the proxy is configured on.
func (p *Proxy) Enabled() bool {
	return p.cfg.Enabled
}

var _ ports.LLMProxy = (*Proxy)(nil)
