package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// ---------------------------------------------------------------------------
// Factory registry tests
// ---------------------------------------------------------------------------

func TestRegisterContextManager_Success(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return &noopContextManager{}, nil
	}
	if err := RegisterContextManager("test_cm", factory); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f, ok := lookupContextManager("test_cm")
	if !ok {
		t.Fatal("expected factory to be registered")
	}
	if f == nil {
		t.Fatal("expected non-nil factory")
	}
}

func TestRegisterContextManager_EmptyName(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	err := RegisterContextManager("", func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return &noopContextManager{}, nil
	})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterContextManager_NilFactory(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	err := RegisterContextManager("nil_factory", nil)
	if err == nil {
		t.Fatal("expected error for nil factory")
	}
	if !strings.Contains(err.Error(), "factory is nil") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRegisterContextManager_Duplicate(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return &noopContextManager{}, nil
	}
	if err := RegisterContextManager("dup_cm", factory); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	err := RegisterContextManager("dup_cm", factory)
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLookupContextManager_Unknown(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	_, ok := lookupContextManager("nonexistent")
	if ok {
		t.Fatal("expected lookup to fail for unknown name")
	}
}

// ---------------------------------------------------------------------------
// resolveContextManager tests
// ---------------------------------------------------------------------------

func TestResolveContextManager_Default(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "", // default → legacy
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	cm := al.contextManager
	if cm == nil {
		t.Fatal("expected non-nil context manager")
	}
	if _, ok := cm.(*legacyContextManager); !ok {
		t.Fatalf("expected *legacyContextManager, got %T", cm)
	}
}

func TestResolveContextManager_ExplicitLegacy(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "legacy",
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	if _, ok := al.contextManager.(*legacyContextManager); !ok {
		t.Fatalf("expected *legacyContextManager, got %T", al.contextManager)
	}
}

func TestResolveContextManager_UnknownFallsBackToLegacy(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "unknown_cm",
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	if _, ok := al.contextManager.(*legacyContextManager); !ok {
		t.Fatalf("expected fallback to *legacyContextManager, got %T", al.contextManager)
	}
}

func TestResolveContextManager_RegisteredFactory(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return &noopContextManager{}, nil
	}
	if err := RegisterContextManager("custom_cm", factory); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "custom_cm",
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	if _, ok := al.contextManager.(*noopContextManager); !ok {
		t.Fatalf("expected *noopContextManager, got %T", al.contextManager)
	}
}

func TestResolveContextManager_FactoryError(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return nil, os.ErrPermission
	}
	if err := RegisterContextManager("broken_cm", factory); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "broken_cm",
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	// Should fall back to legacy when factory returns error
	if _, ok := al.contextManager.(*legacyContextManager); !ok {
		t.Fatalf("expected fallback to *legacyContextManager on factory error, got %T", al.contextManager)
	}
}

// ---------------------------------------------------------------------------
// Legacy Assemble tests
// ---------------------------------------------------------------------------

