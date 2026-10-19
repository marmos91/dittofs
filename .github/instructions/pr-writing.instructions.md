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
- **question** — you need information, not a change.

Rules:

- Prefer GitHub `suggestion` blocks when the fix is local to the commented lines, so the author can apply it in one click.
- Do not repeat the same comment on every occurrence: comment once and say "same in X and Y".
- The overall review body starts with the verdict, then the points that matter most, in order of importance. A reader who skips the inline comments must still know whether the PR can merge and what blocks it. Do not repeat what the inline comments already say.
- If a point needs more than a short paragraph to argue, or the thread goes past two rounds, propose a quick live review instead of writing more.

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
