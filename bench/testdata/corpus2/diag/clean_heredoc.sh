#!/bin/bash
cat <<EOT
text with $HOME and `date`
EOT
cat <<'RAW'
no $expansion here
RAW
f() {
  cat <<-IND
	indented
	IND
}
f
