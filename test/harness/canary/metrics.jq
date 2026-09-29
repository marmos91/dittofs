# The last canary pass as Prometheus metrics (text format), for node_exporter's
# textfile collector. Input: a canary result (last.json), from either implementation.
#   jq -r --arg impl go -f metrics.jq last.json
# Every metric is a gauge labelled share and impl; the counts also carry checkpoint
# (before, written, deleted, gc) and the step timings step.
def metric($name; $help): "# HELP dittofs_canary_\($name) \($help)\n# TYPE dittofs_canary_\($name) gauge";
def num: if type == "number" then tostring else "NaN" end;
def checkpoints: ["before", "written", "deleted", "gc"];

. as $r
| "share=\"\($r.share // "")\",impl=\"\($impl)\"" as $l
| (if ($r.seconds | type) == "object" then $r.seconds else {total: $r.seconds} end) as $secs
| metric("pass_ok"; "1 if the last canary pass passed, 0 if it failed."),
  "dittofs_canary_pass_ok{\($l)} \(if $r.status == "PASS" then 1 else 0 end)",
  metric("last_pass_timestamp_seconds"; "When the last canary pass ended, in Unix time."),
  "dittofs_canary_last_pass_timestamp_seconds{\($l)} \($r.time | fromdateiso8601)",
  metric("pass_duration_seconds"; "How long the last canary pass took."),
  "dittofs_canary_pass_duration_seconds{\($l)} \($secs.total | num)",
  metric("step_duration_seconds"; "How long each step of the last canary pass took."),
  ($secs | to_entries[] | select(.key != "total") | "dittofs_canary_step_duration_seconds{\($l),step=\"\(.key)\"} \(.value | num)"),
  metric("written_bytes"; "Bytes the last canary pass wrote to the share."),
  "dittofs_canary_written_bytes{\($l)} \($r.bytes // 0)",
  ( ["rclone_objects", "rclone_size", "objects", "`rclone size` of the canary's S3 prefix: objects, at each checkpoint of the last pass."],
    ["rclone_bytes", "rclone_size", "bytes", "`rclone size` of the canary's S3 prefix: bytes, at each checkpoint of the last pass."],
    ["rclone_blocks", "rclone_size", "blocks", "`rclone size` of the prefix's blocks/ directory: block objects, at each checkpoint of the last pass."],
    ["rclone_block_bytes", "rclone_size", "block_bytes", "`rclone size` of the prefix's blocks/ directory: bytes, at each checkpoint of the last pass."],
    ["s3_list_objects", "s3_list", "objects", "S3 listing of the canary's prefix (the Go test's cross-check): objects, at each checkpoint of the last pass."],
    ["server_blocks_remote", "server_blocks", "blocks_remote", "The server's blocks_remote for the share (dfsctl store block stats), at each checkpoint of the last pass."],
    ["server_blocks_local", "server_blocks", "blocks_local", "The server's blocks_local for the share, at each checkpoint of the last pass."],
    ["server_blocks_all", "server_blocks", "blocks_total", "The server's blocks_total for the share (not a counter, so not _total), at each checkpoint of the last pass."]
    | . as [$name, $group, $key, $help]
    | metric($name; $help),
      (checkpoints[] as $c | $r[$group][$c][$key]? // empty
        | "dittofs_canary_\($name){\($l),checkpoint=\"\($c)\"} \(.)") )
