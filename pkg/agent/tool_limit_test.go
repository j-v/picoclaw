// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// countingToolLimitProvider always returns tool calls until its tool budget is stripped,
// recording how many calls carried tool definitions.
type countingToolLimitProvider struct {
	mu            sync.Mutex
	callCount     int
	toolDefCounts []int
	summaryText   string
	summaryErr    error
}

func (p *countingToolLimitProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.callCount++
	p.toolDefCounts = append(p.toolDefCounts, len(toolDefs))
	p.mu.Unlock()

	// The wrap-up call is the one with tools stripped.
	if len(toolDefs) == 0 {
		if p.summaryErr != nil {
			return nil, p.summaryErr
		}
		return &providers.LLMResponse{Content: p.summaryText}, nil
	}

	return &providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID:        "call_limit",
			Type:      "function",
			Name:      "tool_limit_test_tool",
			Arguments: map[string]any{"value": "x"},
		}},
	}, nil
}

func (p *countingToolLimitProvider) GetDefaultModel() string {
	return "counting-tool-limit-model"
}

func (p *countingToolLimitProvider) stats() (int, []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCount, append([]int(nil), p.toolDefCounts...)
}

// cheatingToolLimitProvider emits tool calls even when tools are stripped, to prove the
// wrap-up text still wins.
type cheatingToolLimitProvider struct {
	mu        sync.Mutex
	callCount int
}

func (p *cheatingToolLimitProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.callCount++
	p.mu.Unlock()

	if len(toolDefs) == 0 {
		return &providers.LLMResponse{
			Content: "Wrap-up text from the model.",
			ToolCalls: []providers.ToolCall{{
				ID:        "call_cheat",
				Type:      "function",
				Name:      "tool_limit_test_tool",
				Arguments: map[string]any{"value": "cheat"},
			}},
		}, nil
	}

	return &providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID:        "call_limit",
			Type:      "function",
			Name:      "tool_limit_test_tool",
			Arguments: map[string]any{"value": "x"},
		}},
	}, nil
}

func (p *cheatingToolLimitProvider) GetDefaultModel() string {
	return "cheating-tool-limit-model"
}

func newToolLimitTestTurnState(maxIterations int) *turnState {
	return &turnState{
		agentID:    "agent-tool-limit",
		sessionKey: "tool-limit-session",
		agent: &AgentInstance{
			ID:            "agent-tool-limit",
			MaxIterations: maxIterations,
		},
	}
}

func TestBuildToolLimitSummary_BaselineIncludesToolsAndCounts(t *testing.T) {
	ts := newToolLimitTestTurnState(50)
	ts.setIteration(50)
	ts.recordToolExecution("read_file", true, "", nil)
	ts.recordToolExecution("read_file", true, "", nil)
	ts.recordToolExecution("exec", false, "exit status 1", nil)
	ts.recordToolExecution("web_search", true, "", []string{"weather"})

	summary := buildToolLimitSummary(ts, "")

	if !strings.Contains(summary, "50 of 50") {
		t.Errorf("summary missing iteration count: %q", summary)
	}
	if !strings.Contains(summary, "`read_file` ×2") {
		t.Errorf("summary missing aggregated read_file count: %q", summary)
	}
	if !strings.Contains(summary, "`exec` ×1") {
		t.Errorf("summary missing exec row: %q", summary)
	}
	if !strings.Contains(summary, "1 failed") {
		t.Errorf("summary missing failure count: %q", summary)
	}
	if !strings.Contains(summary, "exit status 1") {
		t.Errorf("summary missing error summary: %q", summary)
	}
	if !strings.Contains(summary, "weather") {
		t.Errorf("summary missing skill name: %q", summary)
	}
	if !strings.Contains(summary, "max_tool_iterations") {
		t.Errorf("summary missing remediation hint: %q", summary)
	}
}

func TestBuildToolLimitSummary_LLMTextTakesPrecedenceButKeepsBaseline(t *testing.T) {
	ts := newToolLimitTestTurnState(20)
	ts.setIteration(20)
	ts.recordToolExecution("exec", true, "", nil)

	summary := buildToolLimitSummary(ts, "  I finished the migration but not the tests.  ")

	if !strings.Contains(summary, "I finished the migration but not the tests.") {
		t.Errorf("summary missing model text: %q", summary)
	}
	// The deterministic baseline must survive alongside the model's prose.
	if !strings.Contains(summary, "`exec` ×1") {
		t.Errorf("baseline tool table missing when model text present: %q", summary)
	}
	if strings.Contains(summary, "  I finished") {
		t.Errorf("model text was not trimmed: %q", summary)
	}
}

