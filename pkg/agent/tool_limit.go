// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	// toolLimitSummaryMaxTools caps the distinct tool rows rendered in a wrap-up so the
	// message stays readable in chat channels with short message limits.
	toolLimitSummaryMaxTools = 8
	// toolLimitSummaryMaxErrors caps how many distinct error summaries are listed.
	toolLimitSummaryMaxErrors = 3
	// toolLimitErrorMaxChars truncates a single error summary to a single readable line.
	toolLimitErrorMaxChars = 120
)

// toolUsage aggregates repeated invocations of the same tool for the wrap-up message.
type toolUsage struct {
	name   string
	total  int
	failed int
}

// markToolLimitWrapUp records that this turn exhausted its tool budget. The flag is sticky:
// once set, the coordinator knows the final message must be a tool-limit wrap-up rather than a
// plain assistant answer or the generic default response.
func (ts *turnState) markToolLimitWrapUp() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolLimitWrapUp = true
}

// toolLimitWrapUpActive reports whether this turn already hit its tool budget.
func (ts *turnState) toolLimitWrapUpActive() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.toolLimitWrapUp
}

// maybeStartToolLimitWrapUp arms the graceful wrap-up once the tool budget is spent.
//
// It must be called after tool execution, once the iteration counter has reached
// MaxIterations. The loop's existing graceful-interrupt clause then extends the turn by exactly
// one call, so the configured budget is honoured rather than silently reduced.
//
// pendingSteering defers the wrap-up while a steering message is still waiting: someone who
// interrupts an over-budget turn deserves a real answer from the tools they just asked for, not
// a summary. The wrap-up then arms on the following pass.
//
// It returns true when a wrap-up LLM call was requested.
func (al *AgentLoop) maybeStartToolLimitWrapUp(ts *turnState, pendingSteering int) bool {
	if ts.toolLimitWrapUpActive() {
		return false
	}
	if ts.agent.MaxIterations <= 0 || ts.currentIteration() < ts.agent.MaxIterations {
		return false
	}
	if pendingSteering > 0 {
		return false
	}
	// Nothing worth summarizing if the model never actually ran a tool. This also keeps
	// max_tool_iterations=1 from spending its only iteration on a wrap-up.
	if len(ts.toolExecutionsSnapshot()) == 0 {
		return false
	}

	ts.markToolLimitWrapUp()

	// Sub-turns have no human reader: the parent consumes ForLLM. Keep the deterministic
	// baseline and skip the extra provider round-trip.
	if ts.depth > 0 {
		logger.InfoCF("agent", "Tool limit reached in sub-turn; using baseline summary", map[string]any{
			"agent_id":   ts.agent.ID,
			"iteration":  ts.currentIteration(),
			"max":        ts.agent.MaxIterations,
			"depth":      ts.depth,
			"tool_calls": len(ts.toolExecutionsSnapshot()),
		})
		return false
	}

	requested := ts.requestGracefulInterrupt(toolLimitWrapUpHint(ts.agent.MaxIterations))
	logger.InfoCF("agent", "Tool limit reached; requesting wrap-up summary", map[string]any{
		"agent_id":   ts.agent.ID,
		"iteration":  ts.currentIteration(),
		"max":        ts.agent.MaxIterations,
		"tool_calls": len(ts.toolExecutionsSnapshot()),
		"requested":  requested,
	})
	return requested
}

// toolLimitWrapUpHint builds the instruction handed to the model for the final, tool-less call.
func toolLimitWrapUpHint(maxIterations int) string {
	return fmt.Sprintf(
		"The tool budget for this turn is exhausted (%d of %d iterations used). "+
			"Do not call tools. Summarize for the user: what you completed, "+
			"what is still unfinished, and the single best next step. Be brief and concrete.",
		maxIterations,
		maxIterations,
	)
}

