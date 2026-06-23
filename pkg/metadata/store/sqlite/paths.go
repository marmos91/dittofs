package sqlite

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
// SQLite has no LATERAL join, so the per-level "pick one edge" is done with a
// correlated subquery on the row's implicit rowid: at each level the edge whose
// rowid matches the minimum (parent_id, child_name) ordering for the current
// child is selected, yielding exactly one deterministic parent edge.
//
// The `depth < 4096` guard bounds the walk: parent_child_map's schema permits a
// directed cycle (only FK constraints to inodes(id), no acyclicity check) that
// no filesystem operation can create but a bug or direct DB edit could. Without
// the cap a cycle would make every GetFile for a node in it recurse forever.
// 4096 is well above any real path-component depth (it matches the filesystem
// MaxPathLen ceiling), so it never truncates a legitimate path while turning a
// corrupt cycle into a bounded, finite result.
//
// Path component order is built INTO the recursive accumulator (`acc`) rather
// than relying on group_concat honoring a subquery ORDER BY: the bundled SQLite
// (3.41.x via glebarez/go-sqlite) predates the ordered-aggregate syntax
// (`group_concat(x ORDER BY …)`, 3.44+), and an aggregate over a derived table
// is not guaranteed to consume rows in the subquery's order. Each recursive
// step PREPENDS the parent's name (`e.child_name || '/' || up.acc`), so the
// deepest (root-most) row's `acc` already holds the full root-to-target path;
// the final SELECT picks that single row (`ORDER BY depth DESC LIMIT 1`), which
// is deterministic.
const inodePathExpr = `(
	WITH RECURSIVE up(parent_id, acc, depth) AS (
		SELECT e.parent_id, e.child_name, 1
		FROM parent_child_map e
		WHERE e.child_id = f.id
		  AND e.rowid = (
			SELECT e2.rowid FROM parent_child_map e2
			WHERE e2.child_id = f.id
			ORDER BY e2.parent_id, e2.child_name
			LIMIT 1
		  )

		UNION ALL

		SELECT e.parent_id, e.child_name || '/' || up.acc, up.depth + 1
		FROM parent_child_map e, up
		WHERE e.child_id = up.parent_id
		  AND e.rowid = (
			SELECT e2.rowid FROM parent_child_map e2
			WHERE e2.child_id = up.parent_id
			ORDER BY e2.parent_id, e2.child_name
			LIMIT 1
		  )
		  AND up.depth < 4096
	)
	SELECT COALESCE('/' || (SELECT acc FROM up ORDER BY depth DESC LIMIT 1), '/')
)`