func TestBuildToolLimitSummary_NoToolExecutionsStillRenders(t *testing.T) {
	ts := newToolLimitTestTurnState(10)
	ts.setIteration(10)

	summary := buildToolLimitSummary(ts, "")

	if !strings.Contains(summary, "10 of 10") {
		t.Errorf("summary missing iteration count: %q", summary)
	}
	if strings.Contains(summary, "**Tools used**") {
		t.Errorf("summary should omit empty tool section: %q", summary)
	}
}

func TestBuildToolLimitSummary_TruncatesDistinctTools(t *testing.T) {
	ts := newToolLimitTestTurnState(99)
	ts.setIteration(99)
	names := []string{
		"tool_a", "tool_b", "tool_c", "tool_d", "tool_e",
		"tool_f", "tool_g", "tool_h", "tool_i", "tool_j",
	}
	for _, name := range names {
		ts.recordToolExecution(name, true, "", nil)
	}

	summary := buildToolLimitSummary(ts, "")

	if !strings.Contains(summary, "`tool_h`") {
		t.Errorf("summary missing 8th tool: %q", summary)
	}
	if strings.Contains(summary, "`tool_i`") || strings.Contains(summary, "`tool_j`") {
		t.Errorf("summary did not truncate beyond cap: %q", summary)
	}
	if !strings.Contains(summary, "…and 2 more tool(s)") {
		t.Errorf("summary missing truncation notice: %q", summary)
	}
}

func TestSingleLine_CollapsesAndTruncates(t *testing.T) {
	if got := singleLine("  a \n b\t c  "); got != "a b c" {
		t.Errorf("singleLine = %q, want %q", got, "a b c")
	}
	if got := singleLine("   "); got != "" {
		t.Errorf("singleLine(blank) = %q, want empty", got)
	}

	long := strings.Repeat("x", toolLimitErrorMaxChars+50)
	got := singleLine(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("singleLine did not truncate long input: %q", got)
	}
	if len([]rune(got)) != toolLimitErrorMaxChars+1 {
		t.Errorf("singleLine rune length = %d, want %d", len([]rune(got)), toolLimitErrorMaxChars+1)
	}
}

func TestToolLimitWrapUp_ExtraCallWithToolsStripped(t *testing.T) {
	provider := &countingToolLimitProvider{summaryText: "Model wrap-up."}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	const maxIterations = 2
	agent.MaxIterations = maxIterations
	al.RegisterTool(&toolLimitTestTool{})

	pipeline := NewPipeline(al)
	opts := makeTestProcessOpts("test-session-tool-limit-wrapup")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-wrapup",
		context: newTurnContext(nil, nil, nil),
	})

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}

	calls, toolDefCounts := provider.stats()
	// Tools run max_iterations times, then exactly one extra call with tools stripped.
	if calls != maxIterations+1 {
		t.Fatalf("provider calls = %d, want %d", calls, maxIterations+1)
	}
	for i := 0; i < maxIterations; i++ {
		if toolDefCounts[i] == 0 {
			t.Errorf("call %d had no tool definitions, want tools available", i+1)
		}
	}
	if toolDefCounts[len(toolDefCounts)-1] != 0 {
		t.Errorf("wrap-up call carried %d tool definitions, want 0", toolDefCounts[len(toolDefCounts)-1])
	}

	if !strings.Contains(result.finalContent, "Model wrap-up.") {
		t.Errorf("final content missing model wrap-up: %q", result.finalContent)
	}
	if !strings.Contains(result.finalContent, "`tool_limit_test_tool` ×") {
		t.Errorf("final content missing tool baseline: %q", result.finalContent)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("status = %v, want Completed", result.status)
	}
}

