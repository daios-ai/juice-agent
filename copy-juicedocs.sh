#!/usr/bin/env bash
# Copy the kernel docs from ../juice into ./juicedocs.
set -euo pipefail

src="../juice"
dest="juicedocs"

mkdir -p "$dest"

for f in requirements.md API.md ecosystem-standard.md; do
    cp "$src/$f" "$dest/$f"
    echo "copied $src/$f -> $dest/$f"
done

# The shared juice-family logo, used in the README.
cp "$src/docs/juice-logo.svg" ./juice-logo.svg
echo "copied $src/docs/juice-logo.svg -> ./juice-logo.svg"
