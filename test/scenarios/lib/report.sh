# shellcheck shell=bash disable=SC2153 # CACHE, HERE and SCENARIOS come from setup.sh
# How setup.sh reports a scenario run: its times, one line in the terminal, the logs it keeps and
# its Slack messages. Sourced by setup.sh, which sets CACHE, HERE and SCENARIOS.
#
# Logs: a run that failed or was slow keeps its logs in CACHE/failed/<scenario>/ or
# CACHE/slow/<scenario>/ until the scenario passes at normal speed again. A normal pass leaves
# them in CACHE/run/ until the next run.
#
# Times: every run adds its times to CACHE/times/<scenario>.tsv, created by the first run: one row
# for the scenario, one for each of its lines that ran. A pass is slow when the scenario or one of
# its lines took more than twice its usual time (the median of its last 10 normal passes; slow
# ones do not count) and at least 10 s more.
#
# Slack: with a bot token (scope chat:write) in SLACK_DIR/slack-token and a channel ID in
# SLACK_DIR/slack-channel. SLACK_DIR is ~/.config/dittofs-scenarios, outside the cache, so no
# container sees them:
#   - a scenario's first failure is a message; a later failure at a different check replies in its
#     thread, and the pass that ends it replies there and shows in the channel too;
#   - its first slow pass is a message, and the pass back at normal speed replies in its thread;
#   - a run of several scenarios (setup.sh all) posts one message instead, puts all of the above in
#     its thread, and ends with a summary there that shows in the channel too.
# SCENARIO_HOST names the machine in messages (default: the short hostname, or uname -n where there
# is no hostname command).

SLACK_DIR="${SLACK_DIR:-$HOME/.config/dittofs-scenarios}"
TIMES="$CACHE/times"
COMMIT="$(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo unknown)"

# A run of several scenarios: its thread, how many have been reported, which failed, how many were slow.
RUN_TS=""
RUN_DONE=0
RUN_SLOW=0
RUN_FAILED=()
RUN_START="$(date +%s)"

# times SCENARIO END: this run's times, one "<seconds><tab><what>" line each: the scenario first,
# then each of its lines that ran, e.g. "3.2<tab>line 12: dfsctl system drain-uploads". Each
# command is traced as "+ <epoch seconds> <line> <command>" and lasts until the next one starts;
# the last one lasts until END, when the run ended. A line that runs more than once, in a loop,
# gets its times added up.
times() {
    awk -v scenario="$HERE/$1" -v end="$2" '
        /^\++ [0-9]+\.[0-9]+ [0-9]+ / {
            if (prev) took[line] += $2 - prev; else first = $2
            prev = $2
            line = $3
        }
        END {
            if (!prev) exit
            took[line] += end - prev
            printf "%.1f\tscenario\n", end - first
            while ((getline text <scenario) > 0) {
                n++
                sub(/^[ \t]+/, "", text)
                gsub(/\t/, " ", text)
                if (n in took && text !~ /^#/) printf "%.1f\tline %d: %s\n", took[n], n, substr(text, 1, 120)
            }
        }' "$CACHE/run/run.log"
}

# slower SCENARIO: compares this run's times with the scenario's record and prints the slow ones,
# e.g. "31.0 s (usually 3.1 s) line 12: dfsctl system drain-uploads".
slower() {
    test -f "$TIMES/${1%.sh}.tsv" || return 0
    awk -F'\t' '
        # The record (when, commit, result, seconds, what): the last 10 normal passes of each what.
        FNR == NR {
            if ($3 == "pass") last[$5, runs[$5]++ % 10] = $4 + 0
            next
        }
        # This run (seconds, what).
        $2 in runs {
            usual = median($2)
            if ($1 > 2 * usual && $1 - usual >= 10) printf "%.0f s (usually %.0f s) %s\n", $1, usual, $2
        }
        # The middle one of the last 10, sorted.
        function median(what,   n, i, j, t, sorted) {
            n = runs[what] < 10 ? runs[what] : 10
            for (i = 0; i < n; i++) {
                t = last[what, i]
                for (j = i; j > 0 && sorted[j - 1] > t; j--) sorted[j] = sorted[j - 1]
                sorted[j] = t
            }
            return sorted[int(n / 2)]
        }' "$TIMES/${1%.sh}.tsv" "$CACHE/run/times"
}

# record SCENARIO RESULT: adds this run's times to the scenario's record, created if it is missing.
record() {
    local FILE="$TIMES/${1%.sh}.tsv"
    mkdir -p "$TIMES"
    test -f "$FILE" || printf 'when\tcommit\tresult\tseconds\twhat\n' >"$FILE"
    awk -v OFS='\t' -v when="$(date '+%F %T')" -v commit="$COMMIT" -v result="$2" \
        '{ print when, commit, result, $0 }' "$CACHE/run/times" >>"$FILE"
}

# slack TEXT [THREAD] [BROADCAST]: posts TEXT and prints the new message's ts. The first line is the
# message, the rest a colored attachment: red failing, yellow slow, green passing. With THREAD it is
# a reply there; BROADCAST=yes shows the reply in the channel too. Does nothing without a token and
# a channel. The token goes to curl on stdin, never on the command line.
slack() {
    test -s "$SLACK_DIR/slack-token" && test -s "$SLACK_DIR/slack-channel" || return 0
    local TEXT COLOR TITLE DETAILS BODY
    case "$1" in :x:*) COLOR="#E01E5A" ;; :warning:*) COLOR="#ECB22E" ;; *) COLOR="#2EB67D" ;; esac
    # &, < and > are escaped for Slack, then the text becomes a JSON string.
    TEXT="$(printf '%s' "$1" | tr -d '\000-\011\013-\037' | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')"
    TEXT="${TEXT//\\/\\\\}"
    TEXT="${TEXT//\"/\\\"}"
    TEXT="${TEXT//$'\n'/\\n}"
    TITLE="${TEXT%%\\n*}"
    DETAILS="${TEXT#*\\n}"
    BODY="{\"channel\":\"$(cat "$SLACK_DIR/slack-channel")\",\"text\":\"$TITLE\",\"attachments\":[{\"color\":\"$COLOR\",\"text\":\"$DETAILS\",\"mrkdwn_in\":[\"text\"]}]${2:+,\"thread_ts\":\"$2\"}"
    test "${3:-no}" = no || BODY="$BODY,\"reply_broadcast\":true"
    printf 'header = "Authorization: Bearer %s"\n' "$(cat "$SLACK_DIR/slack-token")" |
        curl -sf -K - -H 'Content-Type: application/json; charset=utf-8' -d "$BODY}" "${SLACK_API:-https://slack.com/api}/chat.postMessage" |
        grep -oE '"ts": *"[0-9]+\.[0-9]+"' | head -1 | grep -oE '[0-9]+\.[0-9]+' || true
}

