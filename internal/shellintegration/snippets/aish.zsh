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
  }

  __aish_preexec() {
    __aish_cmd_open=1
    printf '\033]133;C\033\\'
  }

  autoload -Uz add-zsh-hook
  add-zsh-hook precmd __aish_precmd
  add-zsh-hook preexec __aish_preexec
fi
