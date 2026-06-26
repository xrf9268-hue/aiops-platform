#!/usr/bin/env bash
set -euo pipefail

run_root="${AIOPS_GHMR_RUN_ROOT:-}"
release_repo="${AIOPS_GHMR_RELEASE_REPO:-xrf9268-hue/aiops-platform}"
tag="${AIOPS_GHMR_RELEASE_TAG:-latest}"
maker_workflow="${AIOPS_GHMR_MAKER_WORKFLOW:-}"
reviewer_workflow="${AIOPS_GHMR_REVIEWER_WORKFLOW:-}"

usage() {
  printf 'usage: %s --run-root DIR [--release-repo OWNER/NAME] [--tag latest|vX.Y.Z]\n' "$0" >&2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-root)
      run_root="${2:-}"; shift 2 ;;
    --release-repo)
      release_repo="${2:-}"; shift 2 ;;
    --tag)
      tag="${2:-}"; shift 2 ;;
    --maker-workflow)
      maker_workflow="${2:-}"; shift 2 ;;
    --reviewer-workflow)
      reviewer_workflow="${2:-}"; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      usage; exit 2 ;;
  esac
done

if [ -z "$run_root" ]; then
  printf -- '--run-root is required\n' >&2
  usage
  exit 2
fi

downloads="$run_root/downloads"
artifacts="$run_root/artifacts"
logs="$run_root/logs"
bin_dir="$run_root/bin"
mkdir -p "$downloads" "$artifacts" "$logs" "$bin_dir" "$run_root/state"

case "$(uname -s)" in
  Darwin) os_name="darwin" ;;
  Linux) os_name="linux" ;;
  *) printf 'unsupported OS: %s\n' "$(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch_name="arm64" ;;
  x86_64|amd64) arch_name="amd64" ;;
  *) printf 'unsupported arch: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

if [ "$tag" = "latest" ]; then
  tag="$(gh release view --repo "$release_repo" --json tagName --jq .tagName)"
fi

gh release view "$tag" --repo "$release_repo" --json tagName,publishedAt,url,assets \
  > "$artifacts/release-view-summary.json"

asset="aiops-platform_${tag}_${os_name}_${arch_name}.tar.gz"
sums="aiops-platform_${tag}_SHA256SUMS"
sbom="aiops-platform_${tag}_sbom.cdx.json"

rm -f "$downloads/$asset" "$downloads/$sums" "$downloads/$sbom"
gh release download "$tag" --repo "$release_repo" --dir "$downloads" --pattern "$asset"
gh release download "$tag" --repo "$release_repo" --dir "$downloads" --pattern "$sums"
gh release download "$tag" --repo "$release_repo" --dir "$downloads" --pattern "$sbom"

(
  cd "$downloads"
  awk -v file="$asset" '$2 == file { print }' "$sums" | shasum -a 256 -c -
) | tee "$artifacts/sha256.log"

gh attestation verify "$downloads/$asset" --repo "$release_repo" \
  | tee "$artifacts/attestation.log"

python3 - "$downloads/$sbom" "$artifacts/sbom-summary.json" <<'PY'
import json
import sys
from pathlib import Path

src = Path(sys.argv[1])
dest = Path(sys.argv[2])
data = json.loads(src.read_text())
summary = {
    "bomFormat": data.get("bomFormat"),
    "specVersion": data.get("specVersion"),
    "serialNumber": data.get("serialNumber"),
    "component_count": len(data.get("components") or []),
}
dest.write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n")
PY

extract_dir="$downloads/${asset%.tar.gz}"
rm -rf "$extract_dir"
tar -xzf "$downloads/$asset" -C "$downloads"
cp "$extract_dir/worker" "$bin_dir/worker"
cp "$extract_dir/tui" "$bin_dir/tui"
chmod +x "$bin_dir/worker" "$bin_dir/tui"

"$bin_dir/worker" --version | tee "$artifacts/worker-version.log"
"$bin_dir/tui" --version | tee "$artifacts/tui-version.log"
codex --version | tee "$artifacts/codex-version.log"
gh --version | tee "$artifacts/gh-version.log"

role_log="$artifacts/github-role-auth-preflight.log"
: >"$role_log"
check_role() {
  local label="$1"
  local dir="$2"
  local expected="$3"
  if [ -z "$dir" ]; then
    return
  fi
  local login
  login="$(GH_CONFIG_DIR="$dir" gh api user --jq .login)"
  printf '%s=%s\n' "$label" "$login" | tee -a "$role_log"
  if [ -n "$expected" ] && [ "$expected" != "REPLACE_ME_MAKER_LOGIN" ] && [ "$expected" != "REPLACE_ME_REVIEWER_LOGIN" ] && [ "$login" != "$expected" ]; then
    printf '%s login %s does not match expected %s\n' "$label" "$login" "$expected" >&2
    exit 1
  fi
}

check_role "setup" "${AIOPS_GHMR_SETUP_GH_CONFIG_DIR:-}" ""
check_role "maker" "${AIOPS_GHMR_MAKER_GH_CONFIG_DIR:-}" "${AIOPS_GHMR_MAKER_LOGIN:-}"
check_role "reviewer" "${AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR:-}" "${AIOPS_GHMR_REVIEWER_LOGIN:-}"

if [ -n "${AIOPS_GHMR_REPO:-}" ] && [ -n "${AIOPS_GHMR_MAKER_GH_CONFIG_DIR:-}" ]; then
  dry_run_dir="$(mktemp -d "$run_root/state/maker-push-dry-run.XXXXXX")"
  trap 'rm -rf "$dry_run_dir"' EXIT
  GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" gh repo clone "$AIOPS_GHMR_REPO" "$dry_run_dir/repo" -- --depth 1 \
    > "$artifacts/maker-git-clone-dry-run.log" 2>&1
  git -C "$dry_run_dir/repo" checkout -b "aiops-preflight-dry-run" \
    >> "$artifacts/maker-git-push-dry-run.log" 2>&1
  git -C "$dry_run_dir/repo" \
    -c user.name=aiops-preflight \
    -c user.email=aiops-preflight@example.invalid \
    commit --allow-empty -m "chore: aiops preflight dry run" \
    >> "$artifacts/maker-git-push-dry-run.log" 2>&1
  GH_CONFIG_DIR="$AIOPS_GHMR_MAKER_GH_CONFIG_DIR" \
    git -C "$dry_run_dir/repo" push --dry-run origin "HEAD:refs/heads/aiops-preflight-dry-run-$(date +%s)" \
    2>&1 | tee -a "$artifacts/maker-git-push-dry-run.log"
  rm -rf "$dry_run_dir"
  trap - EXIT
fi

if [ -z "$maker_workflow" ] && [ -f "$run_root/workflows/github-maker-WORKFLOW.md" ]; then
  maker_workflow="$run_root/workflows/github-maker-WORKFLOW.md"
fi
if [ -z "$reviewer_workflow" ] && [ -f "$run_root/workflows/github-reviewer-automerge-WORKFLOW.md" ]; then
  reviewer_workflow="$run_root/workflows/github-reviewer-automerge-WORKFLOW.md"
fi

if [ -n "$maker_workflow" ]; then
  "$bin_dir/worker" --doctor --deploy=binary --mode=real "$maker_workflow" \
    | tee "$logs/maker-doctor.log"
fi
if [ -n "$reviewer_workflow" ]; then
  "$bin_dir/worker" --doctor --deploy=binary --mode=real "$reviewer_workflow" \
    | tee "$logs/reviewer-doctor.log"
fi

printf 'release preflight complete for %s (%s/%s)\n' "$tag" "$os_name" "$arch_name"