func TestLegacyAssemble_Passthrough(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	history := []providers.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	agent.Sessions.SetHistory("test-session", history)

	resp, err := al.contextManager.Assemble(context.Background(), &AssembleRequest{
		SessionKey: "test-session",
		Budget:     8000,
		MaxTokens:  4096,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.History) != len(history) {
		t.Fatalf("expected %d messages, got %d", len(history), len(resp.History))
	}
	for i, msg := range resp.History {
		if msg.Content != history[i].Content || msg.Role != history[i].Role {
			t.Fatalf("message %d mismatch: want %+v, got %+v", i, history[i], msg)
		}
	}
}

func TestLegacyAssemble_EmptyHistory(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	resp, err := al.contextManager.Assemble(context.Background(), &AssembleRequest{
		SessionKey: "test-session",
		Budget:     8000,
		MaxTokens:  4096,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.History) != 0 {
		t.Fatalf("expected empty messages, got %d", len(resp.History))
	}
}

// ---------------------------------------------------------------------------
// Legacy Compact overflow tests
// ---------------------------------------------------------------------------

func TestLegacyCompact_Overflow(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	history := []providers.Message{
		{Role: "user", Content: "msg 1"},
		{Role: "assistant", Content: "resp 1"},
		{Role: "user", Content: "msg 2"},
		{Role: "assistant", Content: "resp 2"},
		{Role: "user", Content: "msg 3"},
	}
	defaultAgent.Sessions.SetHistory("session-overflow", history)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t,
		al,
		16,
		runtimeevents.KindAgentContextCompress,
	)
	defer closeRuntimeEvents()

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-overflow",
		Reason:     ContextCompressReasonRetry,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// After overflow compression, history should be shorter
	newHistory := defaultAgent.Sessions.GetHistory("session-overflow")
	if len(newHistory) >= len(history) {
		t.Fatalf("expected compressed history, got %d messages (was %d)", len(newHistory), len(history))
	}

	// Summary should contain compression note
	summary := defaultAgent.Sessions.GetSummary("session-overflow")
	if !strings.Contains(summary, "Emergency compression") {
		t.Fatalf("expected compression note in summary, got %q", summary)
	}

	// Event should carry the proactive reason
	events := collectRuntimeEventStream(runtimeCh)
	compressEvt, ok := findRuntimeEvent(events, runtimeevents.KindAgentContextCompress)
	if !ok {
		t.Fatal("expected context compress event")
	}
	payload, ok := compressEvt.Payload.(ContextCompressPayload)
	if !ok {
		t.Fatalf("expected ContextCompressPayload, got %T", compressEvt.Payload)
	}
	if payload.Reason != ContextCompressReasonRetry {
		t.Fatalf("expected retry reason, got %q", payload.Reason)
	}
}

func TestLegacyCompact_Overflow_ProactiveReason(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	history := []providers.Message{
		{Role: "user", Content: "msg 1"},
		{Role: "assistant", Content: "resp 1"},
		{Role: "user", Content: "msg 2"},
		{Role: "assistant", Content: "resp 2"},
		{Role: "user", Content: "msg 3"},
	}
	defaultAgent.Sessions.SetHistory("session-proactive", history)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t,
		al,
		16,
		runtimeevents.KindAgentContextCompress,
	)
	defer closeRuntimeEvents()

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-proactive",
		Reason:     ContextCompressReasonProactive,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := collectRuntimeEventStream(runtimeCh)
	compressEvt, ok := findRuntimeEvent(events, runtimeevents.KindAgentContextCompress)
	if !ok {
		t.Fatal("expected context compress event")
	}
	payload, ok := compressEvt.Payload.(ContextCompressPayload)
	if !ok {
		t.Fatalf("expected ContextCompressPayload, got %T", compressEvt.Payload)
	}
	if payload.Reason != ContextCompressReasonProactive {
		t.Fatalf("expected proactive reason, got %q", payload.Reason)
	}
}

func TestLegacyCompact_Overflow_TooShortToCompress(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	history := []providers.Message{
		{Role: "user", Content: "only one"},
	}
	defaultAgent.Sessions.SetHistory("session-tiny", history)

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-tiny",
		Reason:     ContextCompressReasonRetry,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// History should be unchanged (too short to compress)
	newHistory := defaultAgent.Sessions.GetHistory("session-tiny")
	if len(newHistory) != len(history) {
		t.Fatalf("expected history unchanged, got %d messages (was %d)", len(newHistory), len(history))
	}
}

// ---------------------------------------------------------------------------
// Legacy Compact post-turn tests
// ---------------------------------------------------------------------------

func TestLegacyCompact_PostTurn_BelowThreshold(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	// Small history, below summarization thresholds
	history := []providers.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	defaultAgent.Sessions.SetHistory("session-small", history)

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-small",
		Reason:     ContextCompressReasonSummarize,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// History should remain unchanged
	newHistory := defaultAgent.Sessions.GetHistory("session-small")
	if len(newHistory) != len(history) {
		t.Fatalf("expected unchanged history, got %d messages (was %d)", len(newHistory), len(history))
	}
}

