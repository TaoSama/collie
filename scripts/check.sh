#!/bin/sh
# Run the standard repository checks from any directory.
set -eu
cd "$(dirname "$0")/.."

go build ./...
go vet ./...
go test ./... -count=1

for source in web/*.js; do
  node --check "$source"
done
node --test scripts/test/*.test.mjs
