#!/bin/sh

SEMVER="$1"

IFS=. read SEMVER_MAJOR SEMVER_MINOR SEMVER_PATCH <<EOF
${SEMVER##}
EOF
echo "$SEMVER_MAJOR.$SEMVER_MINOR-$SEMVER_PATCH"