func TestToolLimitWrapUp_ModelCannotCheatWithToolCalls(t *testing.T) {
	provider := &cheatingToolLimitProvider{}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 1
	al.RegisterTool(&toolLimitTestTool{})

	pipeline := NewPipeline(al)
	opts := makeTestProcessOpts("test-session-tool-limit-cheat")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-cheat",
		context: newTurnContext(nil, nil, nil),
	})

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}

	// Tool calls emitted during the wrap-up call are discarded; the text wins.
	if !strings.Contains(result.finalContent, "Wrap-up text from the model.") {
		t.Errorf("final content should use wrap-up text, got %q", result.finalContent)
	}
}

func TestToolLimitWrapUp_BaselineWhenWrapUpCallFails(t *testing.T) {
	// Use a non-retryable error: transient errors (timeouts, 5xx) would trigger the
	// provider retry loop and add seconds of backoff to this test.
	provider := &countingToolLimitProvider{summaryErr: errors.New("invalid request payload")}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 1
	al.RegisterTool(&toolLimitTestTool{})

	pipeline := NewPipeline(al)
	opts := makeTestProcessOpts("test-session-tool-limit-fail")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-fail",
		context: newTurnContext(nil, nil, nil),
	})

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn should not surface wrap-up failure, got %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("status = %v, want Completed", result.status)
	}
	if !strings.Contains(result.finalContent, "Tool budget reached") {
		t.Errorf("final content missing baseline summary: %q", result.finalContent)
	}
	if !strings.Contains(result.finalContent, "`tool_limit_test_tool` ×") {
		t.Errorf("final content missing tool table: %q", result.finalContent)
	}
	if strings.Contains(result.finalContent, "deadline exceeded") {
		t.Errorf("provider error leaked into user-facing content: %q", result.finalContent)
	}
}

func TestToolLimitWrapUp_SkippedWithoutToolExecutions(t *testing.T) {
	provider := &simpleMockProvider{response: ""}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 1

	opts := makeTestProcessOpts("test-session-tool-limit-none")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-none",
		context: newTurnContext(nil, nil, nil),
	})

	// No tool ran, so the wrap-up must not be armed even at the budget boundary.
	ts.setIteration(1)
	if al.maybeStartToolLimitWrapUp(ts, 0) {
		t.Error("wrap-up requested without any tool execution")
	}
	if ts.toolLimitWrapUpActive() {
		t.Error("tool-limit wrap-up flag set without any tool execution")
	}
}

func TestToolLimitWrapUp_NotArmedBelowBudget(t *testing.T) {
	provider := &simpleMockProvider{response: ""}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 5

	opts := makeTestProcessOpts("test-session-tool-limit-below")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-below",
		context: newTurnContext(nil, nil, nil),
	})
	ts.setIteration(4)
	ts.recordToolExecution("exec", true, "", nil)

	if al.maybeStartToolLimitWrapUp(ts, 0) {
		t.Error("wrap-up requested before the budget was exhausted")
	}
}

func TestToolLimitWrapUp_SubTurnUsesBaselineWithoutExtraCall(t *testing.T) {
	provider := &simpleMockProvider{response: ""}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 1

	opts := makeTestProcessOpts("test-session-tool-limit-subturn")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-subturn",
		context: newTurnContext(nil, nil, nil),
	})
	ts.depth = 1
	ts.setIteration(1)
	ts.recordToolExecution("exec", true, "", nil)

	if al.maybeStartToolLimitWrapUp(ts, 0) {
		t.Error("sub-turn should not spend an extra provider call on a wrap-up")
	}
	if !ts.toolLimitWrapUpActive() {
		t.Error("sub-turn should still be marked as tool-limit so the baseline renders")
	}
}

func TestToolLimitWrapUp_DefersWhileSteeringPending(t *testing.T) {
	provider := &simpleMockProvider{response: ""}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 3

	opts := makeTestProcessOpts("test-session-tool-limit-steering")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-steering",
		context: newTurnContext(nil, nil, nil),
	})
	ts.setIteration(3)
	ts.recordToolExecution("exec", true, "", nil)

	// Someone interrupted an over-budget turn: answer them instead of summarizing.
	if al.maybeStartToolLimitWrapUp(ts, 1) {
		t.Error("wrap-up requested while a steering message was still pending")
	}
	if ts.toolLimitWrapUpActive() {
		t.Error("tool-limit flag set while a steering message was still pending")
	}

	// Once the steering message has been consumed, the wrap-up arms as usual.
	if !al.maybeStartToolLimitWrapUp(ts, 0) {
		t.Error("wrap-up should arm after pending steering is drained")
	}
	if !ts.toolLimitWrapUpActive() {
		t.Error("tool-limit flag should be set after steering is drained")
	}
}

