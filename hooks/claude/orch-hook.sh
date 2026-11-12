#!/bin/sh
# Claude Code hook entry for `orch hook <event>` (pretool, stop, session-start),
# registered by install_settings.py from settings-fragment.json.
#
# Starting orch costs about 200 ms, and these hooks fire in every Claude
# session, so this gate lets through only orchestrate sessions: a pane started
# by orch (ORCH_ROLE) or a pane with the role marker `orch become` writes,
# whatever multiplexer it sits in. `orch hook` makes the exact decision; the
# gate only has to admit every session it could act on.
event="${1-}"
case "${ORCH_ROLE-}" in
lead)
  # A lead has no pretool rule.
  [ "$event" = pretool ] && exit 0
  ;;
"")
  roles="${ORCH_ROLES_DIR:-$HOME/.local/state/orchestrate/roles}"
  case "$roles" in "~/"*) roles="$HOME/${roles#"~/"}" ;; esac
  found=
  for id in "${HERDR_PANE_ID-}" "${ORCA_PANE_KEY-}" "${ORCA_TERMINAL_HANDLE-}" "${ORCA_AGENT_SESSION_ID-}"; do
    if [ -n "$id" ] && [ -e "$roles/$id" ]; then
      found=1
      break
    fi
  done
  [ -n "$found" ] || exit 0
  ;;
esac
exec "$(dirname "$0")/../../scripts/orch" hook "$event"