func TestLegacyCompact_PostTurn_ExceedsMessageThreshold(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:                 t.TempDir(),
				ModelName:                 "test-model",
				MaxTokens:                 4096,
				MaxToolIterations:         10,
				ContextWindow:             8000,
				SummarizeMessageThreshold: 2,
				SummarizeTokenPercent:     75,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, &simpleMockProvider{response: "summary"})

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	// 6 messages > threshold of 2
	history := []providers.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	}
	defaultAgent.Sessions.SetHistory("session-threshold", history)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t,
		al,
		16,
		runtimeevents.KindAgentSessionSummarize,
	)
	defer closeRuntimeEvents()

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-threshold",
		Reason:     ContextCompressReasonSummarize,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForRuntimeEvent(t, runtimeCh, 5*time.Second, func(evt runtimeevents.Event) bool {
		return evt.Kind == runtimeevents.KindAgentSessionSummarize
	})

	newHistory := defaultAgent.Sessions.GetHistory("session-threshold")
	if len(newHistory) >= len(history) {
		t.Fatalf("expected summarization to reduce history from %d messages, got %d", len(history), len(newHistory))
	}
}

// ---------------------------------------------------------------------------
// Legacy Ingest tests
// ---------------------------------------------------------------------------

func TestLegacyIngest_NoOp(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	err := al.contextManager.Ingest(context.Background(), &IngestRequest{
		SessionKey: "session-ingest",
		Message:    providers.Message{Role: "user", Content: "test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Mock ContextManager — verifies dispatch through AgentLoop
// ---------------------------------------------------------------------------

func TestAgentLoop_UsesCustomContextManager(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	mock := &trackingContextManager{}
	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return mock, nil
	}
	if err := RegisterContextManager("tracking_cm", factory); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "tracking_cm",
			},
		},
	}
	al := newCMTestAgentLoop(cfg)

	// Verify the mock was installed
	if al.contextManager != mock {
		t.Fatalf("expected mock context manager, got %T", al.contextManager)
	}

	// Direct method calls
	_, err := mock.Assemble(context.Background(), &AssembleRequest{
		SessionKey: "s1",
		Budget:     8000,
		MaxTokens:  4096,
	})
	if err != nil {
		t.Fatalf("Assemble error: %v", err)
	}
	if mock.assembleCalls.Load() != 1 {
		t.Fatalf("expected 1 assemble call, got %d", mock.assembleCalls.Load())
	}

	err = mock.Compact(context.Background(), &CompactRequest{
		SessionKey: "s1",
		Reason:     ContextCompressReasonRetry,
	})
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if mock.compactCalls.Load() != 1 {
		t.Fatalf("expected 1 compact call, got %d", mock.compactCalls.Load())
	}

	err = mock.Ingest(context.Background(), &IngestRequest{
		SessionKey: "s1",
		Message:    providers.Message{Role: "user", Content: "test"},
	})
	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if mock.ingestCalls.Load() != 1 {
		t.Fatalf("expected 1 ingest call, got %d", mock.ingestCalls.Load())
	}
}

func TestIngestCalledDuringTurn(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	mock := &trackingContextManager{}
	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return mock, nil
	}
	if err := RegisterContextManager("ingest_track_cm", factory); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "ingest_track_cm",
			},
		},
	}

	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, &simpleMockProvider{response: "done"})
	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	// Run a turn — ingestMessage is called for user message and final assistant message
	_, err := al.runAgentLoop(context.Background(), defaultAgent, processOptions{
		SessionKey:      "session-ingest-turn",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "test ingest",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}

	// Should have at least 2 ingest calls: user message + final assistant message
	if mock.ingestCalls.Load() < 2 {
		t.Fatalf("expected >= 2 ingest calls during turn, got %d", mock.ingestCalls.Load())
	}
}