# report SCENARIO RESULT: reports a run whose log is CACHE/run/run.log. RESULT is pass or fail; a
# pass that was slow is reported as slow.
report() {
    local S="$1" RESULT="$2" LOG="$CACHE/run/run.log" FAILDIR="$CACHE/failed/$1" SLOWDIR="$CACHE/slow/$1"
    local TOOK SLOW="" FAIL_TS FAIL_AT SLOW_TS CONTEXT LAST LINE RAN OUT WHERE CHECK DETAILS T="" BROADCAST=yes
    # The run has just ended, so its last command ended now.
    times "$S" "${EPOCHREALTIME/,/.}" >"$CACHE/run/times"
    TOOK="$(awk -F'\t' '$2 == "scenario" { printf "%d", $1 }' "$CACHE/run/times")"
    test "$RESULT" = fail || SLOW="$(slower "$S")"
    test -z "$SLOW" || RESULT=slow
    record "$S" "$RESULT"
    FAIL_TS="$(cat "$FAILDIR/slack-ts" 2>/dev/null || true)"
    FAIL_AT="$(cat "$FAILDIR/where" 2>/dev/null || true)"
    SLOW_TS="$(cat "$SLOWDIR/slack-ts" 2>/dev/null || true)"
    CONTEXT="${SCENARIO_HOST:-$(hostname -s 2>/dev/null || uname -n)} · commit \`$COMMIT\` · $(date '+%F %T')"
    # In a run of several scenarios, every message goes to the run's thread (T).
    if test "${#SCENARIOS[@]}" -gt 1; then
        test -n "$RUN_TS" || RUN_TS="$(slack ":arrow_forward: *Scenario run: ${#SCENARIOS[@]} scenarios*
$CONTEXT")"
        T="$RUN_TS"
        BROADCAST=no
    fi

    case "$RESULT" in
    fail)
        # The failing command is the last one traced; its output follows it in the log.
        LAST="$(grep -an '^+' "$LOG" | tail -1 || true)"
        LINE="$(sed -nE 's/^[0-9]+:\++ [0-9.]+ ([0-9]+) .*/\1/p' <<<"$LAST")"
        RAN="$(sed -E 's/^[0-9]+:\++ ([0-9.]+ [0-9]+ )?//' <<<"$LAST")"
        OUT="$(tail -n +"$((${LAST%%:*} + 1))" "$LOG" | grep -v '^+' | head -3 | cut -c1-200 || true)"
        WHERE="setup.sh"
        test -z "$LINE" || WHERE="line $LINE: $(sed -n "${LINE}p" "$HERE/$S")"
        echo "FAIL $S ($(date '+%F %T')), ${LINE:+line $LINE: }${RAN:0:200} (logs: $FAILDIR/)"
        CHECK="${WHERE//\`/\'}"
        RAN="${RAN//\`/\'}"
        DETAILS="*Check:* \`${CHECK:0:300}\`
*Ran:* \`${RAN:0:300}\`"
        test -z "$OUT" || DETAILS="$DETAILS
*Output:* \`\`\`$OUT\`\`\`"
        DETAILS="$DETAILS
$CONTEXT · logs \`$FAILDIR/\`"
        # A check moves to another line when lines are added above it, so only its text counts.
        test -d "$FAILDIR" && test "${WHERE#line *: }" != "${FAIL_AT#line *: }" && slack ":x: *$S now fails at a different check*
$DETAILS" "${T:-$FAIL_TS}" >/dev/null
        test -d "$FAILDIR" || FAIL_TS="$(slack ":x: *$S failed*
$DETAILS" "$T")"
        test -z "$T" || test -d "$FAILDIR" || FAIL_TS="$T"
        rm -rf "$FAILDIR"
        mv "$CACHE/run" "$FAILDIR"
        echo "$WHERE" >"$FAILDIR/where"
        test -z "$FAIL_TS" || echo "$FAIL_TS" >"$FAILDIR/slack-ts"
        ;;
    slow)
        echo "SLOW $S ($(date '+%F %T')): ${SLOW//$'\n'/; }"
        test -d "$SLOWDIR" || SLOW_TS="$(slack ":warning: *$S passed but was slow*
${SLOW//\`/\'}
$CONTEXT · logs \`$SLOWDIR/\`" "$T")"
        test -z "$T" || test -d "$SLOWDIR" || SLOW_TS="$T"
        rm -rf "$SLOWDIR"
        mv "$CACHE/run" "$SLOWDIR"
        test -z "$SLOW_TS" || echo "$SLOW_TS" >"$SLOWDIR/slack-ts"
        ;;
    pass)
        test "${#SCENARIOS[@]}" -eq 1 || echo "PASS $S (${TOOK}s)"
        test -d "$SLOWDIR" && slack ":white_check_mark: *$S is back to normal speed*
$CONTEXT" "${T:-$SLOW_TS}" >/dev/null
        rm -rf "$SLOWDIR"
        ;;
    esac
    test "$RESULT" != fail && test -d "$FAILDIR" && slack ":white_check_mark: *$S passes again*
$CONTEXT" "${T:-$FAIL_TS}" "$BROADCAST" >/dev/null
    test "$RESULT" = fail || rm -rf "$FAILDIR"

    # After the last scenario of a run of several: the summary, in the thread and in the channel.
    RUN_DONE=$((RUN_DONE + 1))
    test "$RESULT" != fail || RUN_FAILED+=("${S%.sh}")
    test "$RESULT" != slow || RUN_SLOW=$((RUN_SLOW + 1))
    test -n "$T" && test "$RUN_DONE" -eq "${#SCENARIOS[@]}" || return 0
    TOOK=$(($(date +%s) - RUN_START))
    if test "${#RUN_FAILED[@]}" -gt 0; then
        LINE=":x: *Scenario run: ${#RUN_FAILED[@]} of $RUN_DONE failed*"
    elif test "$RUN_SLOW" -gt 0; then
        LINE=":warning: *Scenario run: all $RUN_DONE passed, $RUN_SLOW slow*"
    else
        LINE=":white_check_mark: *Scenario run: all $RUN_DONE passed*"
    fi
    DETAILS="*Passed:* $((RUN_DONE - ${#RUN_FAILED[@]} - RUN_SLOW)), *slow:* $RUN_SLOW, *failed:* ${#RUN_FAILED[@]}, in $((TOOK / 60)) min $((TOOK % 60)) s"
    test "${#RUN_FAILED[@]}" -eq 0 || DETAILS="$DETAILS
*Failed:* ${RUN_FAILED[*]}"
    slack "$LINE
$DETAILS
$CONTEXT" "$T" yes >/dev/null
}
