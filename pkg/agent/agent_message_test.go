package agent

import (
	"testing"
)

// TestSeedThreadSessionFromStartMessage verifies that a thread session is
// seeded with the parent-channel message it was created from when it has no
// history yet.
func TestSeedThreadSessionFromStartMessage(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	al.seedThreadSessionFromStartMessage(agent, "sk_thread", "[thread started from jonotron's message]: hello")

	history := agent.Sessions.GetHistory("sk_thread")
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	got := history[0]
	if got.Role != "user" {
		t.Fatalf("role = %q, want user", got.Role)
	}
	wantContent := "[thread started from jonotron's message]: hello"
	if got.Content != wantContent {
		t.Fatalf("content = %q, want %q", got.Content, wantContent)
	}
}

// TestSeedThreadSessionFromStartMessage_KeepsExistingHistory verifies that an
// already-active thread session is not re-seeded on later messages.
func TestSeedThreadSessionFromStartMessage_KeepsExistingHistory(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	agent.Sessions.AddMessage("sk_thread", "user", "first message in thread")
	al.seedThreadSessionFromStartMessage(agent, "sk_thread", "[thread started from jonotron's message]: hello")

	history := agent.Sessions.GetHistory("sk_thread")
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1 (start message must not be appended when history exists)", len(history))
	}
	if got := history[0].Content; got != "first message in thread" {
		t.Fatalf("content = %q, want %q (existing history must be preserved)", got, "first message in thread")
	}
}

// TestSeedThreadSessionFromStartMessage_EmptyStart verifies no-op cases: empty
// start message, nil agent, nil session store.
func TestSeedThreadSessionFromStartMessage_EmptyStart(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	al.seedThreadSessionFromStartMessage(agent, "sk_thread", "")
	al.seedThreadSessionFromStartMessage(agent, "sk_thread", "   ")
	if got := len(agent.Sessions.GetHistory("sk_thread")); got != 0 {
		t.Fatalf("history = %d messages, want 0 for empty start message", got)
	}

	al.seedThreadSessionFromStartMessage(nil, "sk_thread", "hello")
	al.seedThreadSessionFromStartMessage(&AgentInstance{}, "sk_thread", "hello")
}

// TestSeedThreadSessionFromStartMessage_TrimmedInput verifies whitespace-only
// start messages are treated as absent (trimmed before seeding).
func TestSeedThreadSessionFromStartMessage_TrimmedInput(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	al.seedThreadSessionFromStartMessage(agent, "sk_thread", "  [thread started from jonotron's message]: hello  ")
	history := agent.Sessions.GetHistory("sk_thread")
	if len(history) != 1 {
		t.Fatalf("history = %d messages, want 1", len(history))
	}
	if got := history[0].Content; got != "[thread started from jonotron's message]: hello" {
		t.Fatalf("content = %q, want trimmed start message", got)
	}
}
