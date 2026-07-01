package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"hivemind/internal/agent"
	"hivemind/internal/config"
	"hivemind/internal/paths"
	"hivemind/internal/runner"
	"hivemind/internal/scaffold"
	"hivemind/internal/session"
)

// shellMetaRe matches characters unsafe in a filename that becomes part of an
// inferred entrypoint (which is turned into a `Bash(<entrypoint>:*)` allow-rule).
var shellMetaRe = regexp.MustCompile("[;|&$`<>(){}\\[\\]!*?~\\\\\"' \t\n]")

// validateToolSource checks the source exists, is a file, and has a basename that's
// safe to embed in an inferred entrypoint / Bash allow-rule.
func validateToolSource(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("tool file: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("%q is a directory, not a tool file", path)
	}
	if base := filepath.Base(path); shellMetaRe.MatchString(base) {
		return fmt.Errorf("unsafe tool filename %q — rename it to remove spaces/shell metacharacters", base)
	}
	return nil
}

// toolDocPromptTmpl asks the model to read a tool's source and write its TOOL.md.
// Args: tool name, user description, source basename, source code.
const toolDocPromptTmpl = `You are writing the documentation file (TOOL.md) for a command-line tool that an autonomous coding agent will use as one of its tools. The agent will read this doc to learn how and when to run the tool.

Write a clear, concise TOOL.md in GitHub-flavored Markdown that covers:
- what the tool does (1–2 sentences),
- exactly how to invoke it (the command, plus any flags/arguments/subcommands it actually supports),
- its inputs and outputs,
- 1–2 short, realistic usage examples.

Be strictly accurate to the source below — do NOT invent flags or behavior that isn't in the code. Keep it practical for an agent that runs it via Bash. Output ONLY the Markdown document, starting with a "# %s" heading. No preamble and no surrounding code fence.

The user describes the tool as: %s

Tool source (%s):
----------------------------------------
%s
----------------------------------------`

// generateToolDoc reads a tool's source and asks Claude to write its TOOL.md,
// returning the Markdown. Under --fake (or when reading the file fails) it returns
// a deterministic stub so the flow stays testable without the real CLI.
func generateToolDoc(p paths.Project, cfg *config.Project, name, about, srcPath, model string) (string, error) {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read tool file: %w", err)
	}
	if flagFake {
		return fakeToolDoc(name, about, srcPath), nil
	}
	if err := pickRunner().Available(); err != nil {
		return "", fmt.Errorf("claude CLI not available: %w — set HIVEMIND_CLAUDE_BIN, or use --fake for a stub doc", err)
	}
	code := string(src)
	if len(code) > 16000 {
		code = code[:16000] + "\n…(source truncated)…\n"
	}
	if model == "" {
		if model = cfg.Defaults.Model; model == "" {
			model = "sonnet"
		}
	}
	tgt, err := agent.ResolveTarget(p, cfg, config.SupervisorName)
	if err != nil {
		return "", err
	}
	spec := runner.PromptSpec{
		Agent:     config.SupervisorName,
		SessionID: session.NewID(), // throwaway, ephemeral planning session
		Workspace: tgt.Workspace,
		Model:     model,
		Prompt:    fmt.Sprintf(toolDocPromptTmpl, name, strings.TrimSpace(about), filepath.Base(srcPath), code),
	}
	doc, err := runner.CaptureResult(spec)
	if err != nil {
		return "", fmt.Errorf("doc generation via claude failed: %w", err)
	}
	return strings.TrimSpace(doc) + "\n", nil
}

func fakeToolDoc(name, about, srcPath string) string {
	return fmt.Sprintf("# %s\n\n%s\n\n## Usage\n\n```sh\n%s\n```\n\n_(generated in --fake mode — run without --fake for a real, AI-written doc)_\n",
		name, strings.TrimSpace(about), inferToolEntrypoint(srcPath))
}

// inferToolEntrypoint guesses how to run a dropped script from its extension.
func inferToolEntrypoint(path string) string {
	b := filepath.Base(path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python3 " + b
	case ".sh":
		return "bash " + b
	case ".js":
		return "node " + b
	case ".ts":
		return "npx ts-node " + b
	case ".rb":
		return "ruby " + b
	case ".pl":
		return "perl " + b
	default:
		return "./" + b
	}
}

