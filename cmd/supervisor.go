package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/events"
	"hivemind/internal/paths"
)

// supervisorCooldown debounces on_event supervisor wakes: a burst of worker turns
// collapses into at most ~one supervisor summary per window. supervisorReport
// always re-reads the latest snapshot, so a skipped trigger loses no information.
const supervisorCooldown = 30 * time.Second

// supervisorThrottleOK reports whether enough time has passed since the last
// on_event wake, updating the cooldown marker when it returns true. This bounds
// both the per-burst fan-out and the rate of any supervisor→worker→supervisor
// feedback. Manual `hivemind report` bypasses this (it does not call here).
func supervisorThrottleOK(p paths.Project) bool {
	marker := filepath.Join(p.AgentDir(config.SupervisorName), "last-report")
	if fi, err := os.Stat(marker); err == nil && time.Since(fi.ModTime()) < supervisorCooldown {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(marker), 0o755)
	_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644)
	return true
}

// supervisorReport wakes the haiku supervisor to summarize the fleet. The
// supervisor reasons over a snapshot we hand it and replies with a digest; its
// final message is recorded to the ledger by its own Stop hook (so it never needs
// to write outside its workspace). A per-agent lock serializes supervisor turns,
// so a burst of events queues into a single in-flight summary rather than stacking
// many concurrent claude processes.
func supervisorReport(p paths.Project, cfg *config.Project, trigger string) error {
	rep := gatherStatusFor(p, cfg)
	prompt := supervisorPrompt(trigger, buildDigest(rep), events.Tail(p.EventsLog(), 12))
	return agent.Dispatch(p, config.SupervisorName, prompt, false, "")
}

// supervisorPrompt assembles the snapshot the supervisor summarizes. It is told
// to answer from the snapshot alone (no tool use) so the turn is cheap and its
// whole reply is suitable for verbatim recording in the ledger.
func supervisorPrompt(trigger, digest string, evs []events.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the hivemind supervisor. Trigger: %s.\n\n", trigger)
	b.WriteString("Current control-plane snapshot:\n\n")
	b.WriteString(digest)
	if len(evs) > 0 {
		b.WriteString("\nRecent per-turn push events (oldest first):\n")
		for _, e := range evs {
			flag := ""
			if e.Blocked {
				flag = " [BLOCKED]"
			} else if e.Errored {
				flag = " [ERROR]"
			}
			fmt.Fprintf(&b, "- %s%s: %s\n", e.Agent, flag, oneLine(e.Summary))
		}
	}
	b.WriteString("\nWrite a concise fleet digest for the human operator (3-6 lines). " +
		"Call out any BLOCKED or errored agent FIRST and say what it needs. Then give per-agent state " +
		"and tool health, and one recommended next action if warranted. Base your answer ONLY on the " +
		"snapshot above — do not run any commands. Your entire reply is recorded verbatim in the ledger.")
	return b.String()
}

// appendLedger writes a timestamped section to .hivemind/ledger.md.
func appendLedger(p paths.Project, text string) error {
	f, err := os.OpenFile(p.Ledger(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n### %s\n%s\n", time.Now().Format("2006-01-02 15:04:05"), strings.TrimSpace(text))
	return err
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
