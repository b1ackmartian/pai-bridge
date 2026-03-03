# Ralph Orchestration Skill

PAI uses this document to decompose user requests into autonomous Ralph task loops dispatched via the bridge.

## Dispatching a Ralph

Output a `RALPH:` directive on its own line with a JSON payload:

```
RALPH: {"title": "Newsletter landing page", "spec_file": "/tmp/ralph-specs/r12.md", "workspace": "/tmp/my-website", "branch": "ralph/newsletter-landing", "tags": ["code", "blog"], "max_iterations": 15}
```

### Directive fields

| Field | Required | Description |
|---|---|---|
| `title` | Yes | Short label for status reporting (shown in Telegram) |
| `spec_file` | Yes | Absolute path to the spec file you wrote |
| `workspace` | No | Working directory for Claude. Repo path for code, null for non-code |
| `branch` | No | Git branch to create/checkout. Only used with workspace. Prefix with `ralph/` |
| `tags` | No | Flexible labels for filtering: `["code"]`, `["research","security"]`, `["content","blog"]` |
| `max_iterations` | No | Safety cap. Defaults to 20. Use fewer for small tasks, more for complex ones |

### Before dispatching

1. Write the spec file to `/tmp/ralph-specs/` (create dir if needed)
2. Ensure the workspace exists and is accessible
3. For code tasks: confirm the repo is clean (`git status`) before dispatching
4. Set `max_iterations` intentionally — 5 for small, 10-15 for medium, 20+ for large

## Writing Specs

The spec is the most important part. A bad spec wastes iterations. A good spec converges.

### Spec structure

```markdown
# Task: <title>

## Goal
<1-2 sentences: what success looks like>

## Context
<Background the worker needs. File paths, architecture notes, conventions, prior decisions.>

## Tasks
- [ ] Task 1: <specific, actionable item>
- [ ] Task 2: <specific, actionable item>
- [ ] Task 3: <specific, actionable item>

## Completion criteria
<How the worker knows ALL tasks are done. Be explicit.>

## Constraints
- <Any rules: don't modify X, use Y pattern, stay under Z lines>
```

### Spec principles

- **Be specific.** "Add a newsletter signup form" is bad. "Add a newsletter signup form to /app/blog/page.tsx using the existing Turnstile component, POST to /api/subscribe, match the existing card styling" is good.
- **Include file paths.** The worker has no memory of prior sessions. Tell it exactly where things are.
- **Define done.** "RALPH_COMPLETE when all checkboxes are done and `bun run build` passes" gives a clear exit signal.
- **One concern per Ralph.** Don't mix "fix the bug AND refactor the module AND add tests." Split into separate Ralphs that can run in parallel.
- **Include constraints.** The worker will over-engineer without boundaries.

### Spec examples by task type

**Code:**
```markdown
## Tasks
- [ ] Create /app/newsletter/page.tsx with signup form
- [ ] Add POST /api/subscribe endpoint
- [ ] Run `bun run build` and fix any errors
## Completion criteria
Build passes with no errors. RALPH_COMPLETE when done.
```

**Research:**
```markdown
## Tasks
- [ ] Search for competitor pricing pages (list in /tmp/ralph-7/findings.md)
- [ ] Summarize pricing models and tiers
- [ ] Write comparison matrix
## Completion criteria
All 3 files written. RALPH_COMPLETE when done.
```

**Content:**
```markdown
## Tasks
- [ ] Draft blog post in /tmp/ralph-9/draft.md following your brand voice
- [ ] Include 3 code examples
- [ ] Write frontmatter with title, description, tags
## Completion criteria
Draft complete with frontmatter. RALPH_COMPLETE when done.
```

## Signal contract

The bridge parses these from the worker's output. They MUST appear on their own line.

| Signal | When | Effect |
|---|---|---|
| `RALPH_PROGRESS: <text>` | After meaningful work in an iteration | Bridge updates DB progress field and progress file |
| `RALPH_COMPLETE: <summary>` | All spec items are done | Bridge marks task completed, sends final notification |
| `RALPH_BLOCKED: <reason>` | Cannot proceed without human input | Bridge marks task failed with reason, notifies user |
| `RALPH_ARTIFACT: <type>:<value>` | Produced an output worth tracking | Bridge records in DB artifacts JSONB. Types: `commit`, `file`, `doc`, `pr`, `report` |

These signals are baked into `buildRalphPrompt()` in `ralph.go` — if you change them here, change them there too.

## Decomposition strategy

When the user gives a multi-part request:

1. **Identify independent tasks** — things that can run in parallel with no dependencies
2. **Identify dependent tasks** — things that must run sequentially (e.g., "create the API" before "write tests for the API")
3. **Write one spec per Ralph** — each Ralph is one concern
4. **Dispatch independent Ralphs simultaneously** — output multiple RALPH: directives
5. **Queue dependent Ralphs** — dispatch the next one when you get the completion notification

### Parallelism examples

**"Build the landing page and fix the scan throttling"**
→ 2 Ralphs, parallel (different repos, no dependencies)

**"Add the API endpoint, then write tests for it"**
→ 2 Ralphs, sequential (tests depend on endpoint existing)

**"Draft 3 blog posts from the content calendar"**
→ 3 Ralphs, parallel (independent content)

## Querying Ralph status

PAI queries `pai.ralphs` directly via the database:

```sql
-- Active Ralphs
SELECT id, title, progress, iterations, max_iterations FROM pai.ralphs WHERE status = 'active';

-- Today's history
SELECT id, title, status, iterations, finished_at - started_at AS duration FROM pai.ralphs WHERE created_at > now() - interval '1 day' ORDER BY id;

-- By tag
SELECT * FROM pai.ralphs WHERE 'code' = ANY(tags);

-- Cancel a Ralph (bridge checks before next iteration)
UPDATE pai.ralphs SET status = 'cancelled' WHERE id = <N>;
```

## Cost awareness

Each iteration is a full `claude -p` invocation. On a Max plan, that's meaningful usage. Guidelines:

- Small tasks (single file edit, short research): `max_iterations: 5`
- Medium tasks (multi-file feature, content draft): `max_iterations: 10-15`
- Large tasks (full feature with tests, deep research): `max_iterations: 20`
- Never exceed 30 without explicit user approval
