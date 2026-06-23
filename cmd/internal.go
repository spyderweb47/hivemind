package cmd

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/events"
	"hivemind/internal/paths"
	"hivemind/internal/runner"
	"hivemind/internal/session"
	"hivemind/internal/tasks"
	"hivemind/internal/transcript"
)

func nonneg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// taskStatusFor maps a finished turn's event to a task terminal status.
func taskStatusFor(ev events.Event) string {
	switch {
	case ev.Blocked:
		return tasks.Blocked
	case ev.Errored:
		return tasks.Failed
	default:
		return tasks.Done
	}
}

// __turn is the hidden command launched (detached) by `send` / the dashboard. It
// runs exactly one lock-serialized prompt turn for an agent, then exits. Keeping
// the turn body here means the per-agent lock lives in one place.
func newTurnCmd() *cobra.Command {
	var agentName, root, promptFile, taskID string
	var fake bool
	c := &cobra.Command{
		Use:    "__turn",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := paths.NewProject(root)
			cfg, err := config.Load(p.ConfigPath())
			if err != nil {
				return err
			}
			b, err := os.ReadFile(promptFile)
			if err != nil {
				return err
			}
			r := runner.Select(fake)
			err = agent.RunTurn(p, cfg, r, agentName, string(b), taskID)
			_ = os.Remove(promptFile)
			// The fake runner doesn't fire Stop hooks, so complete its task here.
			if err == nil && taskID != "" && fake {
				_ = tasks.SetStatus(p, taskID, tasks.Done)
			}
			return err
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent name")
	c.Flags().StringVar(&root, "root", "", "project root")
	c.Flags().StringVar(&promptFile, "prompt-file", "", "file containing the prompt")
	c.Flags().BoolVar(&fake, "fake", false, "use the fake runner")
	c.Flags().StringVar(&taskID, "task-id", "", "task id this turn fulfills")
	return c
}

// stopPayload is the JSON Claude Code passes on the Stop hook's stdin.
type stopPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	Cwd                  string `json:"cwd"`
	LastAssistantMessage string `json:"last_assistant_message"`
	HookEventName        string `json:"hook_event_name"`
}

// __hook-stop is invoked by each agent's Stop hook after every turn (this fires
// under headless `claude -p`). It parses the hook payload + the transcript and
// appends a structured push Event to the global and per-agent logs. It is
// best-effort and must never error (that would surface as a hook failure to the
// agent), so it always returns nil.
func newHookStopCmd() *cobra.Command {
	var agentName, root string
	c := &cobra.Command{
		Use:    "__hook-stop",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _ := io.ReadAll(os.Stdin)
			var pl stopPayload
			_ = json.Unmarshal(raw, &pl)

			p := paths.NewProject(root)
			cfg, err := config.Load(p.ConfigPath())
			if err != nil {
				return nil
			}

			// Resolve the transcript: prefer the path the hook handed us.
			tpath := pl.TranscriptPath
			if tpath == "" {
				if sid, e := session.ReadID(p.AgentSessionFile(agentName)); e == nil {
					tpath, _ = transcript.Locate(paths.ClaudeProjectsDir(), sid)
				}
			}
			var sum transcript.Summary
			if tpath != "" {
				sum, _ = transcript.Parse(tpath)
			}

			model := ""
			if tgt, e := agent.ResolveTarget(p, cfg, agentName); e == nil {
				model = tgt.Model
			}

			// transcript usage is cumulative for the session; record both the
			// cumulative total and the per-turn delta vs. the previous event so the
			// feed shows what THIS turn cost, not the running total.
			cumTokens := sum.TotalTokens()
			cumIn := sum.InputTokens + sum.CacheRead + sum.CacheCreate
			cumOut := sum.OutputTokens
			cumCost := agent.EstimateCost(model, sum)
			dTokens, dIn, dOut, dCost := cumTokens, cumIn, cumOut, cumCost
			// Only diff against the previous event when it's from the SAME session
			// (a fresh/reset session legitimately reports cumulative-as-delta).
			if prev, ok := events.LatestAny(p.AgentEvents(agentName)); ok && prev.SessionID == pl.SessionID {
				dTokens = nonneg(cumTokens - prev.CumTokens)
				dIn = nonneg(cumIn - prev.CumIn)
				dOut = nonneg(cumOut - prev.CumOut)
				if dCost = cumCost - prev.CumCost; dCost < 0 {
					dCost = 0
				}
			}

			ev := events.Event{
				Kind:      events.KindStop,
				Agent:     agentName,
				TS:        time.Now().UTC(),
				SessionID: pl.SessionID,
				Summary:   clip(pl.LastAssistantMessage, 240),
				LastTool:  sum.LastTool,
				Tokens:    dTokens,
				InTokens:  dIn,
				OutTokens: dOut,
				CostUSD:   dCost,
				CumTokens: cumTokens,
				CumIn:     cumIn,
				CumOut:    cumOut,
				CumCost:   cumCost,
				Blocked:   blockedPrefix(pl.LastAssistantMessage) || sum.NeedsInput,
				Errored:   sum.Errored,
			}
			_ = events.Append(p.EventsLog(), ev)
			_ = events.Append(p.AgentEvents(agentName), ev)

			if agentName == config.SupervisorName {
				// The supervisor's reply IS the digest — record it to the ledger.
				if msg := strings.TrimSpace(pl.LastAssistantMessage); msg != "" {
					_ = appendLedger(p, msg)
				}
			} else {
				// Complete exactly the task this turn ran (the marker RunTurn stamped
				// under the lock). A plain `send` leaves no marker → completes nothing.
				if b, e := os.ReadFile(p.AgentTurnTask(agentName)); e == nil {
					if id := strings.TrimSpace(string(b)); id != "" {
						_ = tasks.SetStatus(p, id, taskStatusFor(ev))
					}
					_ = os.Remove(p.AgentTurnTask(agentName))
				}
				if cfg.Supervisor.Report.OnEvent && supervisorThrottleOK(p) {
					// Wake the supervisor to summarize, debounced so a burst of worker
					// turns collapses into ~one summary per cooldown window.
					_ = supervisorReport(p, cfg, "event:"+agentName)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent name")
	c.Flags().StringVar(&root, "root", "", "project root")
	return c
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func blockedPrefix(s string) bool {
	lt := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(lt, "blocked:") || strings.HasPrefix(lt, "[blocked]")
}
