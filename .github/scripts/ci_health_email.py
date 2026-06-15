#!/usr/bin/env python3
"""Build the develop-red alarm email (HTML + text), subject, and issue markdown.

Reads run facts from the environment and pre-collected failure data from /tmp:
  - /tmp/failed_jobs.ndjson  one JSON object per failed job: {name, url, step}
  - /tmp/failing_tests.txt   best-effort failing test names, one per line

Writes (consumed by ci-health.yml):
  /tmp/mail_subject, /tmp/mail.html, /tmp/mail.txt, /tmp/issue.md

Best-effort by design: missing/empty inputs degrade gracefully to a
links-only message, never an error — the alarm must always send.
"""
import html
import json
import os
import pathlib

CAP = 20  # max test names listed inline; the rest live in the logs

W = os.environ.get("WORKFLOW", "conformance")
C = os.environ.get("CONCLUSION", "failure")
SHA = os.environ.get("HEAD_SHA", "")
SHORT = SHA[:8] if SHA else "unknown"
RUN_URL = os.environ.get("RUN_URL", "")
REPO = os.environ.get("REPO", "")
ARTIFACTS_URL = f"{RUN_URL}#artifacts" if RUN_URL else ""
COMMIT_URL = f"https://github.com/{REPO}/commit/{SHA}" if REPO and SHA else RUN_URL


def _read_jobs(path):
    jobs = []
    try:
        for line in pathlib.Path(path).read_text().splitlines():
            line = line.strip()
            if line:
                jobs.append(json.loads(line))
    except (FileNotFoundError, json.JSONDecodeError):
        pass
    return jobs


def _read_lines(path):
    try:
        return [x for x in pathlib.Path(path).read_text().splitlines() if x.strip()]
    except FileNotFoundError:
        return []


jobs = _read_jobs("/tmp/failed_jobs.ndjson")
tests = _read_lines("/tmp/failing_tests.txt")

subject = f"\U0001F534 [DittoFS] develop RED — {len(jobs)} failed job(s) in {W} ({SHORT})"

# --- failed jobs (HTML + text) ---
jobs_html, jobs_text = [], []
for j in jobs:
    name = j.get("name", "?")
    url = j.get("url") or RUN_URL
    step = j.get("step") or ""
    step_html = f" — failed step: <code>{html.escape(step)}</code>" if step else ""
    jobs_html.append(
        f'<li>✗ <b>{html.escape(name)}</b>{step_html} '
        f'&nbsp;<a href="{url}">▶ logs</a></li>'
    )
    jobs_text.append(
        f"  ✗ {name}" + (f" (failed: {step})" if step else "") + f"\n     {url}"
    )
if not jobs:
    jobs_html.append(f'<li>See <a href="{RUN_URL}">the run</a> for failed jobs.</li>')
    jobs_text.append(f"  see {RUN_URL}")

# --- failing tests (HTML + text) ---
if tests:
    shown = tests[:CAP]
    tests_html = (
        "<p><b>Detected failing tests</b> (best-effort — logs are authoritative):</p><ul>"
        + "".join(f"<li><code>{html.escape(t)}</code></li>" for t in shown)
        + "</ul>"
    )
    tests_text = "Detected failing tests (best-effort):\n" + "\n".join(f"  • {t}" for t in shown)
    if len(tests) > CAP:
        more = len(tests) - CAP
        tests_html += f"<p>… and {more} more — see logs.</p>"
        tests_text += f"\n  … and {more} more"
else:
    tests_html = "<p>Test names not auto-extracted — open the job logs above.</p>"
    tests_text = "Test names not auto-extracted — open the job logs."

html_body = f"""<h2>\U0001F534 develop CI failed — {html.escape(W)}</h2>
<table cellpadding="4" style="border-collapse:collapse">
<tr><td><b>Branch</b></td><td>develop</td></tr>
<tr><td><b>Conclusion</b></td><td>{html.escape(C)}</td></tr>
<tr><td><b>Commit</b></td><td><a href="{COMMIT_URL}">{SHORT}</a></td></tr>
</table>
<h3>Failed jobs &amp; tests</h3>
<ul>{''.join(jobs_html)}</ul>
{tests_html}
<h3>Quick links</h3>
<ul>
<li><a href="{RUN_URL}">▶ Full run (all jobs)</a></li>
<li><a href="{ARTIFACTS_URL}">▶ Download logs / artifacts</a></li>
<li><a href="{COMMIT_URL}">▶ Commit &amp; diff</a></li>
</ul>
<p>develop is blocked-by-convention until green; open PRs show a warning banner.
Fix forward, or revert <code>{SHORT}</code> — re-running the failed workflow re-fires this check.</p>"""

text_body = f"""develop CI failed — {W}

Branch:     develop
Conclusion: {C}
Commit:     {SHORT}  {COMMIT_URL}

Failed jobs:
{chr(10).join(jobs_text)}

{tests_text}

Links:
  Full run:  {RUN_URL}
  Artifacts: {ARTIFACTS_URL}
  Commit:    {COMMIT_URL}

develop blocked-by-convention until green; open PRs show a warning.
Fix forward or revert {SHORT}."""

issue_jobs = "\n".join(
    f"- ✗ {j.get('name', '?')}"
    + (f" — failed step: `{j.get('step')}`" if j.get("step") else "")
    + f" · [logs]({j.get('url') or RUN_URL})"
    for j in jobs
) or f"- see [the run]({RUN_URL})"
issue_tests = ""
if tests:
    issue_tests = "\n\n**Detected failing tests:** " + ", ".join(f"`{t}`" for t in tests[:CAP])
    if len(tests) > CAP:
        issue_tests += f" … +{len(tests) - CAP} more"
issue_md = (
    f"**{W}** concluded `{C}` on `develop` at `{SHORT}`.\n\n"
    f"**Failed jobs ({len(jobs)}):**\n{issue_jobs}{issue_tests}\n\n"
    f"Links: [run]({RUN_URL}) · [artifacts]({ARTIFACTS_URL}) · [commit]({COMMIT_URL})"
)

pathlib.Path("/tmp/mail_subject").write_text(subject)
pathlib.Path("/tmp/mail.html").write_text(html_body)
pathlib.Path("/tmp/mail.txt").write_text(text_body)
pathlib.Path("/tmp/issue.md").write_text(issue_md)
print(f"built alarm content: {len(jobs)} failed job(s), {len(tests)} test name(s)")
