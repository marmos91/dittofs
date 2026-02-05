# Phase 6: Documentation and Deployment - Context

**Gathered:** 2026-02-05
**Status:** Ready for planning

<domain>
## Phase Boundary

Complete documentation for the DittoFS Kubernetes operator and validation on Scaleway cluster. Includes CRD reference, installation guide (kubectl + Helm), Percona integration guide, and troubleshooting guide. Creating new operator features or changing functionality is out of scope.

</domain>

<decisions>
## Implementation Decisions

### Documentation Structure
- Docs live in `k8s/dittofs-operator/docs/` (close to code)
- Multi-file organization by topic: INSTALL.md, CRD_REFERENCE.md, PERCONA.md, TROUBLESHOOTING.md
- Main README.md in operator root with brief overview + quick start, linking to detailed docs
- Mermaid diagrams for architecture visualization (renders in GitHub)

### CRD Reference Depth
- Comprehensive documentation: every field with type, default, validation rules, and examples
- Complete CR examples embedded directly in the reference doc (not just links)
- Brief table of status conditions with one-line descriptions

### Installation Format
- Both kubectl and Helm installation methods documented
- Helm chart created as part of this phase (new deliverable)
- Chart location: `k8s/dittofs-operator/chart/`
- Chart name: `dittofs-operator`

### Troubleshooting Scope
- Cover core operator issues + Percona integration issues
- 5-7 most common issues minimum
- Debug kubectl commands inline with each issue section

### Claude's Discretion
- Exact Mermaid diagram content and complexity
- Format for troubleshooting entries (FAQ vs symptom/cause/solution)
- Which CR scenarios to include as complete examples
- Order and grouping of CRD fields in reference

</decisions>

<specifics>
## Specific Ideas

No specific requirements — open to standard approaches

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 06-documentation*
*Context gathered: 2026-02-05*
