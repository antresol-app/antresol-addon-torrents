#!/bin/sh
# corecopy.sh [--check] [CORE]: copy the core helper files this addon builds
# on into internal/corecopy/, from the core checkout CORE (default
# $ANTRESOL_CORE, else the core repository this module sits in: git's top
# level when it holds addonkit/; a relative CORE is taken from the module root).
#
# tools/corecopy.list names the files: globs relative to CORE/internal,
# whitespace-separated, # comments. Test files are never copied, nor the
# files only the torrent family named in the core (events/types_full.go,
# library/torrent_full.go: the addon has its own constants): a glob passes
# over them, naming one is refused. Each copy:
#   - starts with "// Code copied from the core by tools/corecopy.sh. DO NOT EDIT."
#   - loses a leading "//go:build sources" line (the copy builds untagged);
#     any other build line naming sources is refused (choose by hand)
#   - imports github.com/antresol-app/antresol-addon-torrents/internal/corecopy/X
#     for the core's github.com/antresol-app/antresol/internal/X
# A copied file importing a core package that is not copied is refused
# (closure), as is a glob matching nothing; nothing is written then.
#
# --check writes nothing: it copies into a temp dir, compares that with
# internal/corecopy/ and lists the drifted files. Exit 1 on drift or refusal.
set -eu

MOD=$(cd "$(dirname "$0")/.." && pwd)
CHECK=0
if [ "${1:-}" = "--check" ]; then
	CHECK=1
	shift
fi
CORE=${1:-${ANTRESOL_CORE:-}}
if [ -z "$CORE" ]; then
	top=$(git -C "$MOD" rev-parse --show-toplevel 2>/dev/null || true)
	if [ -n "$top" ] && [ -f "$top/addonkit/go.mod" ]; then
		CORE=$top
	else
		echo "corecopy: no core checkout: name one (CORE or ANTRESOL_CORE)" >&2
		exit 1
	fi
fi
case $CORE in
/*) ;;
*) CORE=$MOD/$CORE ;;
esac
if [ ! -d "$CORE/internal" ]; then
	echo "corecopy: no core checkout at $CORE (no internal/)" >&2
	exit 1
fi
CORE=$(cd "$CORE" && pwd)
LIST=$MOD/tools/corecopy.list
DEST=$MOD/internal/corecopy
OLD=github.com/antresol-app/antresol/internal/
NEW=github.com/antresol-app/antresol-addon-torrents/internal/corecopy/
HEADER='// Code copied from the core by tools/corecopy.sh. DO NOT EDIT.'

family_only() {
	case $1 in
	events/types_full.go | library/torrent_full.go) return 0 ;;
	esac
	return 1
}

TMP=$(mktemp -d "${TMPDIR:-/tmp}/corecopy.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM
: >"$TMP/errors"
: >"$TMP/files"

# the files: every glob of the list, expanded in CORE/internal
cd "$CORE/internal"
set -f
for pat in $(sed 's/#.*//' "$LIST"); do
	set +f
	n=0
	for f in $pat; do
		[ -f "$f" ] || continue
		case $f in
		*_test.go) continue ;;
		*.go) ;;
		*) continue ;;
		esac
		# a glob passes over the family-only files; naming one is refused below
		if [ "$f" != "$pat" ] && family_only "$f"; then
			continue
		fi
		n=$((n + 1))
		echo "$f" >>"$TMP/files"
	done
	[ "$n" -gt 0 ] || echo "$pat: matches no Go file in $CORE/internal" >>"$TMP/errors"
	set -f
done
set +f
sort -u "$TMP/files" -o "$TMP/files"
sed 's#/[^/]*$##' "$TMP/files" | sort -u >"$TMP/packages"

while IFS= read -r f; do
	if family_only "$f"; then
		echo "$f: only the torrent family used it in the core; never copied (the addon has its own)" >>"$TMP/errors"
		continue
	fi
	# build lines: a leading //go:build sources goes, any other naming sources is refused
	drop=0
	if [ "$(sed -n '1p' "$f")" = "//go:build sources" ]; then
		drop=1
	fi
	grep -n '^//go:build.*sources' "$f" | while IFS= read -r l; do
		if [ "$drop" -eq 0 ] || [ "${l%%:*}" != 1 ]; then
			echo "$f: build line \"${l#*:}\" (only a leading //go:build sources is dropped; choose the variant by hand)" >>"$TMP/errors"
		fi
	done
	# closure: every core package it imports is copied too
	grep -o '"github\.com/antresol-app/antresol/[^"]*"' "$f" | tr -d '"' | sort -u | while IFS= read -r imp; do
		case $imp in
		github.com/antresol-app/antresol/addonkit | github.com/antresol-app/antresol/addonkit/*) ;;
		"$OLD"*)
			grep -qx "${imp#"$OLD"}" "$TMP/packages" ||
				echo "$f: imports internal/${imp#"$OLD"}, which is not copied (add it to tools/corecopy.list)" >>"$TMP/errors"
			;;
		*) echo "$f: imports $imp, outside internal/ (not copied)" >>"$TMP/errors" ;;
		esac
	done
	mkdir -p "$TMP/out/$(dirname "$f")"
	{
		echo "$HEADER"
		echo
		awk -v drop="$drop" 'drop && NR == 1 { next } drop && NR == 2 && $0 == "" { next } { print }' "$f" |
			sed "s#\"$OLD#\"$NEW#g"
	} >"$TMP/out/$f"
done <"$TMP/files"

if [ -s "$TMP/errors" ]; then
	echo "corecopy: refused:" >&2
	sed 's/^/  /' "$TMP/errors" >&2
	exit 1
fi
mkdir -p "$TMP/out"

if [ "$CHECK" -eq 0 ]; then
	mkdir -p "$MOD/internal"
	rm -rf "$DEST"
	cp -R "$TMP/out" "$DEST"
	echo "corecopy: $(wc -l <"$TMP/files" | tr -d ' ') files from $CORE into internal/corecopy/"
	exit 0
fi

# --check: the fresh copy against internal/corecopy/
(cd "$TMP/out" && find . -type f | sed 's#^\./##' | sort) >"$TMP/want"
if [ -d "$DEST" ]; then
	(cd "$DEST" && find . -type f | sed 's#^\./##' | sort) >"$TMP/have"
else
	: >"$TMP/have"
fi
: >"$TMP/drift"
comm -23 "$TMP/want" "$TMP/have" | sed 's/$/ (missing from internal\/corecopy)/' >>"$TMP/drift"
comm -13 "$TMP/want" "$TMP/have" | sed 's/$/ (not in the list or the core any more)/' >>"$TMP/drift"
comm -12 "$TMP/want" "$TMP/have" | while IFS= read -r f; do
	cmp -s "$TMP/out/$f" "$DEST/$f" || echo "$f (differs from the core)" >>"$TMP/drift"
done
if [ -s "$TMP/drift" ]; then
	echo "corecopy: internal/corecopy/ drifted from $CORE (run make corecopy):" >&2
	sed 's/^/  /' "$TMP/drift" >&2
	exit 1
fi
echo "corecopy: internal/corecopy/ matches $CORE"