// toolNameFromPath derives a safe tool name from a file path.
func toolNameFromPath(path string) string {
	n := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// newToolGendocCmd generates and prints a TOOL.md for a script (no registration).
// Used to preview the AI-written doc (the console captures this output).
func newToolGendocCmd() *cobra.Command {
	var about, model string
	c := &cobra.Command{
		Use:   "gendoc <tool-file>",
		Short: "Generate a TOOL.md from a tool's source via Claude and print it (no registration)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			if err := validateToolSource(args[0]); err != nil {
				return err
			}
			doc, err := generateToolDoc(p, cfg, toolNameFromPath(args[0]), about, args[0], model)
			if err != nil {
				return err
			}
			fmt.Print(doc)
			return nil
		},
	}
	c.Flags().StringVar(&about, "about", "", "one-line description of what the tool does")
	c.Flags().StringVar(&model, "model", "", "model to generate with (default: project default)")
	return c
}

// newToolRegisterCmd registers a tool from source: Claude reads it and writes the
// TOOL.md, the user approves, then it's registered (and optionally attached).
func newToolRegisterCmd() *cobra.Command {
	var file, about, model, typ, entrypoint, docFile, attachTo string
	var yes, force bool
	c := &cobra.Command{
		Use:   "register <name> --file <path>",
		Short: "Register a tool from source: Claude writes its TOOL.md, you approve, it's attached",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, cfg, err := openProject()
			if err != nil {
				return err
			}
			name := args[0]
			if file == "" {
				return fmt.Errorf("--file <path> is required (the tool's script/source)")
			}
			if err := validateToolSource(file); err != nil {
				return err
			}
			defer lockConfig(p)()
			if cfg, err = config.Load(p.ConfigPath()); err != nil { // re-read under the lock
				return err
			}
			existing := cfg.FindTool(name)
			if existing != nil && !force {
				return fmt.Errorf("tool %q already exists — re-run with --force to update its docs (the console's /toolgen updates it automatically)", name)
			}
			var doc string
			if docFile != "" {
				b, err := os.ReadFile(docFile)
				if err != nil {
					return fmt.Errorf("read --doc-file: %w", err)
				}
				doc = string(b)
			} else {
				doc, err = generateToolDoc(p, cfg, name, about, file, model)
				if err != nil {
					return err
				}
				if !yes {
					fmt.Println("\n──────── generated TOOL.md ────────")
					fmt.Println(doc)
					fmt.Println("───────────────────────────────────")
					verb := "Register"
					if existing != nil {
						verb = "Update"
					}
					if !confirm(verb + " the tool with this documentation?") {
						fmt.Println("aborted.")
						return nil
					}
				}
			}
			if entrypoint == "" && typ != config.ToolLibrary {
				entrypoint = inferToolEntrypoint(file)
			}
			if existing != nil {
				// Update in place: refresh the type/entrypoint and rewrite TOOL.md +
				// the dropped source, then re-attach (so re-running /toolgen is safe).
				existing.Type, existing.Entrypoint = typ, entrypoint
				if typ == config.ToolLibrary {
					existing.Path = filepath.Base(file)
				}
				if err := scaffold.WriteToolFiles(p, *existing, doc, file); err != nil {
					return err
				}
				if err := config.Save(p.ConfigPath(), cfg); err != nil {
					return err
				}
				fmt.Printf("updated tool %q\n", name)
			} else {
				t := config.Tool{Name: name, Type: typ, Entrypoint: entrypoint}
				if typ == config.ToolLibrary {
					t.Path = filepath.Base(file)
				}
				if err := scaffold.RegisterTool(p, cfg, t, file, doc); err != nil {
					return err
				}
				fmt.Printf("registered tool %q (%s)\n", name, typ)
			}
			if attachTo != "" {
				if err := scaffold.AttachTool(p, cfg, name, attachTo); err != nil {
					return err
				}
				fmt.Printf("attached to %s (CLAUDE.md regenerated)\n", attachTo)
			}
			return nil
		},
	}
	c.Flags().StringVar(&file, "file", "", "path to the tool's script/source (required)")
	c.Flags().StringVar(&about, "about", "", "one-line description (guides the AI-written doc)")
	c.Flags().StringVar(&docFile, "doc-file", "", "use this TOOL.md verbatim instead of generating one")
	c.Flags().StringVar(&typ, "type", "command", "service|command|library")
	c.Flags().StringVar(&entrypoint, "entrypoint", "", "how to run it (inferred from the file if omitted)")
	c.Flags().StringVar(&model, "model", "", "model to generate the doc with")
	c.Flags().StringVar(&attachTo, "agent", "", "attach the tool to this agent")
	c.Flags().BoolVar(&yes, "yes", false, "skip the approval prompt")
	c.Flags().BoolVar(&force, "force", false, "update the tool if it already exists (instead of erroring)")
	return c
}
