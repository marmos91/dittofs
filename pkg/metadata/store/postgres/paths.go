package postgres

// Path reconstruction (#1166).
//
// The inodes table no longer stores a canonical path: parent_child_map
// (parent_id, child_name -> child_id) is the sole source of truth for the
// namespace. metadata.File.Path is still populated by GetFile and the
// payload-id lookup (callers and the conformance suite derive child paths from
// a parent's Path), so it is reconstructed on read by walking parent_child_map
// up to the share root.
//
// For a hard-linked inode (N names) the walk is deterministic but arbitrary:
// at each level it follows a single edge ordered by (parent_id, child_name),
// yielding ONE of the inode's paths. POSIX does not guarantee which path a
// stat() reflects for a multiply-linked file, so returning any valid reachable
// path is correct. An inode with no parent edge (a share root, or an
// orphaned/unlinked-but-open inode) resolves to "/".
//
// inodePathExpr is a correlated scalar subquery that computes the path for the
// row aliased `f` in the enclosing query (it references f.id). Splice it into a
// SELECT list in place of the old f.path column. It is a single round-trip with
// the rest of the row.
//
// PostgreSQL forbids ORDER BY / LIMIT directly in a recursive CTE's anchor or
// recursive term, so the per-level "pick one edge" is done inside derived
// subqueries (the anchor wraps an ordered LIMIT 1; the recursive term picks via
// CROSS JOIN LATERAL).
//
// The `depth < 4096` guard bounds the walk: parent_child_map's schema permits a
// directed cycle (only FK constraints to inodes(id), no acyclicity check) that
// no filesystem operation can create but a bug or direct DB edit could. Without
// the cap a cycle would make every GetFile for a node in it recurse until
// statement_timeout. 4096 is well above any real path-component depth (it
// matches the filesystem MaxPathLen ceiling), so it never truncates a
// legitimate path while turning a corrupt cycle into a bounded, finite result.
const inodePathExpr = `(
	WITH RECURSIVE up(parent_id, child_name, depth) AS (
		SELECT anchor.parent_id, anchor.child_name, 1
		FROM (
			SELECT e.parent_id, e.child_name
			FROM parent_child_map e
			WHERE e.child_id = f.id
			ORDER BY e.parent_id, e.child_name
			LIMIT 1
		) AS anchor

		UNION ALL

		SELECT picked.parent_id, picked.child_name, up.depth + 1
		FROM up
		CROSS JOIN LATERAL (
			SELECT e.parent_id, e.child_name
			FROM parent_child_map e
			WHERE e.child_id = up.parent_id
			ORDER BY e.parent_id, e.child_name
			LIMIT 1
		) AS picked
		WHERE up.depth < 4096
	)
	SELECT COALESCE('/' || string_agg(child_name, '/' ORDER BY depth DESC), '/')
	FROM up
)`
