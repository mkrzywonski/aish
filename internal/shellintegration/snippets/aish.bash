# aish shell integration: emit OSC 133 semantic-prompt marks and OSC 7 cwd
# reports so the wrapping aish session can frame command output precisely.
# Safe to source in any bash >= 4; does nothing outside an aish session.
if [ -n "$AISH_SESSION" ] && [ -z "$AISH_INTEGRATED" ]; then
  AISH_INTEGRATED=1
  export AISH_INTEGRATED

  # The user's rc may have prepended PATH entries; keep the aish ssh shim first.
  if [ -n "$AISH_SHIM_BIN" ]; then
    case ":$PATH:" in
      ":$AISH_SHIM_BIN:"*) ;;
      *) PATH="$AISH_SHIM_BIN:$PATH" ;;
    esac
  fi

  __aish_report_cwd() {
    printf '\033]7;file://%s%s\033\\' "${HOSTNAME:-$(hostname)}" "$PWD"
  }

  __aish_prompt() {
    local st=$?
    if [ -n "$__aish_cmd_open" ]; then
      printf '\033]133;D;%s\033\\' "$st"
      __aish_cmd_open=
    fi
    __aish_report_cwd
    printf '\033]133;A\033\\'
    __aish_at_prompt=1
    return $st
  }

  __aish_preexec() {
    [ -n "$COMP_LINE" ] && return 0
    [ -z "$__aish_at_prompt" ] && return 0
    __aish_at_prompt=
    __aish_cmd_open=1
    printf '\033]133;C\033\\'
    return 0
  }

  PROMPT_COMMAND='__aish_prompt'${PROMPT_COMMAND:+";$PROMPT_COMMAND"}
  trap '__aish_preexec' DEBUG
fi
