package proxy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/proxy"
)

func TestDisabledProxyIsANoOp(t *testing.T) {
	p := proxy.New(config.ProxyConfig{Enabled: false, Host: "127.0.0.1", Port: 8787}, nil)
	ctx := context.Background()

	if p.Enabled() {
		t.Fatal("expected disabled proxy")
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("expected disabled Start to be a no-op, got %v", err)
	}
	if got := p.Endpoint(); got != "" {
		t.Fatalf("expected empty endpoint when disabled, got %q", got)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("expected Stop to succeed, got %v", err)
	}
	if env := p.EnvironmentFor(domain.AgentKindClaudeCode); env != nil {
		t.Fatalf("expected nil environment when disabled, got %+v", env)
	}
}

func TestEnabledProxyStartReturnsErrNotImplemented(t *testing.T) {
	p := proxy.New(config.ProxyConfig{Enabled: true, Host: "127.0.0.1", Port: 8787}, nil)
	ctx := context.Background()

	if !p.Enabled() {
		t.Fatal("expected enabled proxy")
	}
	err := p.Start(ctx)
	if err == nil || !errors.Is(err, ports.ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
	if got := p.Endpoint(); got != "http://127.0.0.1:8787" {
		t.Fatalf("unexpected endpoint: %q", got)
	}
}

func TestEnvironmentForReturnsExpectedKeysAndNoSecrets(t *testing.T) {
	p := proxy.New(config.ProxyConfig{Enabled: true, Host: "127.0.0.1", Port: 9999}, nil)

	claudeEnv := p.EnvironmentFor(domain.AgentKindClaudeCode)
	if v, ok := claudeEnv["ANTHROPIC_BASE_URL"]; !ok || v != "http://127.0.0.1:9999" {
		t.Fatalf("expected ANTHROPIC_BASE_URL, got %+v", claudeEnv)
	}

	codexEnv := p.EnvironmentFor(domain.AgentKindCodex)
	if v, ok := codexEnv["OPENAI_BASE_URL"]; !ok || v != "http://127.0.0.1:9999" {
		t.Fatalf("expected OPENAI_BASE_URL, got %+v", codexEnv)
	}

	mockEnv := p.EnvironmentFor(domain.AgentKindMock)
	if mockEnv != nil {
		t.Fatalf("expected nil environment for mock kind, got %+v", mockEnv)
	}

	for _, env := range []map[string]string{claudeEnv, codexEnv} {
		for k, v := range env {
			lk, lv := strings.ToLower(k), strings.ToLower(v)
			if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") {
				t.Fatalf("environment key looks like a credential: %q", k)
			}
			if strings.Contains(lv, "sk-") {
				t.Fatalf("environment value looks like a credential: %q=%q", k, v)
			}
		}
	}
}
