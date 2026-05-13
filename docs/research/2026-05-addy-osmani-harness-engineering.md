<!--
Source: https://x.com/addyosmani/status/2053231239721885918
Archived: 2026-05-13
Author: Addy Osmani (@addyosmani)
Published: 2026-05-13
-->

> **Source:** https://x.com/addyosmani/status/2053231239721885918
> **Author:** Addy Osmani ([@addyosmani](https://x.com/addyosmani))
> **Published:** May 13, 2026

---

# Agent Harness Engineering

A coding agent is the model plus everything built around it. Harness engineering treats that scaffolding as a living artifact, tightening it every time the agent makes a mistake.

Simply put: whenever an agent fails, you engineer a permanent solution so it never makes that exact mistake again.

For the last two years, the industry has debated models: which is the smartest, which writes the cleanest React, or which hallucinates the least. While that conversation matters, it misses the other half of the system.

The model is merely one input into a running agent. The rest is the harness: the prompts, tools, context policies, hooks, sandboxes, subagents, feedback loops, and recovery paths wrapped around the model so it can actually complete tasks.

**A decent model with a great harness consistently beats a great model with a bad harness.** Increasingly, the most interesting engineering work isn't in selecting the model, but in designing the scaffolding around it.

That discipline now has a name.

---

## Defining the Harness

Trivedy's core definition does most of the heavy lifting:

> Agent = Model + Harness. If you're not the model, you're the harness.

A harness encompasses every piece of code, configuration, and execution logic that isn't the model itself. A raw model is not an agent. It only becomes one when a harness provides it with state, tool execution, feedback loops, and enforceable constraints.

Concretely, a harness includes:

- System prompts, CLAUDE.md, AGENTS.md, skill files, and subagent instructions.
- Tools, skills, MCP servers, and their technical descriptions.
- Bundled infrastructure, such as the filesystem, sandboxes, and headless browsers.
- Orchestration logic for spawning subagents, handling handoffs, and routing models.
- Hooks and middleware for deterministic execution, like lint checks or context compaction.
- Observability tools for logs, traces, cost, and latency metering.

At its core, an agent is a system that runs tools in a loop to achieve a goal. The real skill lies in designing both the tools and that loop.

While this represents a massive surface area, it is your surface area, not the model provider's. Claude Code, Cursor, Codex, Aider, and Cline are all harnesses. The underlying model might be identical across platforms, but the behavior you experience is dominated by the harness.

---

## Failures Are Configuration Problems

It is common to see engineers blame the model when an agent does something nonsensical, often filing the problem away as something to "wait for the next version" to fix.

The harness-engineering mindset rejects this default. Failures are usually somewhat legible. If the agent ignored a convention, add it to AGENTS.md. If it ran a destructive command, write a hook to block it. If it got lost in a 40-step task, split the architecture into a planner and an executor. If it consistently finishes with broken code, wire a type-checking back-pressure signal into the loop.

As HumanLayer puts it: **"It's not a model problem. It's a configuration problem."**

Consider performance benchmarks: a leading model running inside an off-the-shelf framework often scores drastically lower than the exact same model running in a custom, highly-tuned harness. Moving a model into an environment with better codebase tools, tighter prompts, and sharper back-pressure can unlock capabilities the original setup left behind.

**The gap between what today's models can theoretically do and what you actually see them doing is largely a harness gap.**

---

## Mistakes as Permanent Signals

The most vital habit in harness engineering is treating agent mistakes as permanent signals — not one-off flukes to retry and forget.

If an agent ships a PR with a commented-out test that gets merged by accident, that is an input. The next iteration of AGENTS.md must state: "Never comment out tests; delete or fix them." The next pre-commit hook should automatically flag `.skip(` in the diff. The reviewer subagent must be updated to block commented-out tests.

Constraints should only be added when you observe a real failure, and removed only when a capable model renders them redundant. **Every line in a good system prompt should trace back to a specific, historical failure.**

Because of this, harness engineering is a discipline rather than a one-size-fits-all framework. The right harness for a specific codebase is entirely shaped by its unique failure history.

---

## Harness Design: Behavior First

The most effective way to design a harness is to start with the desired behavior and build the component that delivers it:

> Behavior we want → Harness design to achieve it.

Every piece of the harness must have a distinct job. If you cannot name the specific behavior a component exists to deliver, it should be removed.

---

## Core Harness Components

### Filesystem and Git — Durable State

The filesystem is foundational. Models can only operate on what fits in their context window. A filesystem provides a workspace to read data, a place to offload intermediate work, and a surface for multiple agents to coordinate.

Adding Git provides free versioning, allowing the agent to track progress, branch experiments, and roll back errors.

### Bash and Code Execution — General-Purpose Tooling

Most agents operate on a ReAct loop: reason, act via a tool call, observe, repeat. Instead of pre-building a tool for every conceivable action, giving the agent bash access allows it to build what it needs on the fly.

Agents generally excel at shell commands, making bash and code execution the default strategy for autonomous problem-solving.

### Sandboxes and Default Tooling

Bash is only useful if it runs safely. Sandboxes provide agents with an isolated environment to run code, inspect files, and verify work without risking the host machine.

A good sandbox ships with strong defaults: pre-installed language runtimes, test CLIs, and headless browsers, allowing the agent to observe its own work and close the self-verification loop.

### Memory and Search — Continual Learning

Models have no knowledge beyond their training weights and current context. Harnesses bridge this gap using memory files (like AGENTS.md) that inject knowledge into every session.

For real-time information like new library versions or live data, web search and MCP tools are baked directly into the harness.

### Battling Context Rot

Models degrade in reasoning as their context windows fill up. Harnesses manage this scarcity using three primary techniques:

- **Compaction:** Intelligently summarizing and offloading older context to prevent API errors.
- **Tool-call offloading:** Storing massive tool outputs (like 2,000-line logs) in the filesystem while keeping only the essential headers and footers in context.
- **Progressive disclosure:** Revealing instructions and tools only when a task explicitly requires them, rather than loading everything at startup.

### Long-Horizon Execution

Autonomous, long-running work suffers from early stopping and poor problem decomposition. Harnesses counter this through structural design:

- **Loops:** Intercepting a model's attempt to exit and forcing it to continue against a completion goal in a fresh context window.
- **Planning:** Forcing the model to decompose goals into a step-by-step plan file, checking its work via self-verification hooks after each step.
- **Splits:** Separating generation and evaluation into distinct agents, preventing the inherent positive bias models have when grading their own work.

### Hooks — Your Enforcement Layer

Hooks bridge the gap between requesting an action and enforcing it. They run at specific lifecycles: before a tool call, after a file edit, or before a commit. Hooks block destructive commands, enforce auto-formatting to save tokens, and run test suites.

Ideally, success is silent, and failures are verbose. If a typecheck passes, the agent hears nothing; if it fails, the error is injected directly back into the loop for self-correction.

### The Rulebook and Tool Choice

A flat markdown file at the root of a repository is still the highest-leverage configuration point. However, it must be treated like a pilot's checklist, not a style guide. Keep it short, and ensure every rule is earned through a past failure.

The same discipline applies to tools. **Ten highly focused tools will always outperform fifty overlapping ones.**

Furthermore, because tool descriptions populate the prompt, malicious or sloppy external integrations (like unverified MCP servers) can inject bad prompts into the agent before it even starts working.

---

## What This Looks Like in Production

The clearest public picture of a mature harness is the breakdown of Claude Code's architecture — almost every concept above shows up as a named component:

- Context injection = the knowledge layer
- Loop state = the memory store and the worktree isolator
- Destructive-action hooks = the permission gate
- Subagent context firewalls = the entire multi-agent layer
- Tool dispatch registry = where MCP servers and bash both plug in

Claude Code's trajectory is about the harness at least as much as about the model underneath it.

---

## As Models Improve, Harnesses Shift — Not Disappear

It is tempting to assume better models make scaffolding obsolete. For instance, recent model upgrades drastically reduced the need for "context-anxiety" mitigations. But as the floor raises, so does the ceiling. Tasks that were previously unreachable are now in play, bringing entirely new failure modes.

Every component in a harness encodes an assumption about what the model cannot do on its own. When the model improves, outdated scaffolding should be removed, and new scaffolding must be built to reach the next horizon.

---

## The Training Loop Feedback

There is an active feedback loop between harness design and model training.

Today's models are often post-trained with specific harnesses in the loop, creating a degree of overfitting. The model gets exceptionally good at the specific actions the harness designers prioritized (e.g., filesystem ops, bash, subagent dispatch).

This makes the harness a living system, not a static config file, and proves that the "best" harness is the one optimized specifically for your distinct tasks and workflows.

---

## The Industry Shift: From LLM APIs to Harness APIs

The industry is shifting from building on LLM APIs (which provide completions) to building on Harness APIs (which provide a runtime). SDKs now offer the loop, tools, context management, hooks, and sandboxes right out of the box.

Instead of building orchestration from scratch, the modern default is to select a harness framework, configure its core pillars, and focus purely on domain-specific prompt and tool design.

This is what makes troubleshooting scalable: you are tuning a well-factored configuration surface rather than reinventing the entire agent architecture.

**If you look at the top coding agents today, they look more like each other than their underlying models do.** The models differ, but the harness patterns are converging. The industry is rapidly identifying the load-bearing scaffolding required to turn generative text into shippable software.

---

## Open Problems

The most exciting open problems are moving beyond single agents:

- Orchestrating multiple agents in parallel
- Enabling agents to analyze their own traces to fix harness-level failures
- Building environments that dynamically assemble tools just-in-time

Ultimately, this is the phase where harnesses stop being static configuration files and start acting much more like **compilers**.

---

## Relation to This Project

This post is directly applicable to `aiops-platform` — which is itself a harness (runner + orchestrator + WORKFLOW.md) wrapping Codex/Claude. Key takeaways:

- **AGENTS.md / WORKFLOW.md** should be treated as a pilot's checklist, not a style guide — every rule must trace back to a real observed failure
- **Hooks** (pre-commit, lint, type-check) are the enforcement layer; silence on success, verbose on failure, injected back into the loop
- **Context rot** mitigation (compaction, tool-call offloading, progressive disclosure) is worth building into the runner
- **Planner/executor split** for complex tasks prevents self-grading bias
- **Ten focused tools > fifty overlapping ones** — applies directly to MCP server selection and `WORKFLOW.md` tool definitions
- As Codex improves, remove outdated scaffolding constraints and raise the ceiling — harness is a living artifact

Related docs:
- [OpenAI Symphony blog post](2026-04-27-openai-symphony-blog.md)
- [George's Symphony Electron rewrite thread](2026-05-george-symphony-electron-rewrite.md)
- [Symphony evaluation log](symphony-evaluation-log.md)
