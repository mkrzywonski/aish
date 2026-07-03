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
    __aish_badge
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

  # Visual badge: this prompt is shared with an AI, labeled with the session
  # name (mutable, $AISH_DIR/name) or id. Re-checked every prompt so renames
  # show up; PS1 is rebuilt from the captured base only when the label
  # changes. Inserted after a leading \n (NixOS-style prompts) so the badge
  # sits on the prompt line itself.
  __aish_ps1_base=$PS1
  __aish_badge() {
    local label=
    [ -n "$AISH_DIR" ] && [ -r "$AISH_DIR/name" ] && IFS= read -r label < "$AISH_DIR/name"
    [ -n "$label" ] || label=$AISH_SESSION
    [ "$label" = "$__aish_label" ] && return 0
    __aish_label=$label
    case "$__aish_ps1_base" in
      "\n"*) PS1="\n\[\033[35m\]⧉$__aish_label\[\033[0m\] ${__aish_ps1_base#\\n}" ;;
      *)     PS1="\[\033[35m\]⧉$__aish_label\[\033[0m\] $__aish_ps1_base" ;;
    esac
  }
  __aish_badge
fi
