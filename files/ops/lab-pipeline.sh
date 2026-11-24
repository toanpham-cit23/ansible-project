#!/bin/sh
# lab-pipeline.sh - run the deployment, then re-run only what failed.
#
# The problem this solves: 07_check.yml reports drift per host as
#   CFG_MISSING: ospf neighbours=0 (02_ospf.yml)
# but master.yml is a straight list of import_playbook, so a drift found at the
# end of the run is only reported - never fixed in the same run. You had to read
# the recap and start again by hand.
#
# This driver keeps the pipeline linear but gives it a repair loop:
#
#   pass 1  master.yml                       full deployment
#   read    /tmp/lab-remediation-<host>.txt  written by 07_check.yml
#   pass 2+ only the playbooks that drifted   in numeric order, per host
#   push    09_git_github.yml                 upload the result, next cycle
#
# The retry is bounded (MAX_PASS) so a genuinely broken router cannot loop
# forever, and it re-runs <NN>_<name>.yml by NUMBER, so 02 always runs before 03.
#
# Usage:
#   ./files/ops/lab-pipeline.sh                 # full run + repair loop + push
#   ./files/ops/lab-pipeline.sh --no-push       # skip the GitHub upload
#   ./files/ops/lab-pipeline.sh --max-pass 3    # allow up to 3 repair passes
#   ./files/ops/lab-pipeline.sh --dry-run       # print the plan, run nothing
#
# Exit: 0 all hosts clean, 1 something still failing, 2 setup problem.

set -u

PROJECT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
PLAYBOOKS="$PROJECT_DIR/playbooks"
HOSTS_FILE="$PROJECT_DIR/hosts.ini"
MAX_PASS=2
DO_PUSH=1
DRY_RUN=0
EXTRA_ARGS=""

while [ $# -gt 0 ]; do
    case "$1" in
        --no-push)   DO_PUSH=0 ;;
        --max-pass)  MAX_PASS="${2:-2}"; shift ;;
        --dry-run)   DRY_RUN=1 ;;
        -e|--extra-vars) EXTRA_ARGS="$EXTRA_ARGS -e ${2:-}"; shift ;;
        -h|--help)   sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
    shift
done

cd "$PROJECT_DIR" || exit 2

if [ ! -f "$HOSTS_FILE" ]; then
    echo "FATAL: $HOSTS_FILE not found - run 00_precheck.yml first to clone the repo" >&2
    exit 2
fi

# Ansible on this controller is Alpine; keep the interpreter explicit.
APB=${APB:-ansible-playbook}

log()  { printf '%s\n' "$*"; }
head1() { printf '\n=== %s ===\n' "$*"; }

# ---------------------------------------------------------------- pass 1 ----

head1 "PASS 1 - full pipeline (master.yml)"
if [ "$DRY_RUN" -eq 1 ]; then
    log "would run: $APB playbooks/master.yml"
else
    # shellcheck disable=SC2086
    $APB playbooks/master.yml $EXTRA_ARGS
    PASS1_RC=$?
    log "master.yml exit=$PASS1_RC"
fi

# ------------------------------------------- collect what the check found ----

# 07_check.yml drops one file per host. Read them all, union the playbooks,
# and remember which hosts asked for each one.
collect() {
    rm -f /tmp/lab-replan.txt
    for f in /tmp/lab-remediation-*.txt; do
        [ -f "$f" ] || continue
        host=$(basename "$f" .txt)
        host=${host#lab-remediation-}
        while IFS= read -r pb; do
            [ -n "$pb" ] || continue
            printf '%s %s\n' "$pb" "$host" >> /tmp/lab-replan.txt
        done < "$f"
    done
    if [ -s /tmp/lab-replan.txt ]; then
        # unique playbook names, numeric order (02 before 03 before 10)
        cut -d' ' -f1 /tmp/lab-replan.txt | sort -u | sort
    fi
}

PLAN=$(collect)

if [ -z "$PLAN" ]; then
    head1 "NOTHING TO REPAIR"
    log "07_check.yml reported every host fully configured."
else
    head1 "REPAIR PLAN"
    for pb in $PLAN; do
        who=$(awk -v p="$pb" '$1==p {printf "%s ", $2}' /tmp/lab-replan.txt)
        log "  $pb  ->  $who"
    done
fi

# ------------------------------------------------------- repair passes ------

pass=2
while [ -n "$PLAN" ] && [ "$pass" -le "$MAX_PASS" ]; do
    head1 "PASS $pass - repair ($(echo "$PLAN" | wc -w | tr -d ' ') playbook(s))"
    for pb in $PLAN; do
        # only the hosts that reported this playbook as missing
        who=$(awk -v p="$pb" '$1==p {printf "%s,", $2}' /tmp/lab-replan.txt | sed 's/,$//')
        # Always add localhost. Some repairs have a controller-side half that a
        # router-only limit would filter out - 03_ntp.yml now has to run
        # "allow 192.168.23.0/24" on the controller before the routers can sync
        # at all. Implicit localhost always resolves, so this is harmless for
        # playbooks that have no local play.
        who="$who,localhost"
        log "--- $pb  (limit: $who)"
        if [ "$DRY_RUN" -eq 1 ]; then
            log "    would run: $APB playbooks/$pb --limit $who"
            continue
        fi
        # shellcheck disable=SC2086
        $APB "playbooks/$pb" --limit "$who" $EXTRA_ARGS
        log "    exit=$?"
    done

    # Re-run the check so the plan reflects reality, not the old report.
    if [ "$DRY_RUN" -eq 0 ]; then
        log "--- re-checking"
        $APB playbooks/07_check.yml $EXTRA_ARGS >/dev/null 2>&1
        NEW_PLAN=$(collect)
        if [ "$NEW_PLAN" = "$PLAN" ]; then
            log "plan unchanged after pass $pass - stopping (not a transient failure)"
            PLAN="$NEW_PLAN"
            break
        fi
        PLAN="$NEW_PLAN"
        [ -n "$PLAN" ] && log "still open: $(echo "$PLAN" | tr '\n' ' ')"
    else
        break
    fi
    pass=$((pass + 1))
done

# ------------------------------------------------------------- push back ----

if [ "$DO_PUSH" -eq 1 ]; then
    head1 "PUSH BACK - upload the finished run"
    if [ ! -f "$PROJECT_DIR/token.yml" ]; then
        log "SKIPPED: token.yml absent - run 08_vault.yml once to create it."
        log "         Without it 09_git_github.yml refuses to push (by design)."
    elif [ "$DRY_RUN" -eq 1 ]; then
        log "would run: $APB playbooks/09_git_github.yml"
    else
        # shellcheck disable=SC2086
        $APB playbooks/09_git_github.yml $EXTRA_ARGS
        log "09_git_github.yml exit=$?"
    fi
fi

# ---------------------------------------------------------------- verdict ---

head1 "FINAL"
if [ -n "$PLAN" ]; then
    log "STILL OPEN after $MAX_PASS pass(es):"
    for pb in $PLAN; do
        who=$(awk -v p="$pb" '$1==p {printf "%s ", $2}' /tmp/lab-replan.txt)
        log "  $pb -> $who"
    done
    exit 1
fi

log "ALL HOSTS CLEAN"
exit 0
