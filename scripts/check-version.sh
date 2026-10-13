#!/bin/sh
# Release consistency: the marketplace installs the plugin from tag v<version>,
# so plugin.json version and the marketplace ref must agree (and match the tag
# being released, when one is given).
set -e
cd "$(dirname "$0")/.."
ver="$(jq -r .version .claude-plugin/plugin.json)"
ref="$(jq -r '.plugins[] | select(.name == "brojev") | .source.ref' .claude-plugin/marketplace.json)"
[ "$ref" = "v$ver" ] || { echo "marketplace ref $ref != v$ver (plugin.json)"; exit 1; }
if [ -n "$1" ] && [ "$1" != "v$ver" ]; then
  echo "tag $1 != v$ver (plugin.json)"; exit 1
fi
echo "version v$ver consistent"
