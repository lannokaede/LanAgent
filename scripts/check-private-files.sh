#!/usr/bin/env bash
# Inspect index paths only. Do not open credentials, databases, or user documents.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

git rev-parse --show-toplevel >/dev/null
check_index_paths="$(mktemp "${TMPDIR:-/tmp}/lanagent-index-paths.XXXXXX")"
trap 'rm -f -- "$check_index_paths"' EXIT
git ls-files --cached -z > "$check_index_paths"

check_private_found=false
while IFS= read -r -d '' check_path; do
  check_reason=''
  check_name="${check_path##*/}"
  case "/$check_path" in
    /out/*) check_reason='private resumes and local notes' ;;
    */.runtime/*|*/.runtime|*/.lanagent/state/*|*/.lanagent/state) check_reason='private runtime data' ;;
  esac
  if [[ -z "$check_reason" ]]; then
    case "$check_name" in
      .env.example) ;;
      .env|.env.*|*.local.json|*.local.yaml|*.local.yml)
        check_reason='private environment or local configuration' ;;
      *.db|*.db-wal|*.db-shm|*.sqlite|*.sqlite-wal|*.sqlite-shm|*.sqlite3|*.sqlite3-wal|*.sqlite3-shm)
        # Fixed database fixtures may be versioned under an explicit fixture
        # directory. Private directory/configuration rules above still apply.
        case "/$check_path" in
          */testdata/*|*/fixtures/*) ;;
          *) check_reason='local database or SQLite sidecar' ;;
        esac
        ;;
    esac
  fi
  if [[ -n "$check_reason" ]]; then
    printf 'Refusing tracked private path: %q (%s)\n' "$check_path" "$check_reason" >&2
    check_private_found=true
  fi
done < "$check_index_paths"

if [[ "$check_private_found" == true ]]; then
  printf 'Remove private paths from the Git index. Use explicit .example names for configuration samples.\n' >&2
  exit 1
fi
printf 'Tracked private-file paths: none\n'
