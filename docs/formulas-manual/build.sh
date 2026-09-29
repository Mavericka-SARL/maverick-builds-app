#!/usr/bin/env bash
# Assemble parts/*.html into manual.html and render mavericks-formulas-manual.pdf with headless Chrome.
# Usage: bash docs/formulas-manual/build.sh
set -euo pipefail
cd "$(dirname "$0")"
cat parts/*.html > manual.html
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[ -x "$CHROME" ] || CHROME="$(command -v google-chrome || command -v chromium || true)"
[ -x "$CHROME" ] || { echo "Chrome/Chromium not found; set CHROME=/path/to/chrome" >&2; exit 1; }
"$CHROME" --headless=new --disable-gpu --no-pdf-header-footer \
  --print-to-pdf="$PWD/mavericks-formulas-manual.pdf" "file://$PWD/manual.html" 2>/dev/null
ls -la mavericks-formulas-manual.pdf
