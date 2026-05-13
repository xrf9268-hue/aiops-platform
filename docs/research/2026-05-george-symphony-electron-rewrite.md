<!--
Source: https://x.com/odysseus0z/status/2031850264240800131
Archived: 2026-05-13
Author: George (@odysseus0z)
-->

> **Source:** https://x.com/odysseus0z/status/2031850264240800131
> **Author:** George ([@odysseus0z](https://x.com/odysseus0z))
> **Fork:** https://github.com/odysseus0/symphony

---

# Getting Started with OpenAI Symphony

I pushed 50 tickets to Linear before bed — a tech debt rewrite of an Electron app. Woke up to 30 merged PRs. 7,000 net lines deleted. Two days later, nothing has broken.

This is [OpenAI Symphony](https://github.com/openai/symphony) — OpenAI's open-source orchestrator for Codex agents. Point it at a Linear board and it turns tickets into pull requests.

I didn't even know how to properly test an Electron app. The agents figured it out — attaching to the running app over CDP via agent-browser, validating changes end-to-end, entirely self-directed. I'm learning how to test my own app by reading their logs.

---

## Quick Start

I maintain a [fork](https://github.com/odysseus0/symphony) that's easier to get started with (changes I made listed in README). From your project repo, run:

```bash
npx skills add odysseus0/symphony -s symphony-setup -y
```

Then tell your agent: **"set up Symphony for my repo."**

For manual setup, follow the official docs manually.

---

## How It Works

**Everything happens through Linear. The board is the interface.**

- Push a ticket to **Todo** — an idle agent claims it within seconds.
- Move a ticket to **Rework** with review comments — the agent picks it back up and addresses feedback.
- Cancel a ticket — the agent stops on the next poll.
- Move something back to **Backlog** to hold it.
- Push a batch to **Todo** to dispatch.

---

## Let Your Agent Play Tech Lead

Give your agent access to Linear and hand it the big picture:

> Break this into tickets in project [slug]. Scope each ticket to one reviewable PR. Include acceptance criteria. Set blocking relationships where order matters.

Push the batch to Todo and let Symphony parallelize across everything that isn't blocked.

My 50-ticket Electron rewrite started as one conversation: "here's the tech debt, here's what I want the codebase to look like after." The agent decomposed it, I reviewed the tickets, adjusted a few, and pushed them to Todo.

---

## What Each Worker Does

Each worker gets its own workspace clone, reads the ticket, writes a plan as a Linear comment, implements, validates, and opens a PR.

**The planning step is worth watching.** Before writing code, the agent posts its plan as a Linear comment. Catch bad plans before they become bad PRs. It will check off todos as it goes, and give you a demo video at the end.

---

## Autonomous QA: A Real Example

One of my tickets asked for a `ChatDisplay` refactor — no mention of testing. The agent:

1. Attached to the running Electron app over CDP via agent-browser
2. Injected a temporary probe to force a render error
3. Verified the failure was contained
4. Clicked through recovery
5. Screenshotted both states
6. Removed the probe

End-to-end validation of a UI change, entirely self-directed.

---

## Tuning

`WORKFLOW.md` hot-reloads within a second — no restart needed. Common adjustments:

- **`agent.max_concurrent_agents`** — start at 2–3, scale up as you trust it
- **`agent.max_turns`** — turn limit per ticket. Higher for complex work, lower to cap token spend.

---

## Key Insight

> Most of what makes this effective isn't the orchestrator — it's the prompt in `WORKFLOW.md`. Symphony is plumbing: poll Linear, dispatch workers, manage slots. The prompt teaches the agent how to plan, test, handle review feedback, and constrain scope.

*A follow-up post will go deeper on the prompt.*

---

## Relation to This Project

This is a first-hand practitioner account of Symphony used at scale on a real codebase — directly relevant to `aiops-platform`'s design goals. Key takeaways for this repo:

- The `WORKFLOW.md` prompt is the real leverage point, not the orchestrator itself
- CDP-based agent-browser for Electron/web app QA is a pattern worth adopting
- Ticket-level planning comments (before coding) improve review quality and catch bad plans early
- `max_concurrent_agents` should start conservative (2–3) and scale with trust

Related docs:
- [OpenAI Symphony blog post](2026-04-27-openai-symphony-blog.md)
- [Symphony evaluation log](symphony-evaluation-log.md)
- [Symphony personal productivity research](symphony-personal-productivity.md)
