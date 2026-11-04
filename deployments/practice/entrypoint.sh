#!/bin/sh
# Fixed trusted bootstrap; user code is read only by the Go compiler.
set -eu
cp -r /opt/go-cache /tmp/go-cache
exec "$@"
