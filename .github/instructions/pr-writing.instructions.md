---
applyTo: "**"
---

# Writing PRs, review comments and replies

Everything you write on a PR will be read by a person who is also reviewing other PRs today. Long, multi-point walls of text get skimmed or ignored, and information gets lost. Your job is to save the reader's time, not to look thorough.

## Ground rules (apply to everything below)

- **Lead with the point.** First sentence says what matters. No preamble, no restating the task, the code or the comment you are answering.
- **Code over prose.** If a fix or alternative can be shown, show it as a snippet instead of describing it.
- **Short beats complete.** Cut anything the reader can see in the diff, anything a linter/formatter/CI would catch, and anything you are not confident about.
- **Be honest about uncertainty.** If you are guessing, say "I think" or ask a question. Never invent behavior, benchmarks or references.
- **A human owns the output.** Unless explicitly told to post, return drafts to the user. The person posting must have read every line and be able to defend it. Write so that this is easy: short enough to read in under a minute.

## PR title and description

Follow [`.github/pull_request_template.md`](../pull_request_template.md), including the guidance in its comments.

## Review comments

Post comments as part of one review, not one at a time: the author gets one notification for the review instead of one per comment. Inside that review, choose how many threads to open:

- **One thread per specific objection.** A bug, a design concern or a question each gets its own thread, so it can be discussed and resolved on its own. A design point that touches four separate aspects is four threads.
- **One thread for the minor points.** Past 3–4 nits or small suggestions, collect them as a list in a single comment, so they do not bury the threads that matter.

Where to anchor a comment:

- **On a line** only when the point is about that line. Line comments go outdated and get hidden when the line changes.
- **On the file** (the comment icon in the file header) for anything broader, such as a design point. File comments survive pushes.
- **On the most relevant file** for a point about the PR as a whole, or on the first file if none stands out.

Format:

```
**<severity>:** <the problem, one sentence>. <why it matters, only if not obvious>.

<code showing the fix>
```

Severity tags:

- **blocking** — must change before merge (bug, data loss, security, broken contract).
- **suggestion** — would improve it, author decides.
- **nit** — style/naming, never blocks.
- **question** — you need information, not a change. Ask whenever the intent is unclear rather than guessing it.

Rules:

- Prefer GitHub `suggestion` blocks when the fix is local to the commented lines, so the author can apply it in one click.
- Link the RFC, spec section or doc that backs a point instead of re-explaining it.
- Do not repeat the same comment on every occurrence: comment once and say "same in X and Y".
- If a point needs more than a short paragraph to argue, or the thread goes past two rounds, propose a quick live review instead of writing more.

## Review body

The review state is the verdict: **Request changes** if any comment is blocking, **Approve** if none is, **Comment** only when you can't judge. Nobody should have to dig the verdict out of the text.

A reader who skips the inline comments must still know whether the PR can merge and what blocks it. Write the body in this order, and skip any part that would be empty:

1. **Summary**: the first sentence names what blocks the merge, or says nothing does.
2. **Critical issues**: what blocks the merge (correctness, security, breaking changes).
3. **Suggestions**: improvements to performance or maintainability.
4. **Questions**: what you need to know before you can judge.
5. **Praise**: what was done well, named specifically enough that nobody changes it later by mistake.

Order points by importance: correctness, then performance, then style. In parts 2–4, give each point one line that names its thread; the detail lives in the inline comment, so do not repeat it here.

## What to check

Use this list to guide the review. Do not paste it into the review: report only what fails, as inline comments.

- **Protocol implementation**: encoding/decoding follows the specifications; error codes map correctly; authentication and authorization are correct; behavior compared with reference implementations.
- **Architecture**: protocol and business logic stay separate; store interfaces used correctly; two-phase write pattern followed where needed.
- **Code quality**: no data races or deadlocks; context cancellation respected; proper error handling with structured errors; thread safety ensured.
- **Performance**: no unnecessary allocations in hot paths; buffer pooling used appropriately; efficient algorithms for common operations.
- **Security**: input validated; permission checks at the correct layer; no sensitive data logged.
- **Testing**: new code covered by unit tests; integration/E2E tests for protocol changes; error paths tested.
- **Documentation**: public APIs commented; user documentation updated; configuration examples provided.

### Example

Bad:

> I noticed that in this function there might be a potential issue related to error handling. Specifically, when the write operation fails, the error is being logged but not returned to the caller, which could potentially lead to situations where the caller assumes the operation succeeded. This could have several implications: 1. Data consistency... 2. Observability... 3. ... It would be beneficial to consider propagating the error so that callers can handle it appropriately. Additionally, you might want to wrap the error with context.

Good:

> **blocking:** the write error is logged but swallowed, so the caller treats a failed flush as success.
>
> ```suggestion
> if err := j.write(entry); err != nil {
>     return fmt.Errorf("journal flush: %w", err)
> }
> ```

## Replying to review threads

- If you applied the change: `Done in <short sha>.` Nothing else.
- If you did not: one or two sentences on why, plus a link if relevant. If you still disagree after one round, suggest talking live.