func TestAggregateToolUsage_FirstSeenOrderAndFailures(t *testing.T) {
	usage, truncated := aggregateToolUsage([]ToolExecutionRecord{
		{Name: "b", Success: true},
		{Name: "a", Success: false},
		{Name: "b", Success: false},
	})

	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	if len(usage) != 2 {
		t.Fatalf("usage len = %d, want 2", len(usage))
	}
	if usage[0].name != "b" || usage[0].total != 2 || usage[0].failed != 1 {
		t.Errorf("usage[0] = %+v, want b total 2 failed 1", usage[0])
	}
	if usage[1].name != "a" || usage[1].failed != 1 {
		t.Errorf("usage[1] = %+v, want a failed 1", usage[1])
	}
}

func TestCollectToolErrors_DedupesAndCaps(t *testing.T) {
	records := make([]ToolExecutionRecord, 0, 6)
	for i := 0; i < 5; i++ {
		records = append(records, ToolExecutionRecord{
			Name:         "exec",
			Success:      false,
			ErrorSummary: "same failure",
		})
	}
	records = append(records, ToolExecutionRecord{Name: "exec", Success: true})

	errs := collectToolErrors(records)
	if len(errs) != 1 {
		t.Fatalf("errs len = %d, want 1 (deduped)", len(errs))
	}
	if errs[0] != "same failure" {
		t.Errorf("errs[0] = %q, want %q", errs[0], "same failure")
	}
}

func TestCollectToolErrors_FallsBackToToolName(t *testing.T) {
	errs := collectToolErrors([]ToolExecutionRecord{
		{Name: "read_file", Success: false, ErrorSummary: "   "},
	})
	if len(errs) != 1 {
		t.Fatalf("errs len = %d, want 1", len(errs))
	}
	if errs[0] != "read_file failed" {
		t.Errorf("errs[0] = %q, want %q", errs[0], "read_file failed")
	}
}

// Ensure the tool-limit test tool still satisfies the tools interface after refactors.
var _ tools.Tool = (*toolLimitTestTool)(nil)

func TestToolLimitWrapUp_NoRegressionOnNormalTurn(t *testing.T) {
	// A turn that finishes well inside its budget must not spend an extra call or
	// stamp the tool-limit summary onto an ordinary answer.
	provider := &directAnswerAfterToolsProvider{answerAtCall: 3, answer: "All done."}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	agent.MaxIterations = 50
	al.RegisterTool(&toolLimitTestTool{})

	pipeline := NewPipeline(al)
	opts := makeTestProcessOpts("test-session-tool-limit-normal")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-tool-limit-normal",
		context: newTurnContext(nil, nil, nil),
	})

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}

	if calls := provider.callCount(); calls != 3 {
		t.Errorf("provider calls = %d, want 3 (no wrap-up call)", calls)
	}
	if ts.toolLimitWrapUpActive() {
		t.Error("tool-limit wrap-up armed on a turn that finished inside its budget")
	}
	if strings.Contains(result.finalContent, "Tool budget reached") {
		t.Errorf("normal turn rendered a tool-limit summary: %q", result.finalContent)
	}
	if result.finalContent != "All done." {
		t.Errorf("final content = %q, want %q", result.finalContent, "All done.")
	}
}

// directAnswerAfterToolsProvider returns tool calls until call N, then a direct answer.
type directAnswerAfterToolsProvider struct {
	mu           sync.Mutex
	callCountVal int
	answerAtCall int
	answer       string
}

func (p *directAnswerAfterToolsProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.callCountVal++
	call := p.callCountVal
	p.mu.Unlock()

	if call >= p.answerAtCall || len(toolDefs) == 0 {
		return &providers.LLMResponse{Content: p.answer}, nil
	}

	return &providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID:        "call_normal",
			Type:      "function",
			Name:      "tool_limit_test_tool",
			Arguments: map[string]any{"value": "x"},
		}},
	}, nil
}

func (p *directAnswerAfterToolsProvider) GetDefaultModel() string {
	return "direct-answer-model"
}

func (p *directAnswerAfterToolsProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCountVal
}
