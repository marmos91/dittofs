export const meta = {
  name: 'area-audit',
  description: 'v1.0 area audit: fan out parallel sub-audits, adversarially verify every HIGH, synthesize REVIEW.md',
  whenToUse: 'Run a Wave-1 area PR-A audit (correctness + design). Pass args={area,title,refs,repo,subAreas:[{key,prompt}]}. Returns a synthesized REVIEW.md markdown string + verified findings.',
  phases: [
    { title: 'Audit', detail: 'parallel read-only sub-audits, each returns structured findings' },
    { title: 'Verify', detail: 'adversarially verify every HIGH/CRITICAL finding' },
    { title: 'Synthesize', detail: 'merge verified findings into REVIEW.md' },
  ],
}

const FINDINGS_SCHEMA = {
  type: 'object',
  required: ['summary', 'findings'],
  additionalProperties: false,
  properties: {
    summary: { type: 'string', description: 'one-paragraph state of this sub-area + headline' },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        required: ['severity', 'title', 'file', 'line', 'what', 'why', 'fix', 'confidence'],
        additionalProperties: false,
        properties: {
          severity: { type: 'string', enum: ['HIGH', 'MED', 'LOW'] },
          title: { type: 'string' },
          file: { type: 'string' },
          line: { type: 'string' },
          what: { type: 'string' },
          why: { type: 'string' },
          fix: { type: 'string' },
          confidence: { type: 'integer', minimum: 0, maximum: 100 },
        },
      },
    },
    verifiedCorrect: { type: 'array', items: { type: 'string' } },
  },
}

const VERDICT_SCHEMA = {
  type: 'object',
  required: ['real', 'rationale', 'adjustedSeverity'],
  additionalProperties: false,
  properties: {
    real: { type: 'boolean' },
    rationale: { type: 'string' },
    adjustedSeverity: { type: 'string', enum: ['HIGH', 'MED', 'LOW', 'RESOLVED'] },
  },
}

let A = args
if (typeof A === 'string') {
  try { A = JSON.parse(A) } catch (e) { throw new Error('area-audit: args is a string but not valid JSON: ' + e.message) }
}
if (!A || !A.subAreas || !Array.isArray(A.subAreas) || A.subAreas.length === 0) {
  throw new Error('area-audit requires args={area,title,refs,repo,subAreas:[{key,prompt}]}. Got typeof=' + (typeof args))
}
const AREA = A.area || 'unknown'
const TITLE = A.title || AREA
const REFS = A.refs || '(none specified)'
const REPO = A.repo || '/Users/marmos91/Projects/dittofs-v10-plan'

const auditPreamble = 'v1.0 audit of the DittoFS ' + TITLE + ' area. Repo: ' + REPO + '. READ-ONLY — do NOT modify any source; do NOT write files. Read README.md, CLAUDE.md, and relevant docs/ first for invariants (handlers protocol-only; logic in pkg/metadata; every op carries *metadata.AuthContext; file handles opaque; return metadata.ExportError codes). Cross-check vs: ' + REFS + '.\n\nAudit dimensions: (1) correctness vs canonical spec — flag deviations tests do not catch; (2) security — input validation, path traversal, auth/squash, signing/crypto, unbounded length to alloc (DoS); (3) concurrency — races, lock ordering, goroutine leaks; (4) resource lifecycle — leaks, missing Close, ctx plumbing; (5) error handling — swallowed errors, wrong codes; (6) bloat — dead code, useless interfaces (1 impl + <=2 callers AND not a cycle-break), 1:1 DTO mirrors, god-files; (7) simplicity — over-abstraction, boilerplate.\n\nCRITICAL: cite exact file:line for every finding (verify the line by reading it). Set confidence 0-100 honestly. Be skeptical — if clean, return few/no findings rather than inventing. Also return verifiedCorrect[] for things checked and found OK. Return ONLY the structured object — do not write any file.'

phase('Audit')