func TestClearCommandRoutedAgentCallsContextManagerClear(t *testing.T) {
	cleanup := resetCMRegistry()
	defer cleanup()

	mock := &trackingContextManager{}
	factory := func(cfg json.RawMessage, al *AgentLoop) (ContextManager, error) {
		return mock, nil
	}
	if err := RegisterContextManager("clear_track_cm", factory); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         filepath.Join(workspace, "default"),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				ContextManager:    "clear_track_cm",
			},
			List: []config.AgentConfig{
				{
					ID:        "main",
					Default:   true,
					Workspace: filepath.Join(workspace, "main"),
				},
				{
					ID:        "support",
					Workspace: filepath.Join(workspace, "support"),
				},
			},
			Dispatch: &config.DispatchConfig{
				Rules: []config.DispatchRule{
					{
						Name:  "support-dingtalk",
						Agent: "support",
						When: config.DispatchSelector{
							Channel: "dingtalk",
						},
					},
				},
			},
		},
		Session: config.SessionConfig{
			Dimensions: []string{"chat"},
		},
	}

	al := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: "done"})
	if al.contextManager != mock {
		t.Fatalf("expected mock context manager, got %T", al.contextManager)
	}

	msg := testInboundMessage(bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:  "dingtalk",
			ChatID:   "chat1",
			ChatType: "direct",
			SenderID: "user1",
		},
		Content: "/clear",
	})
	route, _, err := al.resolveMessageRoute(msg)
	if err != nil {
		t.Fatalf("resolveMessageRoute() error = %v", err)
	}
	sessionKey := al.allocateRouteSession(route, msg).SessionKey

	if _, err := al.processMessage(context.Background(), msg); err != nil {
		t.Fatalf("processMessage() error = %v", err)
	}

	if got := mock.clearCalls.Load(); got != 1 {
		t.Fatalf("Clear calls = %d, want 1", got)
	}
	mock.mu.Lock()
	gotKey := mock.lastClearKey
	mock.mu.Unlock()
	if gotKey != sessionKey {
		t.Fatalf("Clear session key = %q, want %q", gotKey, sessionKey)
	}
}

// ---------------------------------------------------------------------------
// forceCompression edge cases (via legacy Compact)
// ---------------------------------------------------------------------------

