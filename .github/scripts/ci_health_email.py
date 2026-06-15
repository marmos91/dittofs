#!/usr/bin/env python3
"""Build the develop-red alarm email (HTML + text), subject, and issue markdown.

Reads run facts from the environment plus pre-collected data from /tmp:
  - /tmp/failed_jobs.ndjson  one JSON object per failed job: {name, url, step}
  - /tmp/failed.log          `gh run view --log-failed` output (job\tstep\tline)

Writes (consumed by ci-health.yml):
  /tmp/mail_subject, /tmp/mail.html, /tmp/mail.txt, /tmp/issue.md

Best-effort by design: missing/empty inputs degrade gracefully to a
links-only message, never an error — the alarm must always send.
"""
import html
import json
import os
import pathlib
import re

CAP_PER_JOB = 12   # failing test names listed per job (rest -> logs)

W = os.environ.get("WORKFLOW", "conformance")
C = os.environ.get("CONCLUSION", "failure")
SHA = os.environ.get("HEAD_SHA", "")
SHORT = SHA[:8] if SHA else "unknown"
RUN_URL = os.environ.get("RUN_URL", "")
REPO = os.environ.get("REPO", "")
ATTEMPT = os.environ.get("RUN_ATTEMPT", "")
_msg_lines = (os.environ.get("COMMIT_MSG", "") or "").splitlines()
COMMIT_MSG = _msg_lines[0] if _msg_lines else ""
COMMIT_AUTHOR = os.environ.get("COMMIT_AUTHOR", "")
ARTIFACTS_URL = f"{RUN_URL}#artifacts" if RUN_URL else ""
COMMIT_URL = f"https://github.com/{REPO}/commit/{SHA}" if REPO and SHA else RUN_URL

# pjdfstest files (rename/23.t) and smbtorture/WPTS ids (smb2.lock.rw).
TEST_RE = re.compile(r"[a-z_]+/[0-9]+\.t|smb2\.[A-Za-z0-9_.]+")
# A line names a failing test if it reads like a failure ("not ok", "FAIL"),
# or is a grader bullet ("  - chmod/11.t") in the "New failures" list.
FAILISH = re.compile(r"not ok|FAIL|New failures|Failed|^\s*-\s+\S", re.IGNORECASE)


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


def _tests_by_job(path):
    """Attribute failing test names to jobs from the prefixed failed-step log.

    `gh run view --log-failed` prefixes each line `<job>\t<step>\t<content>`.
    """
    by_job, seen = {}, {}
    try:
        text = pathlib.Path(path).read_text(errors="replace")
    except FileNotFoundError:
        return by_job
    for line in text.splitlines():
        parts = line.split("\t", 2)
        if len(parts) < 3:
            continue
        job, _step, content = parts
        # gh prefixes each log line with an RFC3339 timestamp — strip it so the
        # grader-bullet anchor ("  - <test>") and other patterns match.
        content = re.sub(r"^\d{4}-\d\d-\d\dT\S+Z\s+", "", content)
        if not FAILISH.search(content):
            continue
        for tok in TEST_RE.findall(content):
            bucket = by_job.setdefault(job, [])
            key = seen.setdefault(job, set())
            if tok not in key:
                key.add(tok)
                bucket.append(tok)
    return by_job


jobs = _read_jobs("/tmp/failed_jobs.ndjson")
tests_by_job = _tests_by_job("/tmp/failed.log")


def _tests_for(job_name):
    if job_name in tests_by_job:
        return tests_by_job[job_name]
    for k, v in tests_by_job.items():
        if k.strip() == job_name.strip():
            return v
    return []


total_tests = sum(len(v) for v in tests_by_job.values())
subject = f"\U0001F534 [DittoFS] develop RED — {len(jobs)} failed job(s) in {W} ({SHORT})"

# ---------------------------------------------------------------- HTML email
BTN = ("display:inline-block;padding:9px 16px;margin:0 8px 8px 0;border-radius:6px;"
       "font-size:14px;font-weight:600;text-decoration:none;")
MONO = "font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;"


def _chip(t):
    return (f'<code style="{MONO}background:#fff1f0;color:#a40e26;'
            f'padding:1px 6px;border-radius:4px;font-size:12px;'
            f'display:inline-block;margin:2px 4px 2px 0">{html.escape(t)}</code>')


job_cards = []
for j in jobs:
    name = html.escape(j.get("name", "?"))
    url = j.get("url") or RUN_URL
    step = j.get("step") or ""
    tests = _tests_for(j.get("name", ""))
    shown = tests[:CAP_PER_JOB]
    chips = "".join(_chip(t) for t in shown) if shown else \
        '<span style="color:#57606a;font-size:13px">no test names extracted — see logs</span>'
    more = f'<span style="color:#57606a;font-size:12px"> +{len(tests)-CAP_PER_JOB} more</span>' \
        if len(tests) > CAP_PER_JOB else ""
    step_line = (f'<div style="color:#57606a;font-size:12px;margin:2px 0 8px">'
                 f'failed step: <code style="{MONO}">{html.escape(step)}</code></div>') if step else ""
    job_cards.append(
        f'<tr><td style="padding:12px 14px;border:1px solid #d0d7de;border-radius:8px;'
        f'background:#fafbfc">'
        f'<div style="font-weight:600;font-size:14px">✗ {name} '
        f'<a href="{url}" style="font-weight:500;font-size:13px;color:#0969da;'
        f'text-decoration:none">▶ logs</a></div>'
        f'{step_line}<div>{chips}{more}</div></td></tr>'
        f'<tr><td style="height:8px"></td></tr>'
    )