const perSub = await pipeline(
  A.subAreas,
  (sub) => agent(
    auditPreamble + '\n\n## Your sub-area: ' + sub.key + '\n' + sub.prompt,
    { label: 'audit:' + sub.key, phase: 'Audit', schema: FINDINGS_SCHEMA }
  ),
  (result, sub) => {
    if (!result) return { sub: sub.key, summary: '(audit failed)', findings: [], verified: [] }
    const highs = result.findings.filter((f) => f.severity === 'HIGH')
    if (highs.length === 0) return { sub: sub.key, summary: result.summary, findings: result.findings, verifiedCorrect: result.verifiedCorrect || [], verified: [] }
    return parallel(
      highs.map((f) => () =>
        agent(
          'Adversarially verify this audit finding by READING the cited code at ' + REPO + '. Your job is to REFUTE it — default to real=false unless the code at the cited location independently confirms the bug exactly as described. READ-ONLY, do not write files.\n\nFinding [' + f.severity + '] "' + f.title + '"\nLocation: ' + f.file + ':' + f.line + '\nClaim: ' + f.what + '\nWhy it matters: ' + f.why + '\n\nRead the actual code. Does the bug exist as described at that location? If the claim misreads the code, or a guard/caller elsewhere already handles it, mark real=false and adjustedSeverity=RESOLVED with rationale citing what you found. If real, keep or adjust severity.',
          { label: 'verify:' + sub.key + ':' + f.file, phase: 'Verify', schema: VERDICT_SCHEMA }
        ).then((v) => ({ finding: f, verdict: v }))
      )
    ).then((verdicts) => ({ sub: sub.key, summary: result.summary, findings: result.findings, verifiedCorrect: result.verifiedCorrect || [], verified: verdicts.filter(Boolean) }))
  }
)

const subs = perSub.filter(Boolean)

let totalHigh = 0, refuted = 0
for (const s of subs) {
  const vmap = new Map((s.verified || []).map((v) => [v.finding.title, v.verdict]))
  for (const f of s.findings) {
    if (f.severity === 'HIGH') {
      const v = vmap.get(f.title)
      if (v && !v.real) { f.severity = v.adjustedSeverity || 'RESOLVED'; f.refutedRationale = v.rationale; refuted++ }
      else { totalHigh++; if (v) f.verifiedRationale = v.rationale }
    }
  }
}
log('Audited ' + subs.length + ' sub-areas. HIGH confirmed: ' + totalHigh + ', refuted/downgraded: ' + refuted + '.')

phase('Synthesize')

const tally = subs.map((s) => {
  const c = { HIGH: 0, MED: 0, LOW: 0, RESOLVED: 0 }
  for (const f of s.findings) c[f.severity] = (c[f.severity] || 0) + 1
  return { sub: s.sub, summary: s.summary, counts: c }
})

const reviewMd = await agent(
  'You are synthesizing a v1.0 area-audit REVIEW.md for the DittoFS ' + TITLE + ' area. Below is verified findings data (JSON) from ' + subs.length + ' parallel sub-audits. Every HIGH was independently adversarially verified; refuted ones were downgraded to RESOLVED with a rationale.\n\nProduce a complete REVIEW.md in GitHub markdown matching this structure (mirror the existing blockstore/nfs REVIEW.md house style):\n- Title + Status/Date/Scope/Cross-check refs header\n- ## 1. Summary — severity table per sub-area + overall total + verdict (PATCH-grade if minor / NEEDS-FIX if HIGH integrity holes) + whether architecture invariants hold\n- ## 2. HIGH findings — ranked by blast radius, grouped, each: bold title — file:line — what / why / fix. Include verifier rationale where useful.\n- ## 3. Triage downgrades / RESOLVED — every refuted HIGH with rationale\n- ## 4. MED findings — terse, grouped by sub-area\n- ## 5. LOW findings — terse, grouped\n- ## 6. Verified-correct — things checked and found OK\n- ## 7. Recommended PR-B shape — split HIGHs into focused fix PRs, defer MED/LOW as issues\n- ## 8. Coverage — what was/was not audited\n\nBe precise, keep file:line citations. Output ONLY the markdown.\n\nDATA:\n' + JSON.stringify({ area: AREA, title: TITLE, refs: REFS, tally, subs }, null, 2),
  { label: 'synthesize:REVIEW.md', phase: 'Synthesize' }
)

return {
  area: AREA,
  reviewMarkdown: reviewMd,
  counts: { highConfirmed: totalHigh, highRefuted: refuted },
  subAreas: tally,
}