func TestLegacyCompact_Overflow_SingleTurnKeepsLastUserMessage(t *testing.T) {
	cfg := testConfig(t)
	al := newCMTestAgentLoop(cfg)

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	// History with only 2 messages — forceCompression should still handle it
	history := []providers.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}
	defaultAgent.Sessions.SetHistory("session-2msg", history)

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: "session-2msg",
		Reason:     ContextCompressReasonRetry,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	newHistory := defaultAgent.Sessions.GetHistory("session-2msg")
	// With 2 messages, forceCompression returns false (len <= 2), so no compression
	if len(newHistory) != len(history) {
		t.Fatalf("expected no compression for 2-message history, got %d", len(newHistory))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// noopContextManager is a minimal ContextManager that does nothing.
type noopContextManager struct{}

func (m *noopContextManager) Assemble(_ context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	return &AssembleResponse{}, nil
}
func (m *noopContextManager) Compact(_ context.Context, _ *CompactRequest) error { return nil }
func (m *noopContextManager) Ingest(_ context.Context, _ *IngestRequest) error   { return nil }
func (m *noopContextManager) Clear(_ context.Context, _ string) error            { return nil }

// trackingContextManager tracks call counts for each method.
type trackingContextManager struct {
	assembleCalls atomic.Int64
	compactCalls  atomic.Int64
	ingestCalls   atomic.Int64
	clearCalls    atomic.Int64
	mu            sync.Mutex
	lastAssemble  *AssembleRequest
	lastCompact   *CompactRequest
	lastIngest    *IngestRequest
	lastClearKey  string
}

func (m *trackingContextManager) Assemble(_ context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	m.assembleCalls.Add(1)
	m.mu.Lock()
	m.lastAssemble = req
	m.mu.Unlock()
	return &AssembleResponse{}, nil
}

func (m *trackingContextManager) Compact(_ context.Context, req *CompactRequest) error {
	m.compactCalls.Add(1)
	m.mu.Lock()
	m.lastCompact = req
	m.mu.Unlock()
	return nil
}

func (m *trackingContextManager) Ingest(_ context.Context, req *IngestRequest) error {
	m.ingestCalls.Add(1)
	m.mu.Lock()
	m.lastIngest = req
	m.mu.Unlock()
	return nil
}

func (m *trackingContextManager) Clear(_ context.Context, sessionKey string) error {
	m.clearCalls.Add(1)
	m.mu.Lock()
	m.lastClearKey = sessionKey
	m.mu.Unlock()
	return nil
}

// resetCMRegistry clears the global factory registry and returns a cleanup
// function that restores the original state after the test.
func resetCMRegistry() func() {
	cmRegistryMu.Lock()
	original := make(map[string]ContextManagerFactory, len(cmRegistry))
	for k, v := range cmRegistry {
		original[k] = v
	}
	cmRegistry = make(map[string]ContextManagerFactory)
	cmRegistryMu.Unlock()

	return func() {
		cmRegistryMu.Lock()
		cmRegistry = original
		cmRegistryMu.Unlock()
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
}

func newCMTestAgentLoop(cfg *config.Config) *AgentLoop {
	msgBus := bus.NewMessageBus()
	return NewAgentLoop(cfg, msgBus, &simpleMockProvider{response: "test"})
}

// ---------------------------------------------------------------------------
// Routed-agent regression tests
// ---------------------------------------------------------------------------
//
// These tests mirror the dispatch setup from
// TestClearCommandRoutedAgentCallsContextManagerClear: a default agent ("main")
// and a routed agent ("support"), with history seeded ONLY into the routed
// agent's session store. The legacy context manager must resolve session
// ownership via agentForSession instead of assuming GetDefaultAgent(), or the
// routed store is invisible to Assemble / maybeSummarize / forceCompression.

// newRoutedCMTestAgentLoop builds an AgentLoop with a default agent (main) and
// a routed agent (support). defaults may override AgentDefaults (e.g. a low
// SummarizeMessageThreshold); nil means the standard test defaults.
func newRoutedCMTestAgentLoop(t *testing.T, defaults *config.AgentDefaults) *AgentLoop {
	t.Helper()
	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         filepath.Join(workspace, "main"),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
			List: []config.AgentConfig{
				{ID: "main", Default: true, Workspace: filepath.Join(workspace, "main")},
				{ID: "support", Workspace: filepath.Join(workspace, "support")},
			},
		},
		Session: config.SessionConfig{
			Dimensions: []string{"chat"},
		},
	}
	if defaults != nil {
		cfg.Agents.Defaults = *defaults
	}
	msgBus := bus.NewMessageBus()
	return NewAgentLoop(cfg, msgBus, &simpleMockProvider{response: "summary"})
}

// routedSessionKey builds an opaque session key scoped to the given agent and
// registers its scope metadata on that agent's store so agentForSession can
// resolve ownership (the same resolution Clear() relies on).
func routedSessionKey(t *testing.T, al *AgentLoop, agentID string) string {
	t.Helper()
	agent, ok := al.registry.GetAgent(agentID)
	if !ok || agent == nil {
		t.Fatalf("expected agent %q in registry", agentID)
	}
	scope := &session.SessionScope{
		Version:    session.ScopeVersionV1,
		AgentID:    agentID,
		Channel:    "cli",
		Account:    "default",
		Dimensions: []string{"chat"},
		Values: map[string]string{
			"chat": "direct:user1",
		},
	}
	key := session.BuildSessionKey(*scope)
	ensureSessionMetadata(agent.Sessions, key, scope, nil)
	return key
}

