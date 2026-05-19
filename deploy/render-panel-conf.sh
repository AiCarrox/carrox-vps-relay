#!/usr/bin/env bash
set -euo pipefail

DOMAIN="${1:?usage: render-panel-conf.sh <domain>}"
TEMPLATE="${2:?usage: render-panel-conf.sh <domain> <template>}"

sed "s/{{.Domain}}/${DOMAIN//\//\\/}/g" "$TEMPLATE"