if not jobs:
    job_cards.append(
        f'<tr><td style="padding:12px 14px;border:1px solid #d0d7de;border-radius:8px">'
        f'See <a href="{RUN_URL}">the run</a> for failed jobs.</td></tr>')

commit_row = ""
if COMMIT_MSG or COMMIT_AUTHOR:
    msg = html.escape(COMMIT_MSG[:100])
    auth = html.escape(COMMIT_AUTHOR)
    commit_row = (f'<tr><td style="padding:3px 0;color:#57606a;width:90px">Commit</td>'
                  f'<td style="padding:3px 0"><a href="{COMMIT_URL}" style="{MONO}color:#0969da;'
                  f'text-decoration:none">{SHORT}</a> {msg}'
                  f'{(" · " + auth) if auth else ""}</td></tr>')

attempt_txt = f" · attempt {ATTEMPT}" if ATTEMPT and ATTEMPT != "1" else ""

html_body = f"""<div style="max-width:680px;margin:0 auto;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1f2328">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse">
<tr><td style="background:#cf222e;border-radius:8px 8px 0 0;padding:16px 20px;color:#fff;font-size:18px;font-weight:700">
&#128308; develop CI is RED</td></tr>
<tr><td style="border:1px solid #d0d7de;border-top:none;border-radius:0 0 8px 8px;padding:16px 20px">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="font-size:13px;border-collapse:collapse">
<tr><td style="padding:3px 0;color:#57606a;width:90px">Workflow</td><td style="padding:3px 0;font-weight:600">{html.escape(W)}{attempt_txt}</td></tr>
<tr><td style="padding:3px 0;color:#57606a">Result</td><td style="padding:3px 0;color:#cf222e;font-weight:600">{html.escape(C)}</td></tr>
{commit_row}
<tr><td style="padding:3px 0;color:#57606a">Branch</td><td style="padding:3px 0">develop</td></tr>
</table>
<div style="margin:16px 0 8px">
<a href="{RUN_URL}" style="{BTN}background:#cf222e;color:#fff">View run &amp; logs</a>
<a href="{COMMIT_URL}" style="{BTN}background:#f6f8fa;color:#24292f;border:1px solid #d0d7de">View commit</a>
<a href="{ARTIFACTS_URL}" style="{BTN}background:#f6f8fa;color:#24292f;border:1px solid #d0d7de">Download artifacts</a>
</div>
<div style="font-size:14px;font-weight:700;margin:14px 0 8px">Failed jobs &amp; tests</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse">
{''.join(job_cards)}
</table>
<div style="margin-top:14px;padding:12px 14px;background:#fff8c5;border:1px solid #d4a72c;border-radius:6px;font-size:13px">
<b>develop is blocked-by-convention until green.</b> Open PRs show a warning banner.
Fix forward, or revert <code style="{MONO}">{SHORT}</code> — re-running the failed workflow re-fires this check.
</div>
</td></tr></table>
<div style="color:#8b949e;font-size:11px;text-align:center;margin-top:12px">DittoFS CI &middot; develop-red alarm</div>
</div>"""

# ---------------------------------------------------------------- plain text
text_jobs = []
for j in jobs:
    tests = _tests_for(j.get("name", ""))[:CAP_PER_JOB]
    line = f"  ✗ {j.get('name','?')}"
    if j.get("step"):
        line += f" (failed step: {j.get('step')})"
    line += f"\n     logs: {j.get('url') or RUN_URL}"
    if tests:
        line += "\n     tests: " + ", ".join(tests)
    text_jobs.append(line)
if not text_jobs:
    text_jobs.append(f"  see {RUN_URL}")

text_body = f"""develop CI is RED — {W}{attempt_txt}

Result:  {C}
Commit:  {SHORT}{('  ' + COMMIT_MSG) if COMMIT_MSG else ''}{('  (' + COMMIT_AUTHOR + ')') if COMMIT_AUTHOR else ''}
         {COMMIT_URL}
Branch:  develop

Failed jobs & tests:
{chr(10).join(text_jobs)}

Links:
  Run & logs: {RUN_URL}
  Artifacts:  {ARTIFACTS_URL}

develop is blocked-by-convention until green; open PRs show a warning banner.
Fix forward, or revert {SHORT}."""

# ---------------------------------------------------------------- issue markdown
issue_lines = []
for j in jobs:
    tests = _tests_for(j.get("name", ""))[:CAP_PER_JOB]
    li = f"- ✗ **{j.get('name','?')}**"
    if j.get("step"):
        li += f" — failed step: `{j.get('step')}`"
    li += f" · [logs]({j.get('url') or RUN_URL})"
    if tests:
        li += "\n  - tests: " + ", ".join(f"`{t}`" for t in tests)
    issue_lines.append(li)
issue_jobs = "\n".join(issue_lines) or f"- see [the run]({RUN_URL})"
commit_md = f"\n- Commit: `{SHORT}` {COMMIT_MSG}".rstrip() if COMMIT_MSG else f"\n- Commit: `{SHORT}`"
issue_md = (
    f"**{W}** concluded `{C}` on `develop`.{commit_md}\n\n"
    f"**Failed jobs ({len(jobs)}):**\n{issue_jobs}\n\n"
    f"Links: [run]({RUN_URL}) · [artifacts]({ARTIFACTS_URL}) · [commit]({COMMIT_URL})"
)

pathlib.Path("/tmp/mail_subject").write_text(subject)
pathlib.Path("/tmp/mail.html").write_text(html_body)
pathlib.Path("/tmp/mail.txt").write_text(text_body)
pathlib.Path("/tmp/issue.md").write_text(issue_md)
print(f"built alarm content: {len(jobs)} failed job(s), {total_tests} test name(s) across jobs")
