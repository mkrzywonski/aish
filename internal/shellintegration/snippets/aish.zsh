# aish shell integration: emit OSC 133 semantic-prompt marks and OSC 7 cwd
# reports so the wrapping aish session can frame command output precisely.
if [[ -n $AISH_SESSION && -z $AISH_INTEGRATED ]]; then
  export AISH_INTEGRATED=1

  # The user's rc may have prepended PATH entries; keep the aish ssh shim first.
  if [[ -n $AISH_SHIM_BIN && ":$PATH:" != *":$AISH_SHIM_BIN:"* ]]; then
    PATH="$AISH_SHIM_BIN:$PATH"
  fi

  __aish_report_cwd() {
    printf '\033]7;file://%s%s\033\\' "${HOST:-$(hostname)}" "$PWD"
  }

  __aish_precmd() {
    local st=$?
    if [[ -n $__aish_cmd_open ]]; then
      printf '\033]133;D;%s\033\\' "$st"
      __aish_cmd_open=
    fi
    __aish_report_cwd
    printf '\033]133;A\033\\'
    __aish_badge
  }

  __aish_preexec() {
    __aish_cmd_open=1
    printf '\033]133;C\033\\'
  }

  autoload -Uz add-zsh-hook
  add-zsh-hook precmd __aish_precmd
  add-zsh-hook preexec __aish_preexec

  # Visual badge: this prompt is shared with an AI, labeled with the session
  # name (mutable, $AISH_DIR/name) or id. Re-checked every prompt so renames
  # show up; PS1 is rebuilt from the captured base only when the label
  # changes. Inserted after a leading newline so the badge sits on the
  # prompt line itself.
  __aish_ps1_base=$PS1
  __aish_badge() {
    local label=
    [[ -n $AISH_DIR && -r $AISH_DIR/name ]] && IFS= read -r label < "$AISH_DIR/name"
    [[ -n $label ]] || label=$AISH_SESSION
    [[ $label == "$__aish_label" ]] && return 0
    __aish_label=$label
    if [[ $__aish_ps1_base == $'\n'* ]]; then
      PS1=$'\n'"%F{magenta}⧉$__aish_label%f ${__aish_ps1_base#$'\n'}"
    else
      PS1="%F{magenta}⧉$__aish_label%f $__aish_ps1_base"
    fi
  }
  __aish_badge
fi