// Assemble must read history/summary from the routed agent's session store,
// not the default agent's store.
func TestLegacyAssemble_RoutedAgent(t *testing.T) {
	al := newRoutedCMTestAgentLoop(t, nil)
	support, ok := al.registry.GetAgent("support")
	if !ok || support == nil {
		t.Fatal("expected support agent")
	}
	key := routedSessionKey(t, al, "support")

	history := []providers.Message{
		{Role: "user", Content: "what did I want before ice cream?"},
		{Role: "assistant", Content: "you wanted a hot dog"},
	}
	support.Sessions.SetHistory(key, history)
	support.Sessions.SetSummary(key, "early summary")
	if err := support.Sessions.Save(key); err != nil {
		t.Fatalf("Save: %v", err)
	}

	resp, err := al.contextManager.Assemble(context.Background(), &AssembleRequest{SessionKey: key})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if len(resp.History) != len(history) {
		t.Fatalf("Assemble() history = %d messages, want %d (routed agent history must be loaded)",
			len(resp.History), len(history))
	}
	if resp.History[0].Content != history[0].Content {
		t.Fatalf("history[0] = %q, want %q", resp.History[0].Content, history[0].Content)
	}
	if resp.Summary != "early summary" {
		t.Fatalf("Assemble() summary = %q, want %q", resp.Summary, "early summary")
	}
}

// maybeSummarize must count messages in the routed
// agent's store and summarize against the routed agent.
func TestLegacyCompact_Summarize_RoutedAgent(t *testing.T) {
	al := newRoutedCMTestAgentLoop(t, &config.AgentDefaults{
		ContextWindow:             8000,
		SummarizeMessageThreshold: 2,
		SummarizeTokenPercent:     75,
	})
	support, ok := al.registry.GetAgent("support")
	if !ok || support == nil {
		t.Fatal("expected support agent")
	}
	key := routedSessionKey(t, al, "support")

	// 6 messages > threshold of 2
	history := []providers.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	}
	support.Sessions.SetHistory(key, history)

	runtimeCh, closeRuntimeEvents := subscribeRuntimeEventsForTest(
		t,
		al,
		16,
		runtimeevents.KindAgentSessionSummarize,
	)
	defer closeRuntimeEvents()

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: key,
		Reason:     ContextCompressReasonSummarize,
	})
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}

	waitForRuntimeEvent(t, runtimeCh, 5*time.Second, func(evt runtimeevents.Event) bool {
		return evt.Kind == runtimeevents.KindAgentSessionSummarize
	})

	newHistory := support.Sessions.GetHistory(key)
	if len(newHistory) >= len(history) {
		t.Fatalf("expected summarization to reduce routed history from %d messages, got %d",
			len(history), len(newHistory))
	}
	if summary := support.Sessions.GetSummary(key); summary == "" {
		t.Fatal("expected summary written to routed agent's store")
	}
}

// TestLegacyCompact_Overflow_RoutedAgent guards forceCompression: overflow
// compression must drop oldest turns in the routed agent's store and leave the
// default agent's store untouched.
func TestLegacyCompact_Overflow_RoutedAgent(t *testing.T) {
	al := newRoutedCMTestAgentLoop(t, nil)
	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}
	support, ok := al.registry.GetAgent("support")
	if !ok || support == nil {
		t.Fatal("expected support agent")
	}
	key := routedSessionKey(t, al, "support")

	history := []providers.Message{
		{Role: "user", Content: "msg 1"},
		{Role: "assistant", Content: "resp 1"},
		{Role: "user", Content: "msg 2"},
		{Role: "assistant", Content: "resp 2"},
		{Role: "user", Content: "msg 3"},
	}
	support.Sessions.SetHistory(key, history)

	err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: key,
		Reason:     ContextCompressReasonRetry,
	})
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}

	newHistory := support.Sessions.GetHistory(key)
	if len(newHistory) >= len(history) {
		t.Fatalf("expected compressed routed history, got %d messages (was %d)",
			len(newHistory), len(history))
	}
	summary := support.Sessions.GetSummary(key)
	if !strings.Contains(summary, "Emergency compression") {
		t.Fatalf("expected compression note in routed summary, got %q", summary)
	}

	// The default agent's store must not be touched by routed compression.
	if h := defaultAgent.Sessions.GetHistory(key); len(h) != 0 {
		t.Fatalf("default agent store unexpectedly has %d messages for routed key", len(h))
	}
}
