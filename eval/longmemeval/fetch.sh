#!/bin/sh
# Downloads the cleaned LongMemEval release (2025/09) into data/.
set -eu
cd "$(dirname "$0")"
mkdir -p data
base=https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main
for f in longmemeval_s_cleaned.json longmemeval_oracle.json; do
  [ -f "data/$f" ] || curl -fL --progress-bar -o "data/$f" "$base/$f"
done
