#!/usr/bin/env bash
#
# Fetch SonarCloud analysis for the current project.
#
# Usage:
#   ./sonar.sh              # analyse default branch (from .env SONAR_BRANCH)
#   ./sonar.sh pr 23        # analyse pull request #23
#   ./sonar.sh branch main  # analyse a specific branch
#
# Requires: curl, jq
# Config:   .env with SONAR_TOKEN, SONAR_PROJECT_KEY, SONAR_HOST (optional)

set -euo pipefail

# ── Config ──────────────────────────────────────────────────────────────────

if [[ ! -f .env ]]; then
  echo "ERROR: .env not found. Create one with SONAR_TOKEN and SONAR_PROJECT_KEY." >&2
  exit 1
fi
set -a; source .env; set +a

: "${SONAR_TOKEN:?Set SONAR_TOKEN in .env}"
: "${SONAR_PROJECT_KEY:?Set SONAR_PROJECT_KEY in .env}"
SONAR_HOST="${SONAR_HOST:-https://sonarcloud.io}"
TOKEN="$SONAR_TOKEN"
PROJECT="$SONAR_PROJECT_KEY"

# ── Parse arguments ─────────────────────────────────────────────────────────

MODE="branch"
TARGET="${SONAR_BRANCH:-}"

if [[ $# -ge 2 ]]; then
  MODE="$1"
  TARGET="$2"
elif [[ $# -eq 1 ]]; then
  # bare number → PR
  if [[ "$1" =~ ^[0-9]+$ ]]; then
    MODE="pr"
    TARGET="$1"
  else
    MODE="branch"
    TARGET="$1"
  fi
fi

case "$MODE" in
  pr|pullRequest|pull_request)
    SCOPE_PARAM="pullRequest=$TARGET"
    SCOPE_LABEL="PR #$TARGET"
    ;;
  branch|"")
    if [[ -z "$TARGET" ]]; then
      echo "ERROR: no branch specified. Set SONAR_BRANCH in .env or pass a branch name." >&2
      echo "Usage: $0 [pr <number> | branch <name>]" >&2
      exit 1
    fi
    ENCODED_TARGET=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$TARGET', safe=''))" 2>/dev/null || echo "$TARGET")
    SCOPE_PARAM="branch=$ENCODED_TARGET"
    SCOPE_LABEL="branch=$TARGET"
    ;;
  *)
    echo "Usage: $0 [pr <number> | branch <name>]" >&2
    exit 1
    ;;
esac

# ── Helpers ─────────────────────────────────────────────────────────────────

sonar_api() {
  local endpoint="$1"
  local params="${2:-}"
  local auth="${3:-basic}"   # "basic" (default) or "bearer"
  local url="$SONAR_HOST/api/$endpoint"
  [[ -n "$params" ]] && url="$url?$params"

  local http_code body auth_flag
  if [[ "$auth" == "bearer" ]]; then
    auth_flag=(-H "Authorization: Bearer $TOKEN")
  else
    auth_flag=(-u "$TOKEN:")
  fi

  local tmpfile
  tmpfile=$(mktemp)
  http_code=$(curl -sS -o "$tmpfile" -w '%{http_code}' "${auth_flag[@]}" "$url")
  body=$(<"$tmpfile")
  rm -f "$tmpfile"

  if [[ "$http_code" -ge 400 ]]; then
    echo "ERROR: $endpoint returned HTTP $http_code" >&2
    local msg
    msg=$(echo "$body" | jq -r '.errors[]?.msg // empty' 2>/dev/null)
    [[ -n "$msg" ]] && echo "  $msg" >&2
    return 1
  fi

  echo "$body"
}

# ── Auth check ──────────────────────────────────────────────────────────────

echo "SonarCloud: $PROJECT ($SCOPE_LABEL)"
echo "---"

if ! sonar_api "authentication/validate" >/dev/null 2>&1; then
  echo "ERROR: authentication failed. Check SONAR_TOKEN in .env" >&2
  exit 1
fi
valid=$(sonar_api "authentication/validate" | jq -r '.valid')
if [[ "$valid" != "true" ]]; then
  echo "ERROR: token is invalid. Generate a new one at:" >&2
  echo "  https://sonarcloud.io/account/security" >&2
  exit 1
fi
echo "Auth: OK"

# ── Quality Gate ────────────────────────────────────────────────────────────

echo ""
echo "=== Quality Gate ==="

qg=$(sonar_api "qualitygates/project_status" "projectKey=$PROJECT&$SCOPE_PARAM")
status=$(echo "$qg" | jq -r '.projectStatus.status')
echo "Status: $status"

echo "$qg" | jq -r '
  .projectStatus.conditions[]? |
  select(.status != "OK") |
  "  FAIL: \(.metricKey) = \(.actualValue) (threshold: \(.errorThreshold))"
'

# ── Issues ──────────────────────────────────────────────────────────────────

echo ""
echo "=== Issues ==="

page=1
all_issues="[]"
while true; do
  resp=$(sonar_api "issues/search" "componentKeys=$PROJECT&$SCOPE_PARAM&ps=500&p=$page&resolved=false")
  total=$(echo "$resp" | jq '.total')
  batch=$(echo "$resp" | jq '.issues')
  count=$(echo "$batch" | jq 'length')

  all_issues=$(echo "$all_issues $batch" | jq -s '.[0] + .[1]')

  [[ $((page * 500)) -ge $total ]] && break
  page=$((page + 1))
done

n_issues=$(echo "$all_issues" | jq 'length')
echo "Total: $n_issues open issues"

if [[ "$n_issues" -gt 0 ]]; then
  # Summary by type/severity
  echo ""
  echo "$all_issues" | jq -r '
    sort_by(.type) | group_by(.type) | .[] |
    .[0].type as $type |
    (sort_by(.severity) | group_by(.severity) | map({key: .[0].severity, value: length}) | from_entries) as $sev |
    "  \($type): \(length) (\($sev | to_entries | map("\(.value) \(.key)") | join(", ")))"
  '

  # List each issue
  echo ""
  echo "--- Details ---"
  echo "$all_issues" | jq -r '
    sort_by(.component, .line) | .[] |
    "\(.severity) \(.type) [\(.rule)]" +
    "\n  " + (.component | sub(".*:"; "")) +
    (if .line then ":\(.line)" else "" end) +
    "\n  " + .message +
    "\n"
  '
fi

# ── Security Hotspots ──────────────────────────────────────────────────────

echo ""
echo "=== Security Hotspots ==="

page=1
all_hotspots="[]"
while true; do
  resp=$(sonar_api "hotspots/search" "projectKey=$PROJECT&$SCOPE_PARAM&ps=500&p=$page" bearer)
  total=$(echo "$resp" | jq '.paging.total')
  batch=$(echo "$resp" | jq '.hotspots')

  all_hotspots=$(echo "$all_hotspots $batch" | jq -s '.[0] + .[1]')

  [[ $((page * 500)) -ge $total ]] && break
  page=$((page + 1))
done

n_hotspots=$(echo "$all_hotspots" | jq 'length')
to_review=$(echo "$all_hotspots" | jq '[.[] | select(.status == "TO_REVIEW")] | length')
echo "Total: $n_hotspots ($to_review to review)"

if [[ "$n_hotspots" -gt 0 ]]; then
  echo ""
  echo "$all_hotspots" | jq -r '
    sort_by(.component, .line) | .[] |
    "\(.vulnerabilityProbability) \(.securityCategory) [\(.ruleKey)] \(.status)" +
    "\n  " + (.component | sub(".*:"; "")) +
    (if .line then ":\(.line)" else "" end) +
    "\n  " + .message +
    "\n"
  '
fi

# ── Duplications ────────────────────────────────────────────────────────────

echo "=== Duplications ==="

measures=$(sonar_api "measures/component" \
  "component=$PROJECT&$SCOPE_PARAM&metricKeys=new_duplicated_lines_density,new_duplicated_lines,new_duplicated_blocks,duplicated_lines_density")

echo "$measures" | jq -r '
  .component.measures[]? |
  if .period then
    "  \(.metric): \(.period.value) (new code)"
  else
    "  \(.metric): \(.value)"
  end
'

# Show files with duplicated blocks
dup_tree=$(sonar_api "measures/component_tree" \
  "component=$PROJECT&$SCOPE_PARAM&metricKeys=duplicated_blocks&metricSort=duplicated_blocks&s=metric&asc=false&ps=20&qualifiers=FIL")

n_dup_files=$(echo "$dup_tree" | jq '[.components[]? | select((.measures[]?.value // "0") != "0")] | length')

if [[ "$n_dup_files" -gt 0 ]]; then
  echo ""
  echo "--- Duplicated Files ---"
  echo "$dup_tree" | jq -r '
    [.components[]? | select((.measures[]?.value // "0") != "0")] |
    sort_by(-(.measures[0].value | tonumber)) | .[] |
    "  \(.measures[0].value) blocks  \(.key | sub(".*:"; ""))"
  '

  # Fetch duplication block details for each file
  echo ""
  echo "--- Duplication Blocks ---"
  echo "$dup_tree" | jq -r '
    [.components[]? | select((.measures[]?.value // "0") != "0")] | .[].key
  ' | while read -r comp_key; do
    dup_detail=$(sonar_api "duplications/show" "key=$comp_key")
    short_name=$(echo "$comp_key" | sed 's/.*://')
    echo "$dup_detail" | jq -r --arg file "$short_name" '
      .duplications[]? |
      [.blocks[] | "\(.from)-\(.from + .size - 1) in \(._ref // "1")"] |
      "  \($file): " + join(" ↔ ")
    '
  done
fi

# ── Write JSON report ──────────────────────────────────────────────────────

report="sonar-report.json"
tmpdir=$(mktemp -d)
echo "$qg" > "$tmpdir/qg.json"
echo "$all_issues" > "$tmpdir/issues.json"
echo "$all_hotspots" > "$tmpdir/hotspots.json"
echo "$measures" > "$tmpdir/measures.json"

jq -n \
  --arg project "$PROJECT" \
  --arg scope "$SCOPE_LABEL" \
  --arg status "$status" \
  --slurpfile qg "$tmpdir/qg.json" \
  --slurpfile issues "$tmpdir/issues.json" \
  --slurpfile hotspots "$tmpdir/hotspots.json" \
  --slurpfile measures "$tmpdir/measures.json" \
  '{
    project: $project,
    scope: $scope,
    fetched_at: (now | todate),
    quality_gate: { status: $status, conditions: $qg[0].projectStatus.conditions },
    issues_count: ($issues[0] | length),
    issues: $issues[0],
    hotspots_count: ($hotspots[0] | length),
    hotspots: $hotspots[0],
    measures: $measures[0].component.measures
  }' > "$report"

rm -rf "$tmpdir"

echo ""
echo "---"
echo "Report: $report"
