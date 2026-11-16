#!/usr/bin/env bash
# Local and CI checks share this entry; checks never rewrite source or modules.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

usage() {
  printf 'Usage: ./scripts/check.sh [--quick|--help]\n'
  printf 'Default: private paths, formatting, module drift, tests, race, vet, command builds.\n'
  printf '  --quick  Skip race for a local iteration; CI always runs the default.\n'
}

if (( $# > 1 )); then
  usage >&2
  exit 2
fi
check_race=true
case "${1:-}" in
  '') ;;
  --quick) check_race=false ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

printf 'Checking tracked private-file paths\n'
./scripts/check-private-files.sh

printf 'Checking Go formatting\n'
# Include new source files before git add, while ignoring private runtime data.
# A removed tracked file is still listed by git until staging, so check it exists.
check_go_files=()
while IFS= read -r -d '' check_path; do
  if [[ -f "$check_path" ]]; then
    check_go_files+=("$check_path")
  fi
done < <(git ls-files --cached --others --exclude-standard -z -- '*.go')
if (( ${#check_go_files[@]} == 0 )); then
  printf 'No repository Go source files found\n' >&2
  exit 1
fi
check_unformatted="$(gofmt -l "${check_go_files[@]}")"
if [[ -n "$check_unformatted" ]]; then
  printf '%s\n' "$check_unformatted" >&2
  printf 'Run gofmt on the listed files before committing.\n' >&2
  exit 1
fi

printf 'Checking go.mod and go.sum drift\n'
go mod tidy -diff

printf 'Running tests\n'
go test -mod=readonly ./...
if [[ "$check_race" == true ]]; then
  printf 'Running race checks\n'
  go test -mod=readonly -race ./...
fi

printf 'Running go vet\n'
go vet -mod=readonly ./...

printf 'Building commands\n'
check_build_dir="$(mktemp -d "${TMPDIR:-/tmp}/lanagent-check.XXXXXX")"
trap 'rm -rf -- "$check_build_dir"' EXIT
go build -mod=readonly -trimpath -o "$check_build_dir/" ./cmd/...
printf 'Checks passed\n'