// buildToolLimitSummary renders the message shown when the tool budget runs out.
//
// llmText is the model's own wrap-up (empty when the wrap-up call failed, produced nothing, or
// was skipped). The deterministic part is always present, so the message is never a dead end
// even when the provider is unavailable.
func buildToolLimitSummary(ts *turnState, llmText string) string {
	llmText = strings.TrimSpace(llmText)
	maxIterations := ts.agent.MaxIterations
	// The wrap-up call itself consumes an extra iteration, so the raw counter can exceed the
	// configured budget. Report budget consumption, not the internal call count.
	iterations := ts.currentIteration()
	if maxIterations > 0 && iterations > maxIterations {
		iterations = maxIterations
	}

	var b strings.Builder
	fmt.Fprintf(
		&b,
		"⏳ **Tool budget reached** — %d of %d iterations used.\n",
		iterations,
		maxIterations,
	)

	if llmText != "" {
		b.WriteString("\n")
		b.WriteString(llmText)
		b.WriteString("\n")
	}

	records := ts.toolExecutionsSnapshot()
	usage, truncated := aggregateToolUsage(records)
	if len(usage) > 0 {
		b.WriteString("\n**Tools used**\n")
		for _, u := range usage {
			b.WriteString("- `")
			b.WriteString(u.name)
			fmt.Fprintf(&b, "` ×%d", u.total)
			if u.failed > 0 {
				fmt.Fprintf(&b, " (%d failed)", u.failed)
			}
			b.WriteString("\n")
		}
		if truncated > 0 {
			fmt.Fprintf(&b, "- _…and %d more tool(s)_\n", truncated)
		}
	}

	if errs := collectToolErrors(records); len(errs) > 0 {
		b.WriteString("\n**Errors**\n")
		for _, e := range errs {
			b.WriteString("- ")
			b.WriteString(e)
			b.WriteString("\n")
		}
	}

	if skills := collectToolSkills(records); len(skills) > 0 {
		b.WriteString("\n**Skills used**\n- `")
		b.WriteString(strings.Join(skills, "`, `"))
		b.WriteString("`\n")
	}

	fmt.Fprintf(
		&b,
		"\n_The turn ended here. Raise `max_tool_iterations` (currently %d) in config.json "+
			"if this task needs more tool steps._\n",
		maxIterations,
	)

	return strings.TrimRight(b.String(), "\n")
}

// aggregateToolUsage counts calls per tool in first-seen order, capping the number of
// distinct rows. It returns the visible rows and how many distinct tools were hidden.
func aggregateToolUsage(records []ToolExecutionRecord) ([]toolUsage, int) {
	if len(records) == 0 {
		return nil, 0
	}

	index := make(map[string]int, len(records))
	ordered := make([]toolUsage, 0, toolLimitSummaryMaxTools)

	for _, record := range records {
		name := strings.TrimSpace(record.Name)
		if name == "" {
			continue
		}

		if at, ok := index[name]; ok {
			ordered[at].total++
			if !record.Success {
				ordered[at].failed++
			}
			continue
		}

		index[name] = len(ordered)
		usage := toolUsage{name: name, total: 1}
		if !record.Success {
			usage.failed = 1
		}
		ordered = append(ordered, usage)
	}

	if len(ordered) <= toolLimitSummaryMaxTools {
		return ordered, 0
	}

	return ordered[:toolLimitSummaryMaxTools], len(ordered) - toolLimitSummaryMaxTools
}

// collectToolErrors returns de-duplicated, single-line error summaries.
func collectToolErrors(records []ToolExecutionRecord) []string {
	if len(records) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(records))
	errs := make([]string, 0, toolLimitSummaryMaxErrors)

	for _, record := range records {
		if record.Success {
			continue
		}

		summary := singleLine(record.ErrorSummary)
		if summary == "" {
			name := strings.TrimSpace(record.Name)
			if name == "" {
				continue
			}
			summary = name + " failed"
		}
		if _, ok := seen[summary]; ok {
			continue
		}
		seen[summary] = struct{}{}
		errs = append(errs, summary)
		if len(errs) >= toolLimitSummaryMaxErrors {
			break
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// collectToolSkills returns the distinct skills touched during the turn, in call order.
func collectToolSkills(records []ToolExecutionRecord) []string {
	if len(records) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(records))
	skills := make([]string, 0, len(records))

	for _, record := range records {
		for _, skill := range record.SkillNames {
			skill = strings.TrimSpace(skill)
			if skill == "" {
				continue
			}
			if _, ok := seen[skill]; ok {
				continue
			}
			seen[skill] = struct{}{}
			skills = append(skills, skill)
		}
	}

	if len(skills) == 0 {
		return nil
	}
	return skills
}

// singleLine collapses whitespace and truncates text so an error summary renders as one line.
func singleLine(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}

	joined := strings.Join(fields, " ")
	runes := []rune(joined)
	if len(runes) <= toolLimitErrorMaxChars {
		return joined
	}
	return strings.TrimRight(string(runes[:toolLimitErrorMaxChars]), " ") + "…"
}
