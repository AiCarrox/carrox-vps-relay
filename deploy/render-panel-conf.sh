#!/usr/bin/env bash
# Render a panel vhost template. Placeholders (same ones panel.go fills in):
#   {{.Domain}}       panel domain
#   {{.HTTPSListen}}  listen directive(s) for the TLS server block
set -euo pipefail

DOMAIN="${1:?usage: render-panel-conf.sh <domain> <template> [https-listen]}"
TEMPLATE="${2:?usage: render-panel-conf.sh <domain> <template> [https-listen]}"
HTTPS_LISTEN="${3:-listen 443 ssl;
    listen [::]:443 ssl;}"

tmpl=$(<"$TEMPLATE")
tmpl=${tmpl//'{{.Domain}}'/"$DOMAIN"}
tmpl=${tmpl//'{{.HTTPSListen}}'/"$HTTPS_LISTEN"}
printf '%s\n' "$tmpl"
